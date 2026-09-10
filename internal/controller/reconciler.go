package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	appconfig "github.com/ardentperf/vault-replica-credentials/internal/config"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	domainmetrics "github.com/ardentperf/vault-replica-credentials/internal/metrics"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

type Clock interface{ Now() time.Time }
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type ReconcilerConfig struct {
	Client    client.Client
	APIReader client.Reader
	State     kubesetup.StateStore
	Vault     vault.Client
	Status    interface {
		Read(context.Context, string, string) (kubesetup.InstanceStatus, error)
	}
	Config    appconfig.Config
	Metrics   *domainmetrics.Metrics
	Clock     Clock
	Logger    *slog.Logger
	RetryBase time.Duration
	RetryMax  time.Duration
}

type Config = ReconcilerConfig

type Reconciler struct {
	client client.Client
	reader client.Reader
	state  kubesetup.StateStore
	vault  vault.Client
	status interface {
		Read(context.Context, string, string) (kubesetup.InstanceStatus, error)
	}
	cfg                 appconfig.Config
	metrics             *domainmetrics.Metrics
	clock               Clock
	logger              *slog.Logger
	retryBase, retryMax time.Duration

	mu               sync.Mutex
	credentials      map[string]vault.Credential
	backoff          map[string]int
	failed           map[string]struct{}
	abstractAbsences map[string]int
}

func NewReconciler(cfg ReconcilerConfig) (*Reconciler, error) {
	if cfg.Client == nil {
		return nil, errors.New("controller Kubernetes client is required")
	}
	if cfg.State == nil {
		return nil, errors.New("controller state store is required")
	}
	if cfg.Vault == nil {
		return nil, errors.New("controller Vault client is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 30 * time.Second
	}
	return &Reconciler{client: cfg.Client, reader: cfg.APIReader, state: cfg.State, vault: cfg.Vault,
		status: cfg.Status, cfg: cfg.Config, metrics: cfg.Metrics, clock: cfg.Clock, logger: cfg.Logger,
		retryBase: cfg.RetryBase, retryMax: cfg.RetryMax, credentials: map[string]vault.Credential{},
		backoff: map[string]int{}, failed: map[string]struct{}{}, abstractAbsences: map[string]int{}}, nil
}

func New(cfg ReconcilerConfig) (*Reconciler, error) { return NewReconciler(cfg) }

func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.cfg.Runtime.Workers}).
		For(&cnpg.Cluster{}, builder.WithPredicates(ClusterPredicate())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
			pod, ok := object.(*corev1.Pod)
			if !ok {
				return nil
			}
			name := podClusterName(pod)
			if name == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: name}}}
		}), builder.WithPredicates(PodPredicate())).
		Complete(r)
}

// Reconcile is serialized by controller-runtime using the namespace/name
// Cluster key. Every mutation is preceded by a fresh Cluster validation.
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	cluster := &cnpg.Cluster{}
	if err := r.client.Get(ctx, request.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	key := request.NamespacedName.String()
	store, err := r.state.Load(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("load controller state: %w", err)
	}
	entry, hasEntry := store.Clusters[key]
	if hasEntry {
		r.syncMetrics(key, entry)
	}

	if hasEntry && entry.ClusterUID != string(cluster.UID) {
		if err := r.cleanupEntry(ctx, key, entry); err != nil {
			return r.retryResult(key), err
		}
		delete(store.Clusters, key)
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		r.clearMemory(key)
		return ctrl.Result{Requeue: true}, nil
	}

	if !cluster.IsReplica() {
		if !hasEntry {
			return ctrl.Result{}, nil
		}
		if err := r.cleanupEntry(ctx, key, entry); err != nil {
			return r.retryResult(key), err
		}
		delete(store.Clusters, key)
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		r.clearMemory(key)
		return ctrl.Result{}, nil
	}

	if !hasEntry {
		entry = state.ClusterState{ClusterUID: string(cluster.UID)}
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		hasEntry = true
	}

	pod, ready, err := r.designatedPrimary(ctx, cluster)
	if err != nil {
		return r.retryResult(key), err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: r.retryDuration(key)}, nil
	}
	trigger := triggerFor(cluster, pod)
	if entry.LastEvent == "" && entry.Pending == nil {
		trigger = "initialization/" + trigger
	}
	if entry.Pending == nil {
		if trigger == entry.LastEvent || stringsTrimInitialization(entry.LastEvent) == trigger {
			return ctrl.Result{}, nil
		}
		if r.status == nil {
			return r.failure(key, trigger, errors.New("instance-manager status reader is not configured"))
		}
		status, statusErr := r.status.Read(ctx, cluster.Namespace, pod.Name)
		if statusErr != nil {
			r.logger.Warn("instance-manager status readiness retry", "namespace", cluster.Namespace, "cluster", cluster.Name, "error", statusErr)
			return r.retryResult(key), nil
		}
		if !status.IsWalReceiverActive {
			return ctrl.Result{RequeueAfter: r.retryDuration(key)}, nil
		}
		return r.issue(ctx, store, cluster, key, trigger)
	}
	return r.resume(ctx, store, cluster, pod, key, trigger)
}

