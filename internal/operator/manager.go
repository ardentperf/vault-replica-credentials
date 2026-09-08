// Package operator builds the regional controller manager used by the binary.
package operator

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	appconfig "github.com/ardentperf/vault-replica-credentials/internal/config"
	workflow "github.com/ardentperf/vault-replica-credentials/internal/controller"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/telemetry"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

// NewManager wires scoped watches, the durable journal, Vault and health checks.
func NewManager(cfg appconfig.Config) (manager.Manager, error) {
	scheme, err := kubesetup.NewScheme()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes scheme: %w", err)
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	lock, err := newLeaseLock(cfg, restConfig)
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                              scheme,
		Cache:                               kubesetup.CacheOptions(cfg),
		Metrics:                             server.Options{BindAddress: cfg.Runtime.MetricsBindAddress},
		HealthProbeBindAddress:              cfg.Runtime.HealthProbeBindAddress,
		LeaderElection:                      cfg.Runtime.LeaderElection,
		LeaderElectionID:                    cfg.Runtime.LeaderElectionID,
		LeaderElectionNamespace:             cfg.SystemNamespace,
		LeaderElectionResourceLockInterface: lock,
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("register health check: %w", err)
	}
	journal := &state.Journal{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Key: client.ObjectKey{Namespace: cfg.SystemNamespace, Name: cfg.StateSecretName}, DataKey: cfg.StateSecretKey, MaxBytes: cfg.Workflow.StateMaxBytes}
	api, err := kubesetup.NewAPI(mgr.GetConfig())
	if err != nil {
		return nil, err
	}
	domainMetrics := telemetry.New(time.Now)
	metrics.Registry.MustRegister(domainMetrics)
	r := workflow.New(mgr.GetClient(), mgr.GetAPIReader(), journal, api, vault.New(cfg.Vault), cfg, domainMetrics)
	if err := ctrl.NewControllerManagedBy(mgr).Named("vault_replica_credentials").
		For(cnpg.NewCluster(), builder.WithPredicates(cnpg.ClusterPredicate())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			name := o.GetLabels()["cnpg.io/cluster"]
			if name == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: name}}}
		}), builder.WithPredicates(cnpg.PodPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r); err != nil {
		return nil, err
	}
	if err := mgr.Add(r); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()
		_, err := journal.Load(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("register readiness check: %w", err)
	}
	return mgr, nil
}

// The ordinary client-go Lease lock supports an optional Event recorder.
// Omit that recorder: the system Role intentionally grants only state and
// Lease operations. Leader-election logs and standard metrics remain enabled.
func newLeaseLock(cfg appconfig.Config, restConfig *rest.Config) (*resourcelock.LeaseLock, error) {
	c, err := coordinationclient.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	return &resourcelock.LeaseLock{LeaseMeta: metav1.ObjectMeta{Namespace: cfg.SystemNamespace, Name: cfg.Runtime.LeaderElectionID}, Client: c, LockConfig: resourcelock.ResourceLockConfig{Identity: hostname + "_" + string(uuid.NewUUID())}}, nil
}
