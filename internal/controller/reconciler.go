package controller

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	kube "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/telemetry"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type API interface {
	PatchPassword(context.Context, cnpg.Observation, string) error
	PatchUsername(context.Context, *unstructured.Unstructured, cnpg.Observation, string) error
	WALReceiver(context.Context, string, string) (bool, error)
}

type Reconciler struct {
	Client    client.Client
	Reader    client.Reader
	Journal   *state.Journal
	API       API
	Vault     vault.Client
	Config    config.Config
	Metrics   *telemetry.Metrics
	Now       func() time.Time
	mu        sync.Mutex
	passwords map[string]string
	retries   map[string]time.Duration
	absences  map[string]int
	failures  map[string]bool
}

func New(c client.Client, reader client.Reader, j *state.Journal, a API, v vault.Client, cfg config.Config, m *telemetry.Metrics) *Reconciler {
	return &Reconciler{Client: c, Reader: reader, Journal: j, API: a, Vault: v, Config: cfg, Metrics: m, Now: time.Now, passwords: map[string]string{}, retries: map[string]time.Duration{}, absences: map[string]int{}, failures: map[string]bool{}}
}

func (r *Reconciler) allowed(namespace string) bool {
	for _, n := range r.Config.WatchNamespaces {
		if n == namespace {
			return true
		}
	}
	return false
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.allowed(req.Namespace) {
		return ctrl.Result{}, nil
	}
	key := req.NamespacedName.String()
	s, err := r.Journal.Load(ctx)
	if err != nil {
		return r.retry(ctx, key, "state_read"), nil
	}
	r.Metrics.Set(s, r.Config.WatchNamespaces)
	entry, exists := s.Clusters[key]
	var before *state.ClusterState
	if exists {
		copy := entry
		before = &copy
		if entry.Pending != nil {
			p := *entry.Pending
			entry.Pending = &p
		}
	}
	c := cnpg.NewCluster()
	err = r.Reader.Get(ctx, req.NamespacedName, c)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} // Only fresh, consecutive sweeps authorize deletion cleanup.
	if err != nil {
		return r.retry(ctx, key, "cluster_read"), nil
	}
	delete(r.absences, key)
	if c.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}
	if exists && entry.ClusterUID != string(c.GetUID()) {
		if err = r.cleanup(ctx, key, before); err != nil {
			return r.retry(ctx, key, "stale_cleanup"), nil
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	o, err := cnpg.Observe(c)
	if err != nil {
		return r.retry(ctx, key, "relationship"), nil
	}
	if !o.Replica {
		if exists {
			pod := r.primary(ctx, o)
			if pod == nil || !cnpg.Ready(pod) {
				return r.retry(ctx, key, "promotion_not_ready"), nil
			}
			active, err := r.API.WALReceiver(ctx, o.Namespace, o.Primary)
			if err != nil || active {
				return r.retry(ctx, key, "promotion_still_replicating"), nil
			}
			if err = r.cleanup(ctx, key, before); err != nil {
				return r.retry(ctx, key, "promotion_cleanup"), nil
			}
		}
		return ctrl.Result{}, nil
	}
	pod := r.primary(ctx, o)
	trigger := o.Trigger(pod)
	if !exists {
		entry = state.ClusterState{ClusterUID: o.UID}
	}
	if entry.Pending == nil {
		if entry.LastEvent == trigger {
			return ctrl.Result{}, nil
		}
		if entry.CurrentLeaseID != "" && (pod == nil || !cnpg.Ready(pod) || (o.Target != "" && o.Target != o.Primary)) {
			return r.retry(ctx, key, "topology_unsettled"), nil
		}
		// Initialization can precede pg_basebackup and the primary Pod. For an
		// existing relationship the endpoint must be reachable before issuance.
		if pod != nil {
			_, statusErr := r.API.WALReceiver(ctx, o.Namespace, o.Primary)
			if statusErr != nil && entry.CurrentLeaseID != "" {
				return r.retry(ctx, key, "preflight_status"), nil
			}
		}
		return r.issue(ctx, key, before, entry, o, trigger)
	}
	p := entry.Pending
	if !r.Now().Before(p.StageDeadline) {
		r.failure(key, entry)
	}
	// A relationship change cannot continue an old password/username sequence.
	// Keep both tracked leases until the previous attempt is safely retired.
	if !strings.HasPrefix(p.TriggerID, o.Relationship+"/") || !r.Now().Add(r.Config.Vault.SafetyMargin).Before(p.ExpiresAt) {
		if p.Stage != state.StageReplacementBackoff {
			p.Stage = state.StageReplacementBackoff
			p.NextActionAt = nil
			return r.save(ctx, key, before, &entry, time.Millisecond)
		}
	}
	switch p.Stage {
	case state.StageIssued, state.StageWaitingForSecret:
		password, ok := r.passwords[p.LeaseID]
		if !ok {
			p.Stage = state.StageReplacementBackoff
			p.NextActionAt = nil
			return r.save(ctx, key, before, &entry, time.Millisecond)
		}
		if err = r.API.PatchPassword(ctx, o, password); err != nil {
			if errors.Is(err, kube.ErrNotFound) && p.Stage != state.StageWaitingForSecret {
				p.Stage = state.StageWaitingForSecret
				return r.save(ctx, key, before, &entry, r.backoff(key))
			}
			return r.retry(ctx, key, "password_patch"), nil
		}
		p.Stage = state.StagePasswordPatched
		next := r.Now().Add(r.Config.Workflow.PasswordPropagationDelay)
		p.NextActionAt = &next
		p.StageDeadline = r.Now().Add(r.Config.Workflow.PasswordUsernameTimeout)
		// Retain the password until journal acknowledgement: a failed state write
		// can safely repeat the same blind PATCH within this process.
		if err := r.put(ctx, key, before, &entry); err != nil {
			return r.retry(ctx, key, "password_state_write"), nil
		}
		delete(r.passwords, p.LeaseID)
		delete(r.retries, key)
		return ctrl.Result{RequeueAfter: r.Config.Workflow.PasswordPropagationDelay}, nil
	case state.StagePasswordPatched:
		if p.NextActionAt != nil && r.Now().Before(*p.NextActionAt) {
			return ctrl.Result{RequeueAfter: p.NextActionAt.Sub(r.Now())}, nil
		}
		// c came from a fresh read this reconciliation. Atomic resourceVersion
		// tests make a concurrent topology/promotion change reject the PATCH.
		if err = r.API.PatchUsername(ctx, c, o, p.Username); err != nil {
			return r.retry(ctx, key, "username_patch"), nil
		}
		p.Stage = state.StageReconnectPending
		next := r.Now().Add(r.Config.Workflow.VerificationDelay)
		p.NextActionAt = &next
		p.StageDeadline = r.Now().Add(r.Config.Workflow.ReconnectStageTimeout)
		return r.save(ctx, key, before, &entry, r.Config.Workflow.VerificationDelay)
	case state.StageReconnectPending:
		if p.NextActionAt != nil && r.Now().Before(*p.NextActionAt) {
			return ctrl.Result{RequeueAfter: p.NextActionAt.Sub(r.Now())}, nil
		}
		if pod == nil || !cnpg.Ready(pod) || (o.Target != "" && o.Target != o.Primary) || o.Username != p.Username {
			return r.retry(ctx, key, "verification_topology"), nil
		}
		active, err := r.API.WALReceiver(ctx, o.Namespace, o.Primary)
		if err != nil || !active {
			if err == nil && !active && !r.Now().Before(p.StageDeadline) {
				p.Stage = state.StageReplacementBackoff
				p.NextActionAt = nil
				return r.save(ctx, key, before, &entry, r.backoff(key))
			}
			return r.retry(ctx, key, "verification_status"), nil
		}
		p.Stage = state.StageVerified
		p.NextActionAt = nil
		// Bootstrap has no stable episode at issue time. Settle the initial Pod
		// identity when verification succeeds, preventing a second issuance.
		if entry.CurrentLeaseID == "" {
			p.TriggerID = trigger
		}
		return r.save(ctx, key, before, &entry, time.Millisecond)
	case state.StageVerified:
		// Re-check active topology before revocation after any restart/delay.
		if pod == nil || !cnpg.Ready(pod) || (o.Target != "" && o.Target != o.Primary) || o.Username != p.Username {
			return r.retry(ctx, key, "verified_topology"), nil
		}
		active, err := r.API.WALReceiver(ctx, o.Namespace, o.Primary)
		if err != nil || !active {
			return r.retry(ctx, key, "verified_status"), nil
		}
		if entry.CurrentLeaseID != "" {
			if err = r.revoke(ctx, entry.CurrentLeaseID, entry.CurrentExpiresAt); err != nil {
				return r.retry(ctx, key, "old_lease_revoke"), nil
			}
		}
		kind := triggerKind(entry)
		entry.CurrentLeaseID = p.LeaseID
		expiry := p.ExpiresAt
		entry.CurrentExpiresAt = &expiry
		entry.LastEvent = p.TriggerID
		entry.Pending = nil
		if err = r.put(ctx, key, before, &entry); err != nil {
			return r.retry(ctx, key, "finalize_state"), nil
		}
		r.Metrics.Rotation("success", kind)
		delete(r.retries, key)
		delete(r.failures, key)
		ctrl.LoggerFrom(ctx).Info("rotation completed", "namespace", o.Namespace, "cluster", o.Name, "trigger", kind)
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	case state.StageReplacementBackoff:
		// Never revoke a pending credential selected by the live relationship
		// while it can still be used. Wait for expiration, or a source/username
		// change that proves it is no longer selected.
		if o.Username == p.Username && r.Now().Before(p.ExpiresAt) && strings.HasPrefix(p.TriggerID, o.Relationship+"/") {
			active, err := r.API.WALReceiver(ctx, o.Namespace, o.Primary)
			if err != nil || active {
				return r.retry(ctx, key, "active_pending_lease"), nil
			}
		}
		if err = r.revoke(ctx, p.LeaseID, &p.ExpiresAt); err != nil {
			return r.retry(ctx, key, "pending_lease_revoke"), nil
		}
		delete(r.passwords, p.LeaseID)
		return r.issue(ctx, key, before, entry, o, trigger)
	}
	return r.retry(ctx, key, "invalid_stage"), nil
}

func (r *Reconciler) primary(ctx context.Context, o cnpg.Observation) *corev1.Pod {
	if o.Primary == "" {
		return nil
	}
	p := &corev1.Pod{}
	if r.Reader.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: o.Primary}, p) != nil || p.DeletionTimestamp != nil || p.Labels["cnpg.io/cluster"] != o.Name {
		return nil
	}
	return p
}
func triggerKind(s state.ClusterState) string {
	if s.CurrentLeaseID == "" {
		return "initialization"
	}
	if s.Pending != nil && strings.SplitN(s.LastEvent, "/", 2)[0] != strings.SplitN(s.Pending.TriggerID, "/", 2)[0] {
		return "topology_change"
	}
	return "pod_replacement"
}
func (r *Reconciler) failure(key string, s state.ClusterState) {
	if !r.failures[key] {
		r.Metrics.Rotation("failure", triggerKind(s))
		r.failures[key] = true
	}
}
func (r *Reconciler) issue(ctx context.Context, key string, before *state.ClusterState, entry state.ClusterState, o cnpg.Observation, trigger string) (ctrl.Result, error) {
	credential, err := r.Vault.IssueDatabaseCredential(ctx, o.Source)
	if err != nil {
		r.Metrics.Vault("issue", "failure")
		r.failure(key, entry)
		if credential.LeaseID != "" {
			entry.Pending = &state.PendingRotation{LeaseID: credential.LeaseID, Username: credential.Username, ExpiresAt: credential.ExpiresAt, Stage: state.StageReplacementBackoff, StageDeadline: r.Now().Add(r.Config.Workflow.IssueStageTimeout), TriggerID: trigger}
			if err := r.put(ctx, key, before, &entry); err != nil {
				_ = r.revoke(ctx, credential.LeaseID, &credential.ExpiresAt)
			}
		}
		return r.retry(ctx, key, "vault_issue"), nil
	}
	r.Metrics.Vault("issue", "success")
	entry.Pending = &state.PendingRotation{LeaseID: credential.LeaseID, Username: credential.Username, ExpiresAt: credential.ExpiresAt, Stage: state.StageIssued, StageDeadline: r.Now().Add(r.Config.Workflow.IssueStageTimeout), TriggerID: trigger}
	if err = r.put(ctx, key, before, &entry); err != nil {
		// A normal write failure has a known lease. Best-effort revoke it; a
		// crash at this cross-system boundary remains bounded by the Vault TTL.
		_ = r.revoke(ctx, credential.LeaseID, &credential.ExpiresAt)
		return r.retry(ctx, key, "pending_persist"), nil
	}
	r.passwords[credential.LeaseID] = credential.Password
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}
func (r *Reconciler) put(ctx context.Context, key string, before, after *state.ClusterState) error {
	if err := r.Journal.Put(ctx, key, before, after); err != nil {
		return err
	}
	s, err := r.Journal.Load(ctx)
	if err == nil {
		r.Metrics.Set(s, r.Config.WatchNamespaces)
	}
	return nil
}
func (r *Reconciler) save(ctx context.Context, key string, before, after *state.ClusterState, delay time.Duration) (ctrl.Result, error) {
	if err := r.put(ctx, key, before, after); err != nil {
		return r.retry(ctx, key, "state_write"), nil
	}
	delete(r.retries, key)
	return ctrl.Result{RequeueAfter: delay}, nil
}
func (r *Reconciler) backoff(key string) time.Duration {
	d := r.retries[key]
	if d == 0 {
		d = r.Config.Workflow.RetryInitialDelay
	} else {
		d *= 2
	}
	if d > r.Config.Workflow.RetryMaxDelay {
		d = r.Config.Workflow.RetryMaxDelay
	}
	r.retries[key] = d
	return d/2 + time.Duration(rand.Float64()*float64(d/2))
}
func (r *Reconciler) retry(ctx context.Context, key, stage string) ctrl.Result {
	ctrl.LoggerFrom(ctx).Info("workflow retry", "cluster", key, "stage", stage)
	return ctrl.Result{RequeueAfter: r.backoff(key)}
}
func (r *Reconciler) revoke(ctx context.Context, id string, expires *time.Time) error {
	err := r.Vault.RevokeLease(ctx, id)
	if err != nil {
		r.Metrics.Vault("revoke", "failure")
		if expires != nil && !r.Now().Before(*expires) {
			return nil
		}
		return errors.New("Vault revocation failed")
	}
	r.Metrics.Vault("revoke", "success")
	return nil
}
func (r *Reconciler) cleanup(ctx context.Context, key string, s *state.ClusterState) error {
	if s == nil {
		return nil
	}
	if s.CurrentLeaseID != "" {
		if err := r.revoke(ctx, s.CurrentLeaseID, s.CurrentExpiresAt); err != nil {
			return err
		}
	}
	if s.Pending != nil {
		if err := r.revoke(ctx, s.Pending.LeaseID, &s.Pending.ExpiresAt); err != nil {
			return err
		}
		delete(r.passwords, s.Pending.LeaseID)
	}
	if err := r.put(ctx, key, s, nil); err != nil {
		return err
	}
	delete(r.absences, key)
	delete(r.retries, key)
	delete(r.failures, key)
	return nil
}

