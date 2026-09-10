package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	kubeapi "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

const observationRetry = 5 * time.Second

// StateStore is the durable journal boundary. Mutate callbacks are pure and
// may be retried after optimistic concurrency conflicts.
type StateStore interface {
	Read(context.Context) (state.Store, error)
	Mutate(context.Context, func(*state.Store) error) (state.Store, error)
}

// MetricRecorder is the finite-label telemetry surface.
type MetricRecorder interface {
	RecordRotation(result, trigger string) error
	RecordVault(operation, result string) error
	SetState(state.Store)
}

// Reconciler serializes one durable workflow per Cluster queue key.
type Reconciler struct {
	Client       client.Client
	State        StateStore
	Vault        vault.Client
	Kube         kubeapi.Mutator
	Metrics      MetricRecorder
	Config       config.WorkflowConfig
	SafetyMargin time.Duration
	Now          func() time.Time

	passwordsMu sync.Mutex
	passwords   map[string]string
}

// NewReconciler validates the boundaries needed by reconciliation.
func NewReconciler(kubeClient client.Client, store StateStore, vaultClient vault.Client, mutator kubeapi.Mutator, recorder MetricRecorder, workflow config.WorkflowConfig, safetyMargin time.Duration, now func() time.Time) (*Reconciler, error) {
	if kubeClient == nil || store == nil || vaultClient == nil || mutator == nil || recorder == nil {
		return nil, errors.New("controller boundaries are incomplete")
	}
	if workflow.PasswordPropagationDelay <= 0 || workflow.VerificationDelay <= 0 || workflow.IssueStageTimeout <= 0 || workflow.PasswordUsernameTimeout <= 0 || workflow.ReconnectStageTimeout <= 0 || safetyMargin <= 0 {
		return nil, errors.New("controller timing configuration is invalid")
	}
	if now == nil {
		now = time.Now
	}
	return &Reconciler{
		Client: kubeClient, State: store, Vault: vaultClient, Kube: mutator, Metrics: recorder,
		Config: workflow, SafetyMargin: safetyMargin, Now: now, passwords: map[string]string{},
	}, nil
}

