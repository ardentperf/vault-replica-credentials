// Package controller implements the bounded, event-driven replica credential
// workflow. It never reads target Secret data and never talks to PostgreSQL.
package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	kubernetes "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	appmetrics "github.com/ardentperf/vault-replica-credentials/internal/metrics"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

const (
	podObservationRequeue = 15 * time.Second
	transientRequeue      = 15 * time.Second
)

// Reconciler dependencies are all true external boundaries. The password map
// intentionally remains process-memory-only and is erased after the Secret
// patch succeeds.
type Reconciler struct {
	client      client.Client
	apiReader   client.Reader
	state       *state.Repository
	vault       vault.Client
	status      kubernetes.PodStatusReader
	config      config.Config
	metrics     *appmetrics.Metrics
	now         func() time.Time
	credentials credentialCache
}

type credentialCache struct {
	mu     sync.Mutex
	values map[string]cachedCredential
}

type cachedCredential struct {
	leaseID  string
	password string
}

type Dependencies struct {
	Client    client.Client
	APIReader client.Reader
	State     *state.Repository
	Vault     vault.Client
	Status    kubernetes.PodStatusReader
	Metrics   *appmetrics.Metrics
	Now       func() time.Time
}

func NewReconciler(cfg config.Config, dependencies Dependencies) (*Reconciler, error) {
	if dependencies.Client == nil || dependencies.State == nil || dependencies.Vault == nil || dependencies.Status == nil {
		return nil, errors.New("reconciler requires Kubernetes client, state repository, Vault client, and pod status reader")
	}
	if dependencies.APIReader == nil {
		dependencies.APIReader = dependencies.Client
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	return &Reconciler{
		client:    dependencies.Client,
		apiReader: dependencies.APIReader,
		state:     dependencies.State,
		vault:     dependencies.Vault,
		status:    dependencies.Status,
		config:    cfg,
		metrics:   dependencies.Metrics,
		now:       dependencies.Now,
		credentials: credentialCache{
			values: map[string]cachedCredential{},
		},
	}, nil
}

// Reconcile processes one Cluster queue key. All waits are persisted and
// returned as timed requeues; no controller worker sleeps.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	key := request.Namespace + "/" + request.Name
	cluster := cnpg.NewClusterObject()
	if err := r.client.Get(ctx, request.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// Deletion needs two fresh sweep confirmations, not one watch event.
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("get Cluster: %w", err)
	}
	info, err := cnpg.Inspect(cluster)
	if err != nil {
		return reconcile.Result{}, err
	}
	store, err := r.state.Load(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(store)
	entry, known := store.Clusters[key]
	if known && entry.ClusterUID != info.UID {
		if err := r.cleanupEntry(ctx, key, entry); err != nil {
			// Stale lease cleanup is independent of the replacement Cluster. Keep
			// the old state intact and retry on a bounded timer rather than relying
			// on the work queue's exponentially growing error delay. In particular,
			// a promoted source can be briefly read-only while its Vault database
			// connection is being moved to the new writable primary.
			return reconcile.Result{RequeueAfter: transientRequeue}, nil
		}
		return reconcile.Result{Requeue: true}, nil
	}
	if !info.Replica {
		if known {
			if err := r.cleanupEntry(ctx, key, entry); err != nil {
				// Promotion cleanup must keep retrying without mutating the
				// standalone Cluster. The entry is deleted only after every known
				// lease has been successfully revoked.
				return reconcile.Result{RequeueAfter: transientRequeue}, nil
			}
		}
		return reconcile.Result{}, nil
	}

	pod := &corev1.Pod{}
	if info.PrimaryPod == "" {
		return reconcile.Result{RequeueAfter: podObservationRequeue}, nil
	}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: info.Namespace, Name: info.PrimaryPod}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{RequeueAfter: podObservationRequeue}, nil
		}
		return reconcile.Result{}, fmt.Errorf("get designated primary Pod: %w", err)
	}
	trigger, ready := cnpg.TriggerForPod(info, pod)
	if !ready {
		return reconcile.Result{RequeueAfter: podObservationRequeue}, nil
	}
	if !known {
		entry = state.ClusterState{ClusterUID: info.UID}
	}
	if entry.Pending == nil && entry.LastEvent == trigger {
		return reconcile.Result{}, nil
	}
	if entry.Pending == nil {
		return r.issueForInfo(ctx, key, entry, trigger, info)
	}
	return r.resume(ctx, key, entry, info)
}