func (r *Reconciler) issue(ctx context.Context, store state.Store, cluster *cnpg.Cluster, key, trigger string) (ctrl.Result, error) {
	role, err := cluster.SourceRole()
	if err != nil {
		return r.failure(key, trigger, err)
	}
	credential, err := r.vault.IssueDatabaseCredential(ctx, role)
	if err != nil {
		r.metricsVault("issue", "failure")
		return r.failure(key, trigger, err)
	}
	r.metricsVault("issue", "success")
	now := r.clock.Now()
	pending := &state.PendingRotation{LeaseID: credential.LeaseID, Username: credential.Username, ExpiresAt: credential.ExpiresAt,
		Stage: state.StageIssued, StageDeadline: now.Add(r.cfg.Workflow.IssueStageTimeout), TriggerID: trigger}
	entry := store.Clusters[key]
	entry.ClusterUID = string(cluster.UID)
	entry.Pending = pending
	store.Clusters[key] = entry
	if err := r.state.Save(ctx, store); err != nil {
		return ctrl.Result{}, err
	}
	r.mu.Lock()
	r.credentials[key] = credential
	r.backoff[key] = 0
	r.mu.Unlock()
	r.metricsPending(key, pending.Stage)
	return r.resume(ctx, store, cluster, nil, key, trigger)
}