// Reconcile resumes exactly the stage recorded in the state Secret.
func (reconciler *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	cluster := cnpg.NewCluster()
	if err := reconciler.Client.Get(ctx, request.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("read Cluster: %w", err)
	}

	store, err := reconciler.readState(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	key := request.Namespace + "/" + request.Name
	entry, exists := store.Clusters[key]
	clusterUID := string(cluster.GetUID())
	if exists && entry.ClusterUID != clusterUID {
		if err := reconciler.cleanupEntry(ctx, key, entry); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: observationRetry}, nil
	}

	replica, _, replicaErr := cnpg.ReplicaMode(cluster)
	if replicaErr != nil {
		return ctrl.Result{}, errors.New("Cluster replica mode is malformed")
	}
	if !replica {
		if exists {
			if err := reconciler.cleanupEntry(ctx, key, entry); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	primaryName := cnpg.DesignatedPrimary(cluster)
	if primaryName == "" {
		return ctrl.Result{RequeueAfter: observationRetry}, nil
	}
	pod := &corev1.Pod{}
	if err := reconciler.Client.Get(ctx, types.NamespacedName{Namespace: request.Namespace, Name: primaryName}, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("read designated-primary Pod: %w", err)
		}
		pod = nil
	}
	observation, err := cnpg.Observe(cluster, pod)
	if err != nil {
		return ctrl.Result{RequeueAfter: observationRetry}, nil
	}
	triggerID := observation.TriggerID()

	if !exists {
		store, err = reconciler.State.Mutate(ctx, func(current *state.Store) error {
			if _, present := current.Clusters[key]; !present {
				current.Clusters[key] = state.ClusterState{ClusterUID: observation.UID}
			}
			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		reconciler.Metrics.SetState(store)
		entry = store.Clusters[key]
	}
	if entry.CurrentLeaseID != "" && observation.PrimaryPodUID == "" {
		// A transition-only Cluster observation is sufficient to bootstrap a
		// relationship whose first instance cannot exist until credentials work.
		// Once a current lease exists, however, an old Pod deletion is only a
		// hint: wait for the replacement/designated Pod to become observable.
		return ctrl.Result{RequeueAfter: observationRetry}, nil
	}

	if entry.Pending == nil && entry.LastEvent == triggerID {
		return ctrl.Result{}, nil
	}
	if entry.Pending == nil && bootstrapEquivalent(entry.LastEvent, triggerID) {
		store, err = reconciler.State.Mutate(ctx, func(current *state.Store) error {
			currentEntry := current.Clusters[key]
			if currentEntry.Pending == nil && currentEntry.LastEvent == entry.LastEvent {
				currentEntry.LastEvent = triggerID
				current.Clusters[key] = currentEntry
			}
			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		reconciler.Metrics.SetState(store)
		return ctrl.Result{}, nil
	}
	if entry.Pending != nil && !strings.Contains(entry.Pending.TriggerID, "/source/"+observation.Source+"/") {
		if err := reconciler.cleanupEntry(ctx, key, entry); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: observationRetry}, nil
	}

	return reconciler.advance(ctx, key, observation, entry, triggerID)
}

func (reconciler *Reconciler) advance(ctx context.Context, key string, observation cnpg.Observation, entry state.ClusterState, triggerID string) (ctrl.Result, error) {
	now := reconciler.Now().UTC()
	triggerKind := cnpg.TriggerKind(entry.LastEvent, triggerID)
	if entry.Pending == nil {
		credential, err := reconciler.issue(ctx, observation.Source)
		if err != nil {
			return ctrl.Result{}, err
		}
		pending := &state.PendingRotation{
			LeaseID: credential.LeaseID, Username: credential.Username, ExpiresAt: credential.ExpiresAt,
			Stage: state.StageIssued, StageDeadline: now.Add(reconciler.Config.IssueStageTimeout), TriggerID: triggerID,
		}
		store, err := reconciler.State.Mutate(ctx, func(current *state.Store) error {
			currentEntry, ok := current.Clusters[key]
			if !ok || currentEntry.ClusterUID != observation.UID {
				return errors.New("Cluster state changed before pending lease persistence")
			}
			if currentEntry.Pending == nil {
				currentEntry.Pending = pending
				current.Clusters[key] = currentEntry
			}
			return nil
		})
		if err != nil {
			// This is the unavoidable issue/persist gap. The lease remains bounded
			// by Vault and no credential Kubernetes object was changed.
			return ctrl.Result{}, err
		}
		reconciler.rememberPassword(key, credential.LeaseID, credential.Password)
		reconciler.Metrics.SetState(store)
		ctrl.LoggerFrom(ctx).Info("credential workflow started", "phase", state.StageIssued, "trigger", triggerKind)
		entry = store.Clusters[key]
	}

	if !now.Add(reconciler.SafetyMargin).Before(entry.Pending.ExpiresAt) && entry.Pending.Stage != state.StageReplacementBackoff {
		updated, err := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StageReplacementBackoff, now.Add(reconciler.Config.IssueStageTimeout), nil)
		if err != nil {
			return ctrl.Result{}, err
		}
		entry = updated
		reconciler.rotationFailure(triggerKind)
	}

	switch entry.Pending.Stage {
	case state.StageIssued, state.StageWaitingForSecret:
		_, current, err := reconciler.relationshipCurrent(ctx, observation)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !current {
			return ctrl.Result{RequeueAfter: observationRetry}, nil
		}
		password, ok := reconciler.password(key, entry.Pending.LeaseID)
		if !ok {
			updated, err := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StageReplacementBackoff, now.Add(reconciler.Config.IssueStageTimeout), nil)
			if err != nil {
				return ctrl.Result{}, err
			}
			return reconciler.replace(ctx, key, observation, updated, triggerID)
		}
		if err := reconciler.Kube.PatchSecretPassword(ctx, observation.Namespace, observation.External.SecretName, observation.External.SecretKey, password); err != nil {
			if errors.Is(err, kubeapi.ErrNotFound) {
				if entry.Pending.Stage != state.StageWaitingForSecret {
					updated, updateErr := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StageWaitingForSecret, entry.Pending.StageDeadline, nil)
					if updateErr != nil {
						return ctrl.Result{}, updateErr
					}
					entry = updated
				}
				return ctrl.Result{}, errors.New("target credential Secret is not available")
			}
			return ctrl.Result{}, err
		}
		next := now.Add(reconciler.Config.PasswordPropagationDelay)
		updated, err := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StagePasswordPatched, now.Add(reconciler.Config.PasswordUsernameTimeout), &next)
		if err != nil {
			return ctrl.Result{}, err
		}
		reconciler.forgetPassword(key, entry.Pending.LeaseID)
		entry = updated
		return ctrl.Result{RequeueAfter: reconciler.until(next)}, nil

	case state.StagePasswordPatched:
		if entry.Pending.NextActionAt != nil && now.Before(*entry.Pending.NextActionAt) {
			return ctrl.Result{RequeueAfter: reconciler.until(*entry.Pending.NextActionAt)}, nil
		}
		latest, current, err := reconciler.relationshipCurrent(ctx, observation)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !current {
			return ctrl.Result{RequeueAfter: observationRetry}, nil
		}
		if err := reconciler.Kube.PatchClusterUsername(ctx, observation.Namespace, observation.Name, latest.External.Index, entry.Pending.Username); err != nil {
			return ctrl.Result{}, err
		}
		next := now.Add(reconciler.Config.VerificationDelay)
		updated, err := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StageReconnectPending, now.Add(reconciler.Config.ReconnectStageTimeout), &next)
		if err != nil {
			return ctrl.Result{}, err
		}
		entry = updated
		return ctrl.Result{RequeueAfter: reconciler.until(next)}, nil

	case state.StageReconnectPending:
		if entry.Pending.NextActionAt != nil && now.Before(*entry.Pending.NextActionAt) {
			return ctrl.Result{RequeueAfter: reconciler.until(*entry.Pending.NextActionAt)}, nil
		}
		if !now.Before(entry.Pending.StageDeadline) {
			updated, err := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StageReplacementBackoff, now.Add(reconciler.Config.IssueStageTimeout), nil)
			if err != nil {
				return ctrl.Result{}, err
			}
			reconciler.rotationFailure(triggerKind)
			return reconciler.replace(ctx, key, observation, updated, triggerID)
		}
		if !observation.PrimaryReady {
			return ctrl.Result{}, errors.New("designated-primary Pod is not Ready")
		}
		active, err := reconciler.Kube.WalReceiverActive(ctx, observation.Namespace, observation.CurrentPrimary)
		if err != nil || !active {
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, errors.New("WAL receiver is not active")
		}
		updated, err := reconciler.setStage(ctx, key, entry.Pending.LeaseID, state.StageVerified, entry.Pending.StageDeadline, nil)
		if err != nil {
			return ctrl.Result{}, err
		}
		entry = updated
		fallthrough

	case state.StageVerified:
		if entry.CurrentLeaseID != "" {
			if err := reconciler.revoke(ctx, entry.CurrentLeaseID); err != nil {
				return ctrl.Result{}, err
			}
		}
		store, err := reconciler.State.Mutate(ctx, func(current *state.Store) error {
			currentEntry, ok := current.Clusters[key]
			if !ok || currentEntry.Pending == nil || currentEntry.Pending.LeaseID != entry.Pending.LeaseID {
				return errors.New("pending workflow changed before commit")
			}
			expires := currentEntry.Pending.ExpiresAt
			currentEntry.CurrentLeaseID = currentEntry.Pending.LeaseID
			currentEntry.CurrentExpiresAt = &expires
			currentEntry.LastEvent = currentEntry.Pending.TriggerID
			currentEntry.Pending = nil
			current.Clusters[key] = currentEntry
			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		reconciler.Metrics.SetState(store)
		_ = reconciler.Metrics.RecordRotation("success", triggerKind)
		ctrl.LoggerFrom(ctx).Info("credential workflow completed", "result", "success", "trigger", triggerKind)
		return ctrl.Result{}, nil

	case state.StageReplacementBackoff:
		return reconciler.replace(ctx, key, observation, entry, triggerID)
	default:
		return ctrl.Result{}, errors.New("pending workflow has an invalid stage")
	}
}

