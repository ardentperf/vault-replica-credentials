package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

func TestMetricsAreBoundedRedactedAndCalculatedAtScrapeTime(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	registry := prometheus.NewPedanticRegistry()
	recorder, err := New(registry, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordRotation("success", "initialization"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordVault("issue", "success"); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Hour)
	recorder.SetState(state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "pod-uid-sensitive", CurrentLeaseID: "lease-sensitive", CurrentExpiresAt: &expires,
			Pending: &state.PendingRotation{LeaseID: "pending-sensitive", Username: "username-sensitive", ExpiresAt: expires, Stage: state.StageReconnectPending, StageDeadline: expires, TriggerID: "event-sensitive"},
		},
	}})
	first := exposition(t, registry)
	if !strings.Contains(first, `vault_replica_current_lease_time_to_expiration_seconds{cluster="db02",namespace="reporting"} 3600`) {
		t.Fatalf("lease metric is missing or inaccurate:\n%s", first)
	}
	for _, forbidden := range []string{"lease-sensitive", "pending-sensitive", "username-sensitive", "pod-uid-sensitive", "event-sensitive"} {
		if strings.Contains(first, forbidden) {
			t.Fatalf("metrics leaked %q", forbidden)
		}
	}
	now = now.Add(10 * time.Second)
	second := exposition(t, registry)
	if !strings.Contains(second, `vault_replica_current_lease_time_to_expiration_seconds{cluster="db02",namespace="reporting"} 3590`) {
		t.Fatalf("lease metric did not decrease at scrape time:\n%s", second)
	}
	recorder.SetState(state.Store{Version: 1, Clusters: map[string]state.ClusterState{}})
	if strings.Contains(exposition(t, registry), `cluster="db02"`) {
		t.Fatal("cleaned Cluster lease series remains exposed")
	}
}

func TestMetricsRejectUnboundedLabels(t *testing.T) {
	recorder, err := New(prometheus.NewRegistry(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordRotation("success", "pod-uid-sensitive"); err == nil {
		t.Fatal("accepted arbitrary trigger label")
	}
	if err := recorder.RecordVault("lease-sensitive", "failure"); err == nil {
		t.Fatal("accepted arbitrary operation label")
	}
}

func exposition(t *testing.T, gatherer prometheus.Gatherer) string {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	encoder := expfmt.NewEncoder(&output, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, family := range families {
		if err := encoder.Encode(family); err != nil {
			t.Fatal(err)
		}
	}
	return output.String()
}
