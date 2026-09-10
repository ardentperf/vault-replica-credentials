// Package operator builds the manager and registers the credential controller.
package operator

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appconfig "github.com/ardentperf/vault-replica-credentials/internal/config"
	"github.com/ardentperf/vault-replica-credentials/internal/controller"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	domainmetrics "github.com/ardentperf/vault-replica-credentials/internal/metrics"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

// NewManager creates the manager, scoped cache, controller watches, Vault
// client, health endpoints, metrics, and orphan sweep.
func NewManager(cfg appconfig.Config) (manager.Manager, error) {
	scheme, err := kubesetup.NewScheme()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
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
		MaxRetries: cfg.Vault.MaxRetries, RetryBackoff: cfg.Vault.RetryBackoff,
		AllowInsecureHTTP: cfg.Vault.AllowInsecureHTTP,
	})
	if err != nil {
		return nil, fmt.Errorf("create Vault client: %w", err)
	}
	statusReader, err := kubesetup.NewProxyStatusReader(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes status reader: %w", err)
	}
	domainMetrics, err := domainmetrics.New(ctrlmetrics.Registry, time.Now)
	if err != nil {
		return nil, fmt.Errorf("register controller metrics: %w", err)
	}
	stateStore := kubesetup.KubernetesStateStore{Client: mgr.GetClient(), Namespace: cfg.SystemNamespace, Name: cfg.StateSecretName, Key: cfg.StateSecretKey, MaxBytes: cfg.Workflow.StateMaxBytes}
	reconciler, err := controller.NewReconciler(controller.ReconcilerConfig{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), State: stateStore, Vault: vaultClient,
		Status: statusReader, Config: cfg, Metrics: domainMetrics,
	})
	if err != nil {
		return nil, fmt.Errorf("create reconciler: %w", err)
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setup reconciler: %w", err)
	}
	if err := mgr.Add(reconciler); err != nil {
		return nil, fmt.Errorf("add orphan sweep: %w", err)
	}
	return mgr, nil
}
