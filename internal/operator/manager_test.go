package operator

import (
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