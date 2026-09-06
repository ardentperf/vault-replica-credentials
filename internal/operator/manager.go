// Package operator builds the non-reconciling manager used by the binary.
package operator

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appconfig "github.com/ardentperf/vault-replica-credentials/internal/config"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
)

// NewManager creates the manager and its scoped cache. It intentionally does
// not register a controller, watches, Vault client, or mutation handler yet.
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
	return mgr, nil
}