func (reconciler *Reconciler) replace(ctx context.Context, key string, observation cnpg.Observation, entry state.ClusterState, triggerID string) (ctrl.Result, error) {
	if err := reconciler.revoke(ctx, entry.Pending.LeaseID); err != nil {
		return ctrl.Result{}, err
	}
	reconciler.forgetPassword(key, entry.Pending.LeaseID)
	credential, err := reconciler.issue(ctx, observation.Source)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := reconciler.Now().UTC()
	store, err := reconciler.State.Mutate(ctx, func(current *state.Store) error {
		currentEntry, ok := current.Clusters[key]
		if !ok || currentEntry.Pending == nil || currentEntry.Pending.LeaseID != entry.Pending.LeaseID || currentEntry.Pending.Stage != state.StageReplacementBackoff {
			return errors.New("replacement workflow changed before lease persistence")
		}
		currentEntry.Pending = &state.PendingRotation{
			LeaseID: credential.LeaseID, Username: credential.Username, ExpiresAt: credential.ExpiresAt,
			Stage: state.StageIssued, StageDeadline: now.Add(reconciler.Config.IssueStageTimeout), TriggerID: triggerID,
		}
		current.Clusters[key] = currentEntry
		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	reconciler.rememberPassword(key, credential.LeaseID, credential.Password)
	reconciler.Metrics.SetState(store)
	return reconciler.advance(ctx, key, observation, store.Clusters[key], triggerID)
}

func (reconciler *Reconciler) issue(ctx context.Context, role string) (vault.Credential, error) {
	credential, err := reconciler.Vault.IssueDatabaseCredential(ctx, role)
	result := "success"
	if err != nil {
		result = "failure"
	}
	_ = reconciler.Metrics.RecordVault("issue", result)
	if err != nil {
		return vault.Credential{}, errors.New("Vault credential issuance failed")
	}
	return credential, nil
}

func (reconciler *Reconciler) revoke(ctx context.Context, leaseID string) error {
	err := reconciler.Vault.RevokeLease(ctx, leaseID)
	result := "success"
	if err != nil {
		result = "failure"
	}
	_ = reconciler.Metrics.RecordVault("revoke", result)
	if err != nil {
		return errors.New("Vault lease revocation failed")
	}
	return nil
}

func (reconciler *Reconciler) cleanupEntry(ctx context.Context, key string, entry state.ClusterState) error {
	if entry.Pending != nil {
		if err := reconciler.revoke(ctx, entry.Pending.LeaseID); err != nil {
			return err
		}
		reconciler.forgetPassword(key, entry.Pending.LeaseID)
	}
	if entry.CurrentLeaseID != "" {
		if err := reconciler.revoke(ctx, entry.CurrentLeaseID); err != nil {
			return err
		}
	}
	store, err := reconciler.State.Mutate(ctx, func(current *state.Store) error {
		if currentEntry, ok := current.Clusters[key]; ok && currentEntry.ClusterUID == entry.ClusterUID {
			delete(current.Clusters, key)
		}
		return nil
	})
	if err != nil {
		return err
	}
	reconciler.Metrics.SetState(store)
	ctrl.LoggerFrom(ctx).Info("credential relationship state cleaned", "clusterKey", key)
	return nil
}

// CleanupKey is used only after the orphan sweeper has confirmed absence.
func (reconciler *Reconciler) CleanupKey(ctx context.Context, key string) error {
	store, err := reconciler.readState(ctx)
	if err != nil {
		return err
	}
	entry, ok := store.Clusters[key]
	if !ok {
		return nil
	}
	return reconciler.cleanupEntry(ctx, key, entry)
}

func (reconciler *Reconciler) setStage(ctx context.Context, key, leaseID string, stage state.Stage, deadline time.Time, next *time.Time) (state.ClusterState, error) {
	store, err := reconciler.State.Mutate(ctx, func(current *state.Store) error {
		entry, ok := current.Clusters[key]
		if !ok || entry.Pending == nil || entry.Pending.LeaseID != leaseID {
			return errors.New("pending workflow changed during stage transition")
		}
		entry.Pending.Stage = stage
		entry.Pending.StageDeadline = deadline
		entry.Pending.NextActionAt = next
		current.Clusters[key] = entry
		return nil
	})
	if err != nil {
		return state.ClusterState{}, err
	}
	reconciler.Metrics.SetState(store)
	ctrl.LoggerFrom(ctx).Info("credential workflow state changed", "clusterKey", key, "phase", stage)
	return store.Clusters[key], nil
}

func (reconciler *Reconciler) readState(ctx context.Context) (state.Store, error) {
	store, err := reconciler.State.Read(ctx)
	if err == nil {
		reconciler.Metrics.SetState(store)
	}
	return store, err
}

// relationshipCurrent is the last-moment mutation guard against promotion,
// deletion, UID reuse, source changes, and credential-reference changes.
func (reconciler *Reconciler) relationshipCurrent(ctx context.Context, expected cnpg.Observation) (cnpg.Observation, bool, error) {
	cluster := cnpg.NewCluster()
	if err := reconciler.Client.Get(ctx, types.NamespacedName{Namespace: expected.Namespace, Name: expected.Name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return cnpg.Observation{}, false, nil
		}
		return cnpg.Observation{}, false, fmt.Errorf("re-read Cluster mutation precondition: %w", err)
	}
	replica, _, err := cnpg.ReplicaMode(cluster)
	if err != nil || !replica || string(cluster.GetUID()) != expected.UID {
		return cnpg.Observation{}, false, nil
	}
	primary := cnpg.DesignatedPrimary(cluster)
	if primary == "" {
		return cnpg.Observation{}, false, nil
	}
	var pod *corev1.Pod
	observedPod := &corev1.Pod{}
	if err := reconciler.Client.Get(ctx, types.NamespacedName{Namespace: expected.Namespace, Name: primary}, observedPod); err != nil {
		if !apierrors.IsNotFound(err) {
			return cnpg.Observation{}, false, fmt.Errorf("re-read Pod mutation precondition: %w", err)
		}
	} else {
		pod = observedPod
	}
	current, err := cnpg.Observe(cluster, pod)
	if err != nil {
		return cnpg.Observation{}, false, nil
	}
	valid := current.Source == expected.Source &&
		current.External.SecretName == expected.External.SecretName &&
		current.External.SecretKey == expected.External.SecretKey
	return current, valid, nil
}

func (reconciler *Reconciler) rotationFailure(trigger string) {
	_ = reconciler.Metrics.RecordRotation("failure", trigger)
}

func (reconciler *Reconciler) until(target time.Time) time.Duration {
	duration := target.Sub(reconciler.Now().UTC())
	if duration <= 0 {
		return time.Millisecond
	}
	return duration
}

func (reconciler *Reconciler) password(key, leaseID string) (string, bool) {
	reconciler.passwordsMu.Lock()
	defer reconciler.passwordsMu.Unlock()
	password, ok := reconciler.passwords[key+"\x00"+leaseID]
	return password, ok
}

func (reconciler *Reconciler) rememberPassword(key, leaseID, password string) {
	reconciler.passwordsMu.Lock()
	reconciler.passwords[key+"\x00"+leaseID] = password
	reconciler.passwordsMu.Unlock()
}

func (reconciler *Reconciler) forgetPassword(key, leaseID string) {
	reconciler.passwordsMu.Lock()
	delete(reconciler.passwords, key+"\x00"+leaseID)
	reconciler.passwordsMu.Unlock()
}

func bootstrapEquivalent(previous, current string) bool {
	transitionIndex := strings.Index(previous, "/transition/")
	podIndex := strings.Index(current, "/pod/")
	return transitionIndex > 0 && podIndex > 0 && previous[:transitionIndex] == current[:podIndex]
}
