package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

func TestRotationPersistsThenPatchesAndCommits(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(cnpg.ClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: cnpg.ClusterGVK.Group, Version: cnpg.ClusterGVK.Version, Kind: "ClusterList"}, &unstructured.UnstructuredList{})
	cluster := testCluster()
	cluster.SetGroupVersionKind(cnpg.ClusterGVK)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replica-1", UID: "pod-uid"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replication"}, Data: map[string][]byte{"password": []byte("dummy")}}
	initial, err := state.Encode(state.New(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	stateSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: config.SystemNamespace, Name: config.DefaultStateSecretName}, Data: map[string][]byte{config.DefaultStateSecretKey: initial}}
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, pod, secret, stateSecret).Build()
	repository, err := state.NewRepository(api, config.SystemNamespace, config.DefaultStateSecretName, config.DefaultStateSecretKey, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	vaultClient := &testVault{credential: vault.Credential{Username: "dynamic-user", Password: "dynamic-password", LeaseID: "lease-new", ExpiresAt: now.Add(48 * time.Hour)}}
	reconciler, err := NewReconciler(testConfig(), Dependencies{Client: api, APIReader: api, State: repository, Vault: vaultClient, Status: alwaysActive{}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "replica"}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("issue reconciliation: %v", err)
	}
	if vaultClient.issues != 1 {
		t.Fatalf("issues = %d, want 1", vaultClient.issues)
	}
	assertStage(t, repository, state.StageIssued)
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("password patch reconciliation: %v", err)
	}
	assertStage(t, repository, state.StagePasswordPatched)
	storedSecret := &corev1.Secret{}
	if err := api.Get(ctx, client.ObjectKey{Namespace: "reporting", Name: "replication"}, storedSecret); err != nil {
		t.Fatal(err)
	}
	if string(storedSecret.Data["password"]) != "dynamic-password" {
		t.Fatalf("password = %q, want dynamic password", storedSecret.Data["password"])
	}

	now = now.Add(testConfig().Workflow.PasswordPropagationDelay)
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("username patch reconciliation: %v", err)
	}
	assertStage(t, repository, state.StageReconnectPending)
	storedCluster := cnpg.NewClusterObject()
	if err := api.Get(ctx, request.NamespacedName, storedCluster); err != nil {
		t.Fatal(err)
	}
	user, _, _ := nestedString(storedCluster.Object, "spec", "externalClusters", "0", "connectionParameters", "user")
	if user != "dynamic-user" {
		t.Fatalf("Cluster user = %q, want dynamic user", user)
	}

	now = now.Add(testConfig().Workflow.VerificationDelay)
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("verification reconciliation: %v", err)
	}
	assertStage(t, repository, state.StageVerified)
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("commit reconciliation: %v", err)
	}
	final, err := repository.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry := final.Clusters["reporting/replica"]
	if entry.Pending != nil || entry.CurrentLeaseID != "lease-new" {
		t.Fatalf("final state = %#v", entry)
	}
	if vaultClient.issues != 1 || len(vaultClient.revoked) != 0 {
		t.Fatalf("Vault calls: issues=%d revoked=%v", vaultClient.issues, vaultClient.revoked)
	}
}