func (r *Reconciler) resume(ctx context.Context, store state.Store, cluster *cnpg.Cluster, pod *corev1.Pod, key, trigger string) (ctrl.Result, error) {
	entry := store.Clusters[key]
	pending := entry.Pending
	if pending == nil {
		return ctrl.Result{}, nil
	}
	now := r.clock.Now()
	if !pending.ExpiresAt.After(now.Add(r.cfg.Vault.SafetyMargin)) {
		return r.replacePending(ctx, store, cluster, key, trigger)
	}

	r.mu.Lock()
	credential, inMemory := r.credentials[key]
	r.mu.Unlock()
	if (pending.Stage == state.StageIssued || pending.Stage == state.StageWaitingForSecret) && !inMemory {
		// Passwords are intentionally not recoverable after restart. Revoke the
		// journaled lease before beginning a replacement issuance.
		if err := r.revoke(ctx, pending.LeaseID); err != nil {
			return r.retryResult(key), err
		}
		pending = nil
		entry.Pending = nil
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		return r.issue(ctx, store, cluster, key, trigger)
	}

	switch pending.Stage {
	case state.StageIssued, state.StageWaitingForSecret:
		if pending.Stage == state.StageWaitingForSecret && pending.NextActionAt != nil && pending.NextActionAt.After(now) {
			return ctrl.Result{RequeueAfter: pending.NextActionAt.Sub(now)}, nil
		}
		secretName, secretKey, err := cluster.CredentialReference()
		if err != nil {
			return r.failure(key, trigger, err)
		}
		if err := (kubesetup.ClientSecretPatcher{Client: r.client}).PatchPassword(ctx, cluster.Namespace, secretName, secretKey, credential.Password); err != nil {
			if apierrors.IsNotFound(err) {
				pending.Stage = state.StageWaitingForSecret
				pending.NextActionAt = r.nextAction(key, now)
				store.Clusters[key] = entry
				if saveErr := r.state.Save(ctx, store); saveErr != nil {
					return ctrl.Result{}, saveErr
				}
				r.metricsPending(key, pending.Stage)
				return ctrl.Result{RequeueAfter: pending.NextActionAt.Sub(now)}, nil
			}
			return r.retryResult(key), err
		}
		pending.Stage = state.StagePasswordPatched
		pending.StageDeadline = now.Add(r.cfg.Workflow.PasswordUsernameTimeout)
		next := now.Add(r.cfg.Workflow.PasswordPropagationDelay)
		pending.NextActionAt = &next
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		r.mu.Lock()
		delete(r.credentials, key)
		r.mu.Unlock()
		r.metricsPending(key, pending.Stage)
		return ctrl.Result{RequeueAfter: r.cfg.Workflow.PasswordPropagationDelay}, nil

	case state.StagePasswordPatched:
		if now.After(pending.StageDeadline) {
			return r.replacePending(ctx, store, cluster, key, trigger)
		}
		if pending.NextActionAt != nil && pending.NextActionAt.After(now) {
			return ctrl.Result{RequeueAfter: pending.NextActionAt.Sub(now)}, nil
		}
		// Re-read validation is performed by the Get at the beginning of every
		// reconcile. Promotion therefore stops the credential mutation here.
		if !cluster.IsReplica() {
			return ctrl.Result{Requeue: true}, nil
		}
		role, err := cluster.SourceRole()
		if err != nil {
			return r.failure(key, trigger, err)
		}
		if err := kubesetup.PatchClusterUsername(ctx, r.client, cluster, role, pending.Username); err != nil {
			return r.retryResult(key), err
		}
		pending.Stage = state.StageReconnectPending
		pending.StageDeadline = now.Add(r.cfg.Workflow.ReconnectStageTimeout)
		next := now.Add(r.cfg.Workflow.VerificationDelay)
		pending.NextActionAt = &next
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		r.metricsPending(key, pending.Stage)
		return ctrl.Result{RequeueAfter: r.cfg.Workflow.VerificationDelay}, nil

	case state.StageReconnectPending:
		if pending.NextActionAt != nil && pending.NextActionAt.After(now) {
			return ctrl.Result{RequeueAfter: pending.NextActionAt.Sub(now)}, nil
		}
		if now.After(pending.StageDeadline) {
			return r.replacePending(ctx, store, cluster, key, trigger)
		}
		if pod == nil {
			var ready bool
			var err error
			pod, ready, err = r.designatedPrimary(ctx, cluster)
			if err != nil {
				return r.retryResult(key), err
			}
			if !ready {
				return ctrl.Result{RequeueAfter: r.retryDuration(key)}, nil
			}
		}
		if r.status == nil {
			return r.failure(key, trigger, errors.New("instance-manager status reader is not configured"))
		}
		status, err := r.status.Read(ctx, cluster.Namespace, pod.Name)
		if err != nil || !status.IsWalReceiverActive || !podReadiness(pod) {
			if err != nil {
				r.logger.Warn("instance-manager status verification retry", "namespace", cluster.Namespace, "cluster", cluster.Name, "error", err)
			}
			delay := r.retryDuration(key)
			if now.Add(delay).Before(pending.StageDeadline) {
				return ctrl.Result{RequeueAfter: delay}, nil
			}
			return r.replacePending(ctx, store, cluster, key, trigger)
		}
		pending.Stage = state.StageVerified
		pending.NextActionAt = nil
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		r.metricsPending(key, pending.Stage)
		fallthrough

	case state.StageVerified:
		if entry.CurrentLeaseID != "" && entry.CurrentLeaseID != pending.LeaseID {
			if err := r.revoke(ctx, entry.CurrentLeaseID); err != nil {
				return r.retryResult(key), err
			}
		}
		entry.CurrentLeaseID = pending.LeaseID
		expiry := pending.ExpiresAt
		entry.CurrentExpiresAt = &expiry
		entry.Pending = nil
		entry.LastEvent = pending.TriggerID
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		r.metrics.ClearPending(key)
		r.metrics.SetLease(cluster.Namespace, cluster.Name, expiry)
		if r.metrics != nil {
			r.metrics.RecordRotation("success", triggerKind(pending.TriggerID))
		}
		r.clearBackoff(key)
		r.logger.Info("replica credential rotation committed", "namespace", cluster.Namespace, "cluster", cluster.Name, "trigger", pending.TriggerID)
		return ctrl.Result{}, nil

	case state.StageReplacementBackoff:
		if pending.NextActionAt != nil && pending.NextActionAt.After(now) {
			return ctrl.Result{RequeueAfter: pending.NextActionAt.Sub(now)}, nil
		}
		if err := r.revoke(ctx, pending.LeaseID); err != nil {
			return r.retryResult(key), err
		}
		entry.Pending = nil
		store.Clusters[key] = entry
		if err := r.state.Save(ctx, store); err != nil {
			return ctrl.Result{}, err
		}
		return r.issue(ctx, store, cluster, key, trigger)
	default:
		return r.failure(key, trigger, fmt.Errorf("unsupported workflow stage %q", pending.Stage))
	}
}

