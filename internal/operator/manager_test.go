package operator

import (
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ardentperf/vault-replica-credentials/internal/config"
)

func TestManagerConfigurationDefaultsAreSingleWorker(t *testing.T) {
	if config.DefaultWorkerCount != 1 {
		t.Fatalf("DefaultWorkerCount = %d, want 1", config.DefaultWorkerCount)
	}
	if config.SystemNamespace != "cnpg-system" {
		t.Fatalf("SystemNamespace = %q, want cnpg-system", config.SystemNamespace)
	}
}

func TestLeaderElectionDoesNotRequireSystemEventPermissions(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer s.Close()
	cfg := config.Config{SystemNamespace: config.SystemNamespace, Runtime: config.RuntimeConfig{LeaderElectionID: config.DefaultLeaderElectionID}}
	lock, err := newLeaseLock(cfg, &rest.Config{Host: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	lock.RecordEvent("became leader")
	if requests != 0 || lock.LockConfig.EventRecorder != nil {
		t.Fatal("leader attempted an unauthorized system Event")
	}
	if lock.LeaseMeta.Namespace != "cnpg-system" || lock.Identity() == "" {
		t.Fatal("unscoped or anonymous leader lock")
	}
}
