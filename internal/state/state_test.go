package state

import (
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeRejectsCredentialMaterial(t *testing.T) {
	expires := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	store := New()
	store.Clusters["reporting/replica"] = ClusterState{
		ClusterUID:       "cluster-uid",
		CurrentLeaseID:   "lease-1",
		CurrentExpiresAt: &expires,
		Pending: &PendingRotation{
			LeaseID: "lease-2", Username: "dynamic-user", ExpiresAt: expires,
			Stage: StageIssued, StageDeadline: expires, TriggerID: "trigger",
		},
	}
	raw, err := Encode(store, 1024)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatalf("state contains password field: %s", raw)
	}
	decoded, err := Decode(raw, 1024)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.Clusters["reporting/replica"].Pending.Username != "dynamic-user" {
		t.Fatal("pending state did not round-trip")
	}
	if _, err := Decode([]byte(`{"version":1,"clusters":{},"password":"secret"}`), 1024); err == nil {
		t.Fatal("Decode() accepted an unknown credential field")
	}
}

func TestDecodeValidatesSchemaAndSize(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"clusters":{}}`,
		`{"version":1,"clusters":null}`,
		`{"version":1,"clusters":{"bad":{"clusterUID":"uid"}}}`,
	} {
		if _, err := Decode([]byte(raw), 1024); err == nil {
			t.Fatalf("Decode(%s) error = nil", raw)
		}
	}
	if _, err := Decode([]byte(`{"version":1,"clusters":{}}`), 1); err == nil {
		t.Fatal("Decode() accepted payload above ceiling")
	}
}