func (r *Reconciler) replacePending(ctx context.Context, store state.Store, cluster *cnpg.Cluster, key, trigger string) (ctrl.Result, error) {
	entry := store.Clusters[key]
	if entry.Pending == nil {
		return r.issue(ctx, store, cluster, key, trigger)
	}
	now := r.clock.Now()
	entry.Pending.Stage = state.StageReplacementBackoff
	next := now.Add(r.retryDuration(key))
	entry.Pending.NextActionAt = &next
	store.Clusters[key] = entry
	if err := r.state.Save(ctx, store); err != nil {
		return ctrl.Result{}, err
	}
	r.metricsPending(key, state.StageReplacementBackoff)
	return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
}

func (r *Reconciler) cleanupEntry(ctx context.Context, key string, entry state.ClusterState) error {
	if entry.Pending != nil {
		if err := r.revoke(ctx, entry.Pending.LeaseID); err != nil {
			return err
		}
	}
	if entry.CurrentLeaseID != "" && (entry.Pending == nil || entry.CurrentLeaseID != entry.Pending.LeaseID) {
		if err := r.revoke(ctx, entry.CurrentLeaseID); err != nil {
			return err
		}
	}
	r.metrics.ClearPending(key)
	r.metrics.DeleteLease(stringsBeforeSlash(key), stringsAfterSlash(key))
	return nil
}

func (r *Reconciler) revoke(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return nil
	}
	if err := r.vault.RevokeLease(ctx, leaseID); err != nil {
		r.metricsVault("revoke", "failure")
		return err
	}
	r.metricsVault("revoke", "success")
	return nil
}

func (r *Reconciler) designatedPrimary(ctx context.Context, cluster *cnpg.Cluster) (*corev1.Pod, bool, error) {
	list := &corev1.PodList{}
	if err := r.client.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{"cnpg.io/cluster": cluster.Name}); err != nil {
		// Some released CNPG versions use the postgresql.cnpg.io label.
		list = &corev1.PodList{}
		if err := r.client.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{"postgresql.cnpg.io/cluster": cluster.Name}); err != nil {
			return nil, false, err
		}
	} else if len(list.Items) == 0 {
		fallback := &corev1.PodList{}
		if err := r.client.List(ctx, fallback, client.InNamespace(cluster.Namespace), client.MatchingLabels{"postgresql.cnpg.io/cluster": cluster.Name}); err != nil {
			return nil, false, err
		}
		if len(fallback.Items) > 0 {
			list = fallback
		}
	}
	var candidates []*corev1.Pod
	for i := range list.Items {
		pod := &list.Items[i]
		if cluster.Status.CurrentPrimary != "" && pod.Name == cluster.Status.CurrentPrimary {
			candidates = append(candidates, pod)
			continue
		}
		if cluster.Status.CurrentPrimary == "" && primaryLabel(pod) {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	for _, pod := range candidates {
		if podReadiness(pod) {
			return pod, true, nil
		}
	}
	return candidates[0], false, nil
}

func (r *Reconciler) Sweep(ctx context.Context) error {
	store, err := r.state.Load(ctx)
	if err != nil {
		return err
	}
	visible := map[string]types.UID{}
	for _, namespace := range r.cfg.WatchNamespaces {
		list := &cnpg.ClusterList{}
		var reader client.Reader = r.client
		if r.reader != nil {
			reader = r.reader
		}
		if err := reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return err
		}
		for i := range list.Items {
			visible[types.NamespacedName{Namespace: namespace, Name: list.Items[i].Name}.String()] = list.Items[i].UID
		}
	}
	changed := false
	for key, entry := range store.Clusters {
		namespace := stringsBeforeSlash(key)
		if !r.isWatched(namespace) {
			continue
		}
		uid, exists := visible[key]
		if exists {
			r.mu.Lock()
			delete(r.abstractAbsences, key)
			r.mu.Unlock()
			if string(uid) != entry.ClusterUID {
				if err := r.cleanupEntry(ctx, key, entry); err != nil {
					return err
				}
				delete(store.Clusters, key)
				changed = true
			}
			continue
		}
		r.mu.Lock()
		r.abstractAbsences[key]++
		absences := r.abstractAbsences[key]
		r.mu.Unlock()
		if absences < r.cfg.Workflow.OrphanAbsenceSweeps {
			continue
		}
		if err := r.cleanupEntry(ctx, key, entry); err != nil {
			return err
		}
		delete(store.Clusters, key)
		r.mu.Lock()
		delete(r.abstractAbsences, key)
		r.mu.Unlock()
		changed = true
	}
	if changed {
		return r.state.Save(ctx, store)
	}
	return nil
}

