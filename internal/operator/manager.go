// Package operator builds the bounded reconciliation manager used by the
// binary.
package operator

import (
	"fmt"
	"time"

	clientset "k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appconfig "github.com/ardentperf/vault-replica-credentials/internal/config"
	appcontroller "github.com/ardentperf/vault-replica-credentials/internal/controller"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	appmetrics "github.com/ardentperf/vault-replica-credentials/internal/metrics"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

// NewManager creates the manager, scoped cache, health endpoints and the
// controller. The manager's cache never includes target credential Secrets.
func NewManager(cfg appconfig.Config) (manager.Manager, error) {
	scheme, err := kubesetup.NewScheme()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes scheme: %w", err)
	}

	restConfig := ctrl.GetConfigOrDie()
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
	stateRepository, err := state.NewRepository(mgr.GetClient(), cfg.SystemNamespace, cfg.StateSecretName, cfg.StateSecretKey, cfg.Workflow.StateMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("create state repository: %w", err)
	}
	vaultClient, err := vault.NewHTTPClient(vault.Config{
		Address:        cfg.Vault.Address,
		Token:          cfg.Vault.Token,
		RequestTimeout: cfg.Vault.RequestTimeout,
		MaxRetries:     cfg.Vault.MaxRetries,
		RetryDelay:     cfg.Vault.RetryDelay,
		MinLease:       cfg.Vault.MinLease,
	})
	if err != nil {
		return nil, fmt.Errorf("create Vault client: %w", err)
	}
	clients, err := clientset.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes pod-proxy client: %w", err)
	}
	reconciler, err := appcontroller.NewReconciler(cfg, appcontroller.Dependencies{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		State:     stateRepository,
		Vault:     vaultClient,
		Status:    kubesetup.NewPodStatusReader(clients.CoreV1()),
		Metrics:   appmetrics.New(controllermetrics.Registry, time.Now),
	})
	if err != nil {
		return nil, fmt.Errorf("create reconciler: %w", err)
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("register reconciler: %w", err)
	}
	return mgr, nil
}
