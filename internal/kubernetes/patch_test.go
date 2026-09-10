package kubernetes

import (
	"context"
	"encoding/base64"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

func TestSecretPatcherUsesOnlyPasswordData(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "target"}, Data: map[string][]byte{"password": []byte("old"), "unrelated": []byte("preserve")}}).Build()
	if err := (ClientSecretPatcher{Client: client}).PatchPassword(context.Background(), "reporting", "target", "password", "new-password"); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "reporting", Name: "target"}, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["password"]) != "new-password" || string(secret.Data["unrelated"]) != "preserve" {
		t.Fatalf("patched Secret data = %#v", secret.Data)
	}
	if got := base64.StdEncoding.EncodeToString([]byte("new-password")); got == string(secret.Data["password"]) {
		t.Fatal("client Secret stores encoded payload instead of decoded data")
	}
}

func TestPatchClusterUsernameChangesSelectedExternalClusterOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = cnpg.AddToScheme(scheme)
	cluster := &cnpg.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replica"}, Spec: cnpg.ClusterSpec{ExternalClusters: []cnpg.ExternalCluster{{Name: "other", ConnectionParameters: map[string]string{"user": "keep"}}, {Name: "source", ConnectionParameters: map[string]string{"host": "keep-host", "user": "old"}}}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	if err := PatchClusterUsername(context.Background(), client, cluster, "source", "new"); err != nil {
		t.Fatal(err)
	}
	got := &cnpg.Cluster{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "reporting", Name: "replica"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.ExternalClusters[0].ConnectionParameters["user"] != "keep" || got.Spec.ExternalClusters[1].ConnectionParameters["user"] != "new" || got.Spec.ExternalClusters[1].ConnectionParameters["host"] != "keep-host" {
		t.Fatalf("Cluster = %#v", got.Spec)
	}
}
