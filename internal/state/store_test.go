package state

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestJournalValidation(t *testing.T) {
	for _, raw := range []string{``, `{}`, `{"version":2,"clusters":{}}`, `{"version":1,"clusters":{},"password":"secret"}`, `{"version":1,"clusters":{"bad":{}}}`, `{"version":1,"clusters":{}} {}`} {
		if _, err := Decode([]byte(raw), 262144); err == nil {
			t.Fatalf("accepted invalid journal %q", raw)
		}
	}
	if _, err := Decode([]byte(`{"version":1,"clusters":{}}`), 5); err == nil {
		t.Fatal("size limit ignored")
	}
}

func TestJournalRoundTripAndNoop(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "cnpg-system"}, Data: map[string][]byte{"state.json": []byte(`{"version":1,"clusters":{}}`)}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	j := Journal{Client: c, Reader: c, Key: client.ObjectKeyFromObject(secret), DataKey: "state.json", MaxBytes: 262144}
	expires := time.Now().UTC().Add(time.Hour)
	s := ClusterState{ClusterUID: "uid", CurrentLeaseID: "lease", CurrentExpiresAt: &expires, LastEvent: "event"}
	if err := j.Put(context.Background(), "ns/db", nil, &s); err != nil {
		t.Fatal(err)
	}
	got, err := j.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Clusters["ns/db"].CurrentLeaseID != "lease" {
		t.Fatal("lost lease")
	}
	if err := j.Put(context.Background(), "ns/db", nil, &s); err == nil {
		t.Fatal("stale write accepted")
	}
	before := &corev1.Secret{}
	_ = c.Get(context.Background(), j.Key, before)
	if err := j.Put(context.Background(), "ns/db", &s, &s); err != nil {
		t.Fatal(err)
	}
	after := &corev1.Secret{}
	_ = c.Get(context.Background(), j.Key, after)
	if before.ResourceVersion != after.ResourceVersion {
		t.Fatal("no-op wrote state")
	}
	if strings.Contains(string(after.Data["state.json"]), "username") {
		t.Fatal("current username persisted")
	}
}
