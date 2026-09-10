// Package operator wires the scoped manager and credential reconciler.
package operator

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	appconfig "github.com/ardentperf/vault-replica-credentials/internal/config"
	credentialcontroller "github.com/ardentperf/vault-replica-credentials/internal/controller"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	domainmetrics "github.com/ardentperf/vault-replica-credentials/internal/metrics"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
	"k8s.io/apimachinery/pkg/types"
)

// NewManager creates the manager against the current Kubernetes REST config.
func NewManager(cfg appconfig.Config) (manager.Manager, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes REST configuration: %w", err)
	}
	return NewManagerForConfig(cfg, restConfig)
}

// NewManagerForConfig creates and fully wires a manager. It is exposed for
// envtest and focused integration tests.
func NewManagerForConfig(cfg appconfig.Config, restConfig *rest.Config) (manager.Manager, error) {
	scheme, err := kubesetup.NewScheme()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		Cache:                   kubesetup.CacheOptions(cfg),
		Metrics:                 server.Options{BindAddress: cfg.Runtime.MetricsBindAddress},
		HealthProbeBindAddress:  cfg.Runtime.HealthProbeBindAddress,
		LeaderElection:          cfg.Runtime.LeaderElection,
		LeaderElectionID:        cfg.Runtime.LeaderElectionID,
		LeaderElectionNamespace: cfg.SystemNamespace,
	})
	if err != nil {
		return nil, fmt.Errorf("create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("register health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("register readiness check: %w", err)
	}

	vaultClient, err := vault.NewHTTPClient(vault.HTTPConfig{
		Address: cfg.Vault.Address, Token: cfg.Vault.Token, MinLease: cfg.Vault.MinLease,
		SafetyMargin: cfg.Vault.SafetyMargin, RequestTimeout: cfg.Vault.RequestTimeout,
		RetryAttempts: cfg.Vault.RetryAttempts, RetryBackoff: cfg.Vault.RetryBackoff,
	})
	if err != nil {
		return nil, fmt.Errorf("create Vault client: %w", err)
	}
	api, err := kubesetup.NewAPI(restConfig)
	if err != nil {
		return nil, err
	}
	repository, err := state.NewRepository(mgr.GetClient(), types.NamespacedName{Namespace: cfg.SystemNamespace, Name: cfg.StateSecretName}, cfg.StateSecretKey, cfg.Workflow.StateMaxBytes)
	if err != nil {
		return nil, err
	}
	recorder, err := domainmetrics.New(controllermetrics.Registry, nil)
	if err != nil {
		return nil, fmt.Errorf("register domain metrics: %w", err)
	}
	reconciler, err := credentialcontroller.NewReconciler(mgr.GetClient(), repository, vaultClient, api, recorder, cfg.Workflow, cfg.Vault.SafetyMargin, nil)
	if err != nil {
		return nil, err
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("vault-replica-credentials").
		For(cnpg.NewCluster(), builder.WithPredicates(credentialcontroller.ClusterPredicate())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(credentialcontroller.MapPodToCluster), builder.WithPredicates(credentialcontroller.PodPredicate())).
		WithOptions(controlleroptions.Options{MaxConcurrentReconciles: cfg.Runtime.Workers}).
		Complete(reconciler); err != nil {
		return nil, fmt.Errorf("register credential controller: %w", err)
	}
	sweeper, err := credentialcontroller.NewSweeper(mgr.GetAPIReader(), reconciler, cfg.WatchNamespaces, cfg.Workflow.OrphanSweepInterval, cfg.Workflow.OrphanAbsenceSweeps)
	if err != nil {
		return nil, err
	}
	if err := mgr.Add(sweeper); err != nil {
		return nil, fmt.Errorf("register orphan sweeper: %w", err)
	}
	return mgr, nil
}