func (r *Reconciler) resume(ctx context.Context, key string, entry state.ClusterState, info cnpg.ClusterInfo) (reconcile.Result, error) {
	pending := entry.Pending
	if pending == nil {
		return reconcile.Result{}, errors.New("resume called without pending rotation")
	}
	now := r.now().UTC()
	if pending.Stage == state.StageReplacementBackoff || pending.ExpiresAt.Add(-r.config.Vault.SafetyMargin).Before(now) {
		return r.replacePending(ctx, key, entry)
	}
	switch pending.Stage {
	case state.StageIssued:
		if !pending.StageDeadline.After(now) {
			return r.markReplacement(ctx, key, entry)
		}
		credential, ok := r.credentials.get(key, pending.LeaseID)
		if !ok {
			// Passwords intentionally do not survive restart. Revoking first keeps
			// an otherwise unpatchable lease from becoming an orphan.
			return r.replacePending(ctx, key, entry)
		}
		return r.patchPassword(ctx, key, entry, info, credential)
	case state.StageWaitingForSecret:
		credential, ok := r.credentials.get(key, pending.LeaseID)
		if !ok {
			// Passwords intentionally do not survive restart. Revoking first keeps
			// an otherwise unpatchable lease from becoming an orphan.
			return r.replacePending(ctx, key, entry)
		}
		return r.patchPassword(ctx, key, entry, info, credential)
	case state.StagePasswordPatched:
		if wait, later := waitUntil(now, pending.NextActionAt); later {
			return reconcile.Result{RequeueAfter: wait}, nil
		}
		if !pending.StageDeadline.After(now) {
			return r.markReplacement(ctx, key, entry)
		}
		return r.patchUsername(ctx, key, entry, info)
	case state.StageReconnectPending:
		if wait, later := waitUntil(now, pending.NextActionAt); later {
			return reconcile.Result{RequeueAfter: wait}, nil
		}
		if !pending.StageDeadline.After(now) {
			return r.markReplacement(ctx, key, entry)
		}
		return r.verify(ctx, key, entry, info)
	case state.StageVerified:
		return r.commit(ctx, key, entry)
	default:
		return reconcile.Result{}, fmt.Errorf("unsupported pending stage %q", pending.Stage)
	}
}