// Sweep uses uncached reads and counts only consecutive confirmed absences.
// Its mutex shares serialization with the single reconciliation worker.
func (r *Reconciler) Sweep(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.Journal.Load(ctx)
	if err != nil {
		return err
	}
	r.Metrics.Set(s, r.Config.WatchNamespaces)
	for key, entry := range s.Clusters {
		parts := strings.SplitN(key, "/", 2)
		if !r.allowed(parts[0]) {
			continue
		}
		c := cnpg.NewCluster()
		err := r.Reader.Get(ctx, client.ObjectKey{Namespace: parts[0], Name: parts[1]}, c)
		if apierrors.IsNotFound(err) {
			r.absences[key]++
			if r.absences[key] < r.Config.Workflow.OrphanAbsenceSweeps {
				continue
			}
		} else if err != nil {
			delete(r.absences, key)
			continue
		} else {
			delete(r.absences, key)
			if string(c.GetUID()) == entry.ClusterUID {
				continue
			}
		}
		if err := r.cleanup(ctx, key, &entry); err != nil {
			ctrl.LoggerFrom(ctx).Info("cleanup retry", "cluster", key)
		}
	}
	return nil
}

func (r *Reconciler) NeedLeaderElection() bool { return true }
func (r *Reconciler) Start(ctx context.Context) error {
	// Startup restores metric state even for an unwatched namespace, without
	// altering it. Initial Cluster informer events resume all visible workflows.
	if err := r.Sweep(ctx); err != nil {
		ctrl.LoggerFrom(ctx).Info("startup state unavailable")
	}
	ticker := time.NewTicker(r.Config.Workflow.OrphanSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Sweep(ctx); err != nil {
				ctrl.LoggerFrom(ctx).Info("orphan sweep retry")
			}
		}
	}
}
