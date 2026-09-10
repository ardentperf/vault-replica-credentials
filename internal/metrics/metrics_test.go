package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

func TestMetricsAreBoundedAndDoNotExposeCredentials(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	registry := prometheus.NewRegistry()
	metrics := New(registry, func() time.Time { return now })
	metrics.Rotation(RotationSuccess, "untrusted-trigger-value")
	metrics.VaultOperation(VaultIssue, RotationSuccess)
	expires := now.Add(time.Hour)
	store := state.New()
	store.Clusters["reporting/replica"] = state.ClusterState{
		ClusterUID: "uid", CurrentLeaseID: "sensitive-lease", CurrentExpiresAt: &expires,
		Pending: &state.PendingRotation{LeaseID: "pending-lease", Username: "sensitive-user", ExpiresAt: expires, Stage: state.StageIssued, StageDeadline: expires, TriggerID: "sensitive-trigger"},
	}
	metrics.ObserveState(store)
	text := ""
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range metricFamilies {
		text += family.String()
	}
	for _, sensitive := range []string{"sensitive-lease", "pending-lease", "sensitive-user", "sensitive-trigger"} {
		if strings.Contains(text, sensitive) {
			t.Fatalf("metric exposition leaks %q: %s", sensitive, text)
		}
	}
	if !strings.Contains(text, "name:\"namespace\"") || !strings.Contains(text, "reporting") || !strings.Contains(text, "name:\"cluster\"") || !strings.Contains(text, "replica") {
		t.Fatalf("lease labels missing from exposition: %s", text)
	}
	now = now.Add(10 * time.Minute)
	metricFamilies, err = registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var value float64
	for _, family := range metricFamilies {
		if family.GetName() == "vault_replica_current_lease_time_to_expiration_seconds" {
			value = family.Metric[0].GetGauge().GetValue()
		}
	}
	if value != 50*60 {
		t.Fatalf("lease value = %v, want 3000 without another state update", value)
	}
}
