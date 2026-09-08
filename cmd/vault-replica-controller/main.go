package main

import (
	"flag"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ardentperf/vault-replica-credentials/internal/config"
	"github.com/ardentperf/vault-replica-credentials/internal/operator"
)

func main() {
	var zapOptions ctrlzap.Options
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseFlagOptions(&zapOptions)))
	log := ctrl.Log.WithName("vault-replica-controller")

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		log.Error(err, "invalid configuration")
		os.Exit(1)
	}
	log.Info("configuration loaded", "watchNamespaces", cfg.WatchNamespaces, "systemNamespace", cfg.SystemNamespace)
	log.Info("starting controller", "leaderElection", cfg.Runtime.LeaderElection)

	mgr, err := operator.NewManager(cfg)
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}
