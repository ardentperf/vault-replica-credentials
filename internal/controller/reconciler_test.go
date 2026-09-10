package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type memoryState struct{ value state.Store }

func (s *memoryState) Load(context.Context) (state.Store, error)       { return s.value, nil }
func (s *memoryState) Save(_ context.Context, value state.Store) error { s.value = value; return nil }

type fakeVault struct {
	issued  int
	revoked []string
}

func (v *fakeVault) IssueDatabaseCredential(context.Context, string) (vault.Credential, error) {
	v.issued++
	return vault.Credential{Username: "dynamic-user", Password: "dynamic-password", LeaseID: "lease-new", LeaseDuration: 24 * time.Hour, ExpiresAt: time.Now().Add(24 * time.Hour)}, nil
}
func (v *fakeVault) RevokeLease(_ context.Context, lease string) error {
	v.revoked = append(v.revoked, lease)
	return nil
}
func TestReconcilerRunsOrderedRotationAndCommitsOnlyAfterVerification(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = cnpg.AddToScheme(scheme)
	cluster := testCluster()
	cluster.Status.CurrentPrimary = "replica-1"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replica-credentials"}, Data: map[string][]byte{"password": []byte("dummy")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replica-1", UID: types.UID("pod-1"), Labels: map[string]string{"cnpg.io/cluster": "replica"}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, secret, pod).Build()
	store := &memoryState{value: state.New()}
	fakeVaultClient := &fakeVault{}
	clock := &testClock{now: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)}
	r, err := NewReconciler(ReconcilerConfig{Client: client, State: store, Vault: fakeVaultClient, Config: config.Config{WatchNamespaces: []string{"reporting"}, Vault: config.VaultConfig{SafetyMargin: time.Hour}, Workflow: config.WorkflowConfig{PasswordPropagationDelay: 5 * time.Second, VerificationDelay: 2 * time.Minute, IssueStageTimeout: time.Minute, PasswordUsernameTimeout: time.Minute, ReconnectStageTimeout: 5 * time.Minute, OrphanAbsenceSweeps: 2}}, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	// The production status interface uses kubernetes.InstanceStatus; install a
	// compatible adapter after construction so no PostgreSQL connection exists.
	r.status = instanceStatusFunc(func(context.Context, string, string) (kubesetup.InstanceStatus, error) {
		return kubesetup.InstanceStatus{IsWalReceiverActive: true}, nil
	})

	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "replica"}}
	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 5*time.Second || fakeVaultClient.issued != 1 {
		t.Fatalf("first reconcile result=%#v issued=%d", result, fakeVaultClient.issued)
	}
	if string(secret.Data["password"]) == "dynamic-password" {
		t.Fatal("fake object was not refreshed after patch")
	}
	clock.Advance(5 * time.Second)
	if _, err = r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	updated := &cnpg.Cluster{}
	if err := client.Get(context.Background(), request.NamespacedName, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.ExternalClusters[0].ConnectionParameters["user"] != "dynamic-user" {
		t.Fatalf("username = %q", updated.Spec.ExternalClusters[0].ConnectionParameters["user"])
	}
	clock.Advance(2 * time.Minute)
	if _, err = r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fakeVaultClient.issued != 1 || store.value.Clusters[request.NamespacedName.String()].CurrentLeaseID != "lease-new" || store.value.Clusters[request.NamespacedName.String()].Pending != nil {
		t.Fatalf("state after commit = %#v", store.value)
	}
	if _, err = r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fakeVaultClient.issued != 1 {
		t.Fatalf("duplicate stable observation issued %d credentials", fakeVaultClient.issued)
	}
}

type instanceStatusFunc func(context.Context, string, string) (kubesetup.InstanceStatus, error)

func (f instanceStatusFunc) Read(ctx context.Context, ns, pod string) (kubesetup.InstanceStatus, error) {
	return f(ctx, ns, pod)
}