func (r *Reconciler) Start(ctx context.Context) error {
	interval := r.cfg.Workflow.OrphanSweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if err := r.Sweep(ctx); err != nil {
		r.logger.Error("initial orphan sweep failed", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Sweep(ctx); err != nil {
				r.logger.Error("orphan sweep failed", "error", err)
			}
		}
	}
}

func (r *Reconciler) syncMetrics(key string, entry state.ClusterState) {
	if entry.Pending != nil {
		r.metricsPending(key, entry.Pending.Stage)
	} else {
		r.metrics.ClearPending(key)
	}
	if entry.CurrentExpiresAt != nil {
		r.metrics.SetLease(stringsBeforeSlash(key), stringsAfterSlash(key), *entry.CurrentExpiresAt)
	}
}
func (r *Reconciler) metricsPending(key string, stage state.Stage) {
	if r.metrics != nil {
		r.metrics.SetPending(key, stage)
	}
}
func (r *Reconciler) metricsVault(operation, result string) {
	if r.metrics != nil {
		r.metrics.RecordVault(operation, result)
	}
}
func (r *Reconciler) failure(key, trigger string, err error) (ctrl.Result, error) {
	r.mu.Lock()
	_, seen := r.failed[key+"/"+trigger]
	r.failed[key+"/"+trigger] = struct{}{}
	r.mu.Unlock()
	if !seen && r.metrics != nil {
		r.metrics.RecordRotation("failure", triggerKind(trigger))
	}
	return r.retryResult(key), err
}
func (r *Reconciler) retryResult(key string) ctrl.Result {
	return ctrl.Result{RequeueAfter: r.retryDuration(key)}
}
func (r *Reconciler) retryDuration(key string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	attempt := r.backoff[key]
	r.backoff[key] = attempt + 1
	value := r.retryBase
	for i := 0; i < attempt && value < r.retryMax; i++ {
		value *= 2
	}
	if value > r.retryMax {
		value = r.retryMax
	}
	return value
}
func (r *Reconciler) nextAction(key string, now time.Time) *time.Time {
	next := now.Add(r.retryDuration(key))
	return &next
}
func (r *Reconciler) clearBackoff(key string) { r.mu.Lock(); delete(r.backoff, key); r.mu.Unlock() }
func (r *Reconciler) clearMemory(key string) {
	r.mu.Lock()
	delete(r.credentials, key)
	delete(r.backoff, key)
	delete(r.failed, key)
	r.mu.Unlock()
}
func (r *Reconciler) isWatched(namespace string) bool {
	for _, candidate := range r.cfg.WatchNamespaces {
		if candidate == namespace {
			return true
		}
	}
	return false
}
func stringsBeforeSlash(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return ""
}
func stringsAfterSlash(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[i+1:]
		}
	}
	return key
}
func triggerKind(trigger string) string {
	if len(trigger) >= len("pod-replacement") && contains(trigger, "pod-replacement") {
		return "pod_replacement"
	}
	if contains(trigger, "initialization") {
		return "initialization"
	}
	return "topology_change"
}
func stringsTrimInitialization(trigger string) string {
	const prefix = "initialization/"
	if len(trigger) > len(prefix) && trigger[:len(prefix)] == prefix {
		return trigger[len(prefix):]
	}
	return trigger
}
func contains(value, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

var _ client.Object = (*cnpg.Cluster)(nil)
