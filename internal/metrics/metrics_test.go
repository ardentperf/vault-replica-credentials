package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

func TestMetricsUseBoundedLabelsAndScrapeTimeLeaseValues(t *testing.T) {
	registry := prometheus.NewRegistry()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	m, err := New(registry, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	m.RecordRotation("success", "pod_replacement")
	m.RecordVault("issue", "success")
	m.SetPending("reporting/replica", state.StagePasswordPatched)
	m.SetLease("reporting", "replica", now.Add(time.Hour))
	first, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	text := gatherText(t, first)
	if !strings.Contains(text, `vault_replica_rotations_total{result="success",trigger="pod_replacement"} 1`) {
		t.Fatalf("rotation metric missing: %s", text)
	}
	if strings.Contains(text, "dynamic-user") || strings.Contains(text, "v-password") || strings.Contains(text, "lease-") {
		t.Fatalf("sensitive value in metrics: %s", text)
	}
	now = now.Add(30 * time.Second)
	second, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("metric gather unexpectedly empty")
	}
	m.ClearPending("reporting/replica")
	m.DeleteLease("reporting", "replica")
	text = gatherText(t, mustGather(registry))
	if strings.Contains(text, "vault_replica_current_lease_time_to_expiration_seconds") {
		t.Fatal("lease series was not cleaned up")
	}
}

func gatherText(t *testing.T, families []*dto.MetricFamily) string {
	t.Helper()
	var builder strings.Builder
	encoder := expfmt.NewEncoder(&builder, expfmt.FmtText)
	for _, family := range families {
		if err := encoder.Encode(family); err != nil {
			t.Fatal(err)
		}
	}
	return builder.String()
}
func mustGather(registry *prometheus.Registry) []*dto.MetricFamily {
	families, _ := registry.Gather()
	return families
}
