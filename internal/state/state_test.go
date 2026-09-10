package state

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
)

func TestDecodeStrictSchemaAndMigration(t *testing.T) {
	store, err := Decode([]byte(`{"clusters":{}}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if store.Version != CurrentVersion || store.Clusters == nil {
		t.Fatalf("migration result = %#v", store)
	}

	for _, raw := range []string{
		`{"version":2,"clusters":{}}`,
		`{"version":1,"clusters":{},"password":"secret"}`,
		`{"version":1,"clusters":{}} trailing`,
	} {
		if _, err := Decode([]byte(raw), 1024); err == nil {
			t.Fatalf("Decode(%q) accepted invalid state", raw)
		}
	}
}

func TestEncodeEnforcesSizeAndLeaseRelationships(t *testing.T) {
	now := time.Now().UTC()
	store := Store{Version: CurrentVersion, Clusters: map[string]ClusterState{
		"reporting/db02": {ClusterUID: "uid", CurrentLeaseID: "lease", CurrentExpiresAt: &now},
	}}
	encoded, err := Encode(store, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password") {
		t.Fatalf("encoded state contains a password field: %s", encoded)
	}
	if _, err := Encode(store, 8); err == nil {
		t.Fatal("Encode() accepted an oversized journal")
	}
	broken := store
	entry := broken.Clusters["reporting/db02"]
	entry.CurrentExpiresAt = nil
	broken.Clusters["reporting/db02"] = entry
	if _, err := Encode(broken, 1024); err == nil {
		t.Fatal("Encode() accepted incomplete current lease metadata")
	}
}

func TestRepositoryReadsAndUpdatesPrecreatedSecret(t *testing.T) {
	scheme, err := kubesetup.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "system"},
		Data:       map[string][]byte{"state.json": []byte(`{"version":1,"clusters":{}}`)},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	repository, err := NewRepository(kubeClient, types.NamespacedName{Namespace: "system", Name: "state"}, "state.json", 4096)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.Mutate(context.Background(), func(store *Store) error {
		store.Clusters["reporting/db02"] = ClusterState{ClusterUID: "uid"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repository.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Clusters["reporting/db02"].ClusterUID != "uid" {
		t.Fatalf("state = %#v", got)
	}
}