func (r *Reconciler) issueForInfo(ctx context.Context, key string, entry state.ClusterState, trigger string, info cnpg.ClusterInfo) (reconcile.Result, error) {
	credential, err := r.vault.IssueDatabaseCredential(ctx, info.SourceName)
	if err != nil {
		r.metrics.VaultOperation(appmetrics.VaultIssue, appmetrics.RotationFailure)
		return reconcile.Result{}, err
	}
	r.metrics.VaultOperation(appmetrics.VaultIssue, appmetrics.RotationSuccess)
	now := r.now().UTC()
	entry.Pending = &state.PendingRotation{
		LeaseID:       credential.LeaseID,
		Username:      credential.Username,
		ExpiresAt:     credential.ExpiresAt.UTC(),
		Stage:         state.StageIssued,
		StageDeadline: now.Add(r.config.Workflow.IssueStageTimeout),
		TriggerID:     trigger,
	}
	updated, err := r.update(ctx, func(store *state.Store) error {
		store.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(updated)
	r.credentials.put(key, credential.LeaseID, credential.Password)
	return reconcile.Result{Requeue: true}, nil
}

func (r *Reconciler) patchPassword(ctx context.Context, key string, entry state.ClusterState, info cnpg.ClusterInfo, credential cachedCredential) (reconcile.Result, error) {
	payload, err := json.Marshal(map[string]map[string]string{"data": {info.SecretKey: base64.StdEncoding.EncodeToString([]byte(credential.password))}})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("encode target Secret patch: %w", err)
	}
	target := &corev1.Secret{}
	target.Namespace, target.Name = info.Namespace, info.SecretName
	err = r.client.Patch(ctx, target, client.RawPatch(types.MergePatchType, payload))
	if err != nil {
		if apierrors.IsNotFound(err) {
			entry.Pending.Stage = state.StageWaitingForSecret
			updated, updateErr := r.update(ctx, func(store *state.Store) error {
				store.Clusters[key] = entry
				return nil
			})
			if updateErr != nil {
				return reconcile.Result{}, updateErr
			}
			r.observe(updated)
			return reconcile.Result{RequeueAfter: transientRequeue}, nil
		}
		return reconcile.Result{}, fmt.Errorf("patch target Secret: %w", err)
	}
	next := r.now().UTC().Add(r.config.Workflow.PasswordPropagationDelay)
	entry.Pending.Stage = state.StagePasswordPatched
	entry.Pending.StageDeadline = r.now().UTC().Add(r.config.Workflow.PasswordUsernameTimeout)
	entry.Pending.NextActionAt = &next
	updated, err := r.update(ctx, func(store *state.Store) error {
		store.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(updated)
	r.credentials.delete(key, credential.leaseID)
	return reconcile.Result{RequeueAfter: r.config.Workflow.PasswordPropagationDelay}, nil
}

func (r *Reconciler) patchUsername(ctx context.Context, key string, entry state.ClusterState, info cnpg.ClusterInfo) (reconcile.Result, error) {
	// Re-fetch immediately before mutation so a concurrent promotion cannot be
	// turned back into a replica credential change by a stale cache observation.
	current := cnpg.NewClusterObject()
	if err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: info.Namespace, Name: info.Name}, current); err != nil {
		return reconcile.Result{}, fmt.Errorf("re-read Cluster before username patch: %w", err)
	}
	currentInfo, err := cnpg.Inspect(current)
	if err != nil {
		return reconcile.Result{}, err
	}
	if !currentInfo.Replica || currentInfo.UID != entry.ClusterUID || currentInfo.SourceName != info.SourceName || currentInfo.ExternalIndex != info.ExternalIndex {
		return reconcile.Result{Requeue: true}, nil
	}
	path := "/spec/externalClusters/" + strconv.Itoa(currentInfo.ExternalIndex) + "/connectionParameters/user"
	payload, err := json.Marshal([]map[string]any{{"op": "replace", "path": path, "value": entry.Pending.Username}})
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("encode Cluster username patch: %w", err)
	}
	target := cnpg.NewClusterObject()
	target.SetNamespace(info.Namespace)
	target.SetName(info.Name)
	if err := r.client.Patch(ctx, target, client.RawPatch(types.JSONPatchType, payload)); err != nil {
		return reconcile.Result{}, fmt.Errorf("patch Cluster username: %w", err)
	}
	next := r.now().UTC().Add(r.config.Workflow.VerificationDelay)
	entry.Pending.Stage = state.StageReconnectPending
	entry.Pending.StageDeadline = r.now().UTC().Add(r.config.Workflow.ReconnectStageTimeout)
	entry.Pending.NextActionAt = &next
	updated, err := r.update(ctx, func(store *state.Store) error {
		store.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(updated)
	return reconcile.Result{RequeueAfter: r.config.Workflow.VerificationDelay}, nil
}

func (r *Reconciler) verify(ctx context.Context, key string, entry state.ClusterState, info cnpg.ClusterInfo) (reconcile.Result, error) {
	active, err := r.status.WalReceiverActive(ctx, info.Namespace, info.PrimaryPod)
	if err != nil {
		return reconcile.Result{RequeueAfter: transientRequeue}, nil
	}
	if !active {
		return reconcile.Result{RequeueAfter: transientRequeue}, nil
	}
	entry.Pending.Stage = state.StageVerified
	entry.Pending.NextActionAt = nil
	updated, err := r.update(ctx, func(store *state.Store) error {
		store.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(updated)
	return reconcile.Result{Requeue: true}, nil
}

func (r *Reconciler) commit(ctx context.Context, key string, entry state.ClusterState) (reconcile.Result, error) {
	pending := entry.Pending
	if pending == nil {
		return reconcile.Result{}, errors.New("commit called without pending rotation")
	}
	if entry.CurrentLeaseID != "" && entry.CurrentLeaseID != pending.LeaseID {
		if err := r.revoke(ctx, entry.CurrentLeaseID); err != nil {
			return reconcile.Result{}, err
		}
	}
	entry.CurrentLeaseID = pending.LeaseID
	expiration := pending.ExpiresAt
	entry.CurrentExpiresAt = &expiration
	entry.LastEvent = pending.TriggerID
	entry.Pending = nil
	updated, err := r.update(ctx, func(store *state.Store) error {
		store.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(updated)
	r.metrics.Rotation(appmetrics.RotationSuccess, triggerKind(pending.TriggerID))
	return reconcile.Result{}, nil
}

func (r *Reconciler) markReplacement(ctx context.Context, key string, entry state.ClusterState) (reconcile.Result, error) {
	entry.Pending.Stage = state.StageReplacementBackoff
	entry.Pending.NextActionAt = nil
	updated, err := r.update(ctx, func(store *state.Store) error {
		store.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	r.observe(updated)
	r.metrics.Rotation(appmetrics.RotationFailure, triggerKind(entry.Pending.TriggerID))
	return reconcile.Result{Requeue: true}, nil
}

func (r *Reconciler) replacePending(ctx context.Context, key string, entry state.ClusterState) (reconcile.Result, error) {
	if entry.Pending != nil {
		if err := r.revoke(ctx, entry.Pending.LeaseID); err != nil {
			return reconcile.Result{}, err
		}
		r.credentials.delete(key, entry.Pending.LeaseID)
		entry.Pending = nil
		updated, err := r.update(ctx, func(store *state.Store) error {
			store.Clusters[key] = entry
			return nil
		})
		if err != nil {
			return reconcile.Result{}, err
		}
		r.observe(updated)
	}
	return reconcile.Result{Requeue: true}, nil
}

func (r *Reconciler) cleanupEntry(ctx context.Context, key string, entry state.ClusterState) error {
	if entry.CurrentLeaseID != "" {
		if err := r.revoke(ctx, entry.CurrentLeaseID); err != nil {
			return err
		}
	}
	if entry.Pending != nil && entry.Pending.LeaseID != "" && entry.Pending.LeaseID != entry.CurrentLeaseID {
		if err := r.revoke(ctx, entry.Pending.LeaseID); err != nil {
			return err
		}
	}
	r.credentials.deleteAny(key)
	updated, err := r.update(ctx, func(store *state.Store) error {
		delete(store.Clusters, key)
		return nil
	})
	if err == nil {
		r.observe(updated)
	}
	return err
}

func (r *Reconciler) revoke(ctx context.Context, leaseID string) error {
	if err := r.vault.RevokeLease(ctx, leaseID); err != nil {
		r.metrics.VaultOperation(appmetrics.VaultRevoke, appmetrics.RotationFailure)
		return err
	}
	r.metrics.VaultOperation(appmetrics.VaultRevoke, appmetrics.RotationSuccess)
	return nil
}

func (r *Reconciler) update(ctx context.Context, mutate func(*state.Store) error) (state.Store, error) {
	return r.state.Update(ctx, mutate)
}

func (r *Reconciler) observe(store state.Store) {
	if r.metrics != nil {
		r.metrics.ObserveState(store)
	}
}

func (c *credentialCache) put(key, leaseID, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = cachedCredential{leaseID: leaseID, password: password}
}

func (c *credentialCache) get(key, leaseID string) (cachedCredential, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	return value, ok && value.leaseID == leaseID
}

func (c *credentialCache) delete(key, leaseID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.values[key]; ok && value.leaseID == leaseID {
		delete(c.values, key)
	}
}

func (c *credentialCache) deleteAny(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.values, key)
}

func waitUntil(now time.Time, deadline *time.Time) (time.Duration, bool) {
	if deadline == nil || !deadline.After(now) {
		return 0, false
	}
	return deadline.Sub(now), true
}

func triggerKind(trigger string) string {
	if trigger == "" {
		return "initialization"
	}
	return "pod_replacement"
}
