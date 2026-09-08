package telemetry

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestScrapeTimeExpiryAndCleanup(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	m := New(func() time.Time { return now })
	reg := prometheus.NewRegistry()
	reg.MustRegister(m)
	m.Set(state.Store{Version: 1, Clusters: map[string]state.ClusterState{"ns/db": {CurrentLeaseID: "private-lease", CurrentExpiresAt: &expires, Pending: &state.PendingRotation{Stage: state.StageIssued, Username: "private-user"}}, "hidden/db": {CurrentLeaseID: "hidden-lease", CurrentExpiresAt: &expires}}}, []string{"ns"})
	m.Rotation("success", "initialization")
	m.Vault("issue", "success")
	scrape := func() string {
		r := httptest.NewRecorder()
		promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
		return r.Body.String()
	}
	first := scrape()
	if !strings.Contains(first, `{cluster="db",namespace="ns"} 3600`) {
		t.Fatal("missing expiry")
	}
	if strings.Contains(first, "private-") || strings.Contains(first, "hidden") {
		t.Fatal("sensitive/out-of-scope series")
	}
	now = now.Add(time.Second)
	if !strings.Contains(scrape(), `{cluster="db",namespace="ns"} 3599`) {
		t.Fatal("expiry does not decrease at scrape time")
	}
	m.Set(state.Store{Clusters: map[string]state.ClusterState{}}, []string{"ns"})
	if strings.Contains(scrape(), `cluster="db"`) {
		t.Fatal("stale series survived cleanup")
	}
}
