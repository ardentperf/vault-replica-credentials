package state

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestStateRoundTripAndMigration(t *testing.T) {
	expires := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	store := New()
	store.Clusters["reporting/replica"] = ClusterState{ClusterUID: "uid-1", CurrentLeaseID: "lease-old", CurrentExpiresAt: &expires,
		Pending: &PendingRotation{LeaseID: "lease-new", Username: "v-user", ExpiresAt: expires.Add(24 * time.Hour), Stage: StageIssued, StageDeadline: expires.Add(time.Hour), TriggerID: "pod/uid"}}
	raw, err := store.Marshal(256 * 1024)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "password") {
		t.Fatalf("state contains credential field: %s", raw)
	}
	got, err := Unmarshal(raw, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.Clusters["reporting/replica"].Pending.Username != "v-user" {
		t.Fatalf("round trip lost pending username")
	}
	legacy := strings.Replace(string(raw), `"version":1`, `"version":0`, 1)
	if migrated, err := Unmarshal([]byte(legacy), 256*1024); err != nil || migrated.Version != CurrentVersion {
		t.Fatalf("migration = %#v, %v", migrated, err)
	}
}

func TestStateRejectsUnknownFieldsAndOversizeDocuments(t *testing.T) {
	if _, err := Unmarshal([]byte(`{"version":1,"clusters":{},"password":"secret"}`), 0); err == nil {
		t.Fatal("unknown credential field was accepted")
	}
	store := New()
	store.Clusters["namespace/cluster"] = ClusterState{ClusterUID: strings.Repeat("x", 100)}
	if _, err := store.Marshal(16); err == nil {
		t.Fatal("oversize state was accepted")
	}
}

func TestStateRejectsMalformedIdentityAndMissingSecretData(t *testing.T) {
	store := New()
	store.Clusters["reporting/replica/extra"] = ClusterState{ClusterUID: "uid"}
	if _, err := store.Marshal(0); err == nil {
		t.Fatal("Marshal accepted an identity with multiple slashes")
	}
	secret := &corev1.Secret{Data: map[string][]byte{}}
	if _, err := LoadSecret(secret, "state.json", 1024); err == nil {
		t.Fatal("LoadSecret accepted a missing state key")
	}
}
