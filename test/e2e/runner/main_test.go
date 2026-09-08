package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCredentialSecretReloadsPasswordOnlyChanges(t *testing.T) {
	raw, _ := json.Marshal(credentialSecret(&database{name: "db02", ns: "ns"}))
	var manifest map[string]any
	_ = json.Unmarshal(raw, &manifest)
	if str(manifest, "metadata", "labels", "cnpg.io/reload") != "true" {
		t.Fatal("CNPG must reload the passfile after a password-only Secret change")
	}
}

func TestReceiverInactiveRequiresExplicitBoolean(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"isWalReceiverActive":false}`, true},
		{`{"isWalReceiverActive": false}`, true},
		{`{"isWalReceiverActive":true}`, false},
		{`{"isWalReceiverActive":"false"}`, false},
		{`{"isWalReceiverActive":null}`, false},
		{`{}`, false},
		{`invalid`, false},
	} {
		if receiverInactive([]byte(tc.body)) != tc.want {
			t.Errorf("unexpected inactive observation for %s", tc.body)
		}
	}
}

func TestRestartMetricBaselineWaitsForRestoredSeries(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"data":{"result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"result":[{"value":[0,"100"]}]}}`))
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	a := &actor{ctx: context.Background(), http: server.Client(), ports: map[string]int{"us": port}, artifact: t.TempDir()}
	v, err := a.restartLeaseBaseline("us", "lease")
	if err != nil || v != 100 || calls != 2 {
		t.Fatal("restart must wait for leader-restored metric state")
	}
}

func TestInvalidPasswordRequiresSustainedInactiveReceiver(t *testing.T) {
	w := &inactiveWindow{}
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	if w.observe(true, now) {
		t.Fatal("a momentary disconnect is not failed authentication proof")
	}
	if w.observe(false, now.Add(6*time.Second)) {
		t.Fatal("an early successful reconnect must reset the observation")
	}
	if w.observe(true, now.Add(7*time.Second)) {
		t.Fatal("the inactivity window was not reset")
	}
	if !w.observe(true, now.Add(13*time.Second)) {
		t.Fatal("sustained inactive receiver was not accepted")
	}
}

func TestVaultConfigurationRetriesTemporaryConnectionFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":["connection reset during source restart"]}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	a := &actor{ctx: context.Background(), address: server.URL, http: server.Client()}
	if err := a.configureVault("db", &database{host: "gateway", port: "5432"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("idempotent Vault configuration did not retry the restart race")
	}
}

func TestArtifactRedaction(t *testing.T) {
	input := `{"user":"v-token-db01-sensitive","password":"anything","lease_id":"database/creds/secret","lastPromotionToken":"private-promotion-token"} Root Token: e2e-dev-root-token`
	got := redact(input)
	for _, secret := range []string{"v-token-db01-sensitive", "anything", "database/creds/secret", "e2e-dev-root-token", "private-promotion-token"} {
		if strings.Contains(got, secret) {
			t.Fatal("artifact leaks credential")
		}
	}
}

func TestGatewayPortsRemainStableAcrossAllocation(t *testing.T) {
	d := &database{port: "15433"}
	if gatewayHealthPort(d) != "17433" {
		t.Fatal("health port must be derived from the database's allocated port")
	}
}

func TestConnectivityProbeCanScheduleWithTaintedWorkers(t *testing.T) {
	if !strings.Contains(probeOverrides(), `"node-role.kubernetes.io/control-plane"`) {
		t.Fatal("probe lacks control-plane placement")
	}
	if !strings.Contains(probeOverrides(), `"tolerations"`) {
		t.Fatal("probe cannot schedule on a tainted control plane")
	}
}

func TestDistributedTopologyIncludesItsSelfReference(t *testing.T) {
	a := &actor{}
	source := &database{name: "db01", region: "us", ns: "ns"}
	replica := &database{name: "db02", region: "eu", ns: "ns"}
	for _, tc := range []struct{ d, source *database }{{source, nil}, {replica, source}} {
		manifest := a.clusterManifest(tc.d, tc.source)
		entries := nested(manifest, "spec", "externalClusters").([]any)
		found := false
		for _, e := range entries {
			if e.(map[string]any)["name"] == tc.d.name {
				found = true
				if e.(map[string]any)["connectionParameters"] == nil {
					t.Fatal("CNPG external entry requires connection parameters")
				}
			}
		}
		if !found {
			t.Fatal("CNPG rejects distributed topology without a self externalCluster")
		}
	}
}

func TestCNPGNamespaceConfigurationUsesSupportedConfigMap(t *testing.T) {
	cfg := cnpgConfig("e2e-bootstrap,e2e-first")
	if str(cfg, "metadata", "name") != "cnpg-controller-manager-config" || str(cfg, "data", "WATCH_NAMESPACE") != "e2e-bootstrap,e2e-first" {
		t.Fatal("CNPG namespace scope must be explicitly configured, not assumed from the plugin render flag")
	}
}

func TestReplicationReadinessDoesNotWaitForDrainedLocalVolume(t *testing.T) {
	pod := map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}
	if !primaryReady(pod) {
		t.Fatal("a Ready designated primary is sufficient to verify streaming during drain")
	}
	pod["metadata"] = map[string]any{"deletionTimestamp": "2026-09-08T00:00:00Z"}
	if primaryReady(pod) {
		t.Fatal("a deleting primary must not pass readiness")
	}
}