func TestPromotionCleanupRetriesVaultFailureWithoutMutatingPrimary(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(cnpg.ClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: cnpg.ClusterGVK.Group, Version: cnpg.ClusterGVK.Version, Kind: "ClusterList"}, &unstructured.UnstructuredList{})
	cluster := testCluster()
	cluster.SetGroupVersionKind(cnpg.ClusterGVK)
	unstructured.RemoveNestedField(cluster.Object, "spec", "replica")
	expiresAt := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	encoded, err := state.Encode(state.Store{Version: state.CurrentVersion, Clusters: map[string]state.ClusterState{
		"reporting/replica": {
			ClusterUID:       "cluster-uid",
			CurrentLeaseID:   "old-lease",
			CurrentExpiresAt: &expiresAt,
		},
	}}, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	stateSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: config.SystemNamespace, Name: config.DefaultStateSecretName}, Data: map[string][]byte{config.DefaultStateSecretKey: encoded}}
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, stateSecret).Build()
	repository, err := state.NewRepository(api, config.SystemNamespace, config.DefaultStateSecretName, config.DefaultStateSecretKey, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	vaultClient := &testVault{revokeErr: errors.New("source is temporarily read-only")}
	reconciler, err := NewReconciler(testConfig(), Dependencies{Client: api, APIReader: api, State: repository, Vault: vaultClient, Status: alwaysActive{}})
	if err != nil {
		t.Fatal(err)
	}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "replica"}}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("cleanup reconciliation: %v", err)
	}
	if result.RequeueAfter != transientRequeue {
		t.Fatalf("requeue delay = %s, want %s", result.RequeueAfter, transientRequeue)
	}
	store, err := repository.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Clusters["reporting/replica"]; !ok {
		t.Fatal("cleanup state was removed after failed Vault revocation")
	}

	vaultClient.revokeErr = nil
	result, err = reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("successful cleanup reconciliation: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("successful cleanup result = %#v", result)
	}
	store, err = repository.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Clusters["reporting/replica"]; ok {
		t.Fatal("cleanup state remained after successful Vault revocation")
	}
	if got, want := vaultClient.revoked, []string{"old-lease", "old-lease"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("revoked leases = %v, want %v", got, want)
	}
}

func testCluster() *unstructured.Unstructured {
	cluster := cnpg.NewClusterObject()
	cluster.SetNamespace("reporting")
	cluster.SetName("replica")
	cluster.SetUID(types.UID("cluster-uid"))
	cluster.Object = map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"namespace": "reporting", "name": "replica", "uid": "cluster-uid"},
		"spec": map[string]any{
			"replica": map[string]any{"enabled": true, "source": "source-db"},
			"externalClusters": []any{map[string]any{
				"name": "source-db", "connectionParameters": map[string]any{"user": "initial-user"},
				"password": map[string]any{"name": "replication", "key": "password"},
			}},
		},
		"status": map[string]any{"targetPrimary": "replica-1"},
	}
	return cluster
}

func testConfig() config.Config {
	return config.Config{
		WatchNamespaces: []string{"reporting"}, SystemNamespace: config.SystemNamespace, StateSecretName: config.DefaultStateSecretName, StateSecretKey: config.DefaultStateSecretKey,
		Vault:    config.VaultConfig{SafetyMargin: time.Hour},
		Workflow: config.WorkflowConfig{PasswordPropagationDelay: time.Second, VerificationDelay: time.Second, IssueStageTimeout: time.Minute, PasswordUsernameTimeout: time.Minute, ReconnectStageTimeout: time.Minute, OrphanAbsenceSweeps: 2, StateMaxBytes: 1024 * 1024},
	}
}

func assertStage(t *testing.T, repository *state.Repository, want state.Stage) {
	t.Helper()
	store, err := repository.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entry := store.Clusters["reporting/replica"]
	if entry.Pending == nil || entry.Pending.Stage != want {
		t.Fatalf("stage = %#v, want %q", entry.Pending, want)
	}
}

type testVault struct {
	credential vault.Credential
	issues     int
	revoked    []string
	revokeErr  error
}

func (v *testVault) IssueDatabaseCredential(context.Context, string) (vault.Credential, error) {
	v.issues++
	return v.credential, nil
}

func (v *testVault) RevokeLease(_ context.Context, leaseID string) error {
	v.revoked = append(v.revoked, leaseID)
	return v.revokeErr
}

type alwaysActive struct{}

func (alwaysActive) WalReceiverActive(context.Context, string, string) (bool, error) {
	return true, nil
}

func nestedString(object map[string]any, fields ...string) (string, bool, error) {
	// Convert the one JSON array index used by the assertion into an ordinary
	// map lookup so the production adapter remains the code under test.
	raw, err := json.Marshal(object)
	if err != nil {
		return "", false, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", false, err
	}
	spec := decoded["spec"].(map[string]any)
	externals := spec["externalClusters"].([]any)
	connection := externals[0].(map[string]any)["connectionParameters"].(map[string]any)
	value, ok := connection["user"].(string)
	return value, ok, nil
}
