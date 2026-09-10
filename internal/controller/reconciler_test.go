package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	kubesetup "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
)

func TestReconcileRunsOrderedWorkflowAndDeduplicates(t *testing.T) {
	fixture := newReconcileFixture(t, emptyStore())
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}

	result, err := fixture.reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("first RequeueAfter = %s", result.RequeueAfter)
	}
	fixture.now = fixture.now.Add(5 * time.Second)
	result, err = fixture.reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Fatalf("second RequeueAfter = %s", result.RequeueAfter)
	}
	fixture.now = fixture.now.Add(2 * time.Minute)
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	wantOrder := []string{"issue:db01", "secret:credentials/password", "username:db02/0", "status:db02-1"}
	if fmt.Sprint(fixture.calls) != fmt.Sprint(wantOrder) {
		t.Fatalf("calls = %#v, want %#v", fixture.calls, wantOrder)
	}
	store, err := fixture.repository.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entry := store.Clusters["reporting/db02"]
	if entry.Pending != nil || entry.CurrentLeaseID != "lease-1" || entry.LastEvent == "" {
		t.Fatalf("committed entry = %#v", entry)
	}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(fixture.calls) != len(wantOrder) || fixture.vault.issues != 1 {
		t.Fatalf("duplicate observation caused work: calls=%#v issues=%d", fixture.calls, fixture.vault.issues)
	}
}

func TestReconcileRestartRevokesUnrecoverablePendingLeaseBeforeReplacement(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expires := now.Add(768 * time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "cluster-uid",
			Pending:    &state.PendingRotation{LeaseID: "abandoned", Username: "old-pending-user", ExpiresAt: expires, Stage: state.StageIssued, StageDeadline: now.Add(time.Minute), TriggerID: triggerForFixture()},
		},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fixture.vault.revoked) != "[abandoned]" || fixture.vault.issues != 1 {
		t.Fatalf("revoked=%#v issues=%d", fixture.vault.revoked, fixture.vault.issues)
	}
	if len(fixture.calls) < 3 || fixture.calls[0] != "revoke:abandoned" || fixture.calls[1] != "issue:db01" || fixture.calls[2] != "secret:credentials/password" {
		t.Fatalf("restart recovery order = %#v", fixture.calls)
	}
}

func TestReconcileMissingSecretRetainsLeaseAndRetriesSameCredential(t *testing.T) {
	fixture := newReconcileFixture(t, emptyStore())
	fixture.mutator.secretErrors = []error{kubesetup.ErrNotFound, nil}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("first reconcile error = nil, want waiting Secret retry")
	}
	store, _ := fixture.repository.Read(context.Background())
	if store.Clusters["reporting/db02"].Pending.Stage != state.StageWaitingForSecret {
		t.Fatalf("pending = %#v", store.Clusters["reporting/db02"].Pending)
	}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fixture.vault.issues != 1 {
		t.Fatalf("issues = %d, want retained credential", fixture.vault.issues)
	}
}

func TestUsernamePatchResolvesSelectedExternalFromLatestCluster(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expires := now.Add(768 * time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "cluster-uid",
			Pending: &state.PendingRotation{
				LeaseID: "pending", Username: "new-user", ExpiresAt: expires,
				Stage: state.StagePasswordPatched, StageDeadline: now.Add(time.Minute),
				TriggerID: triggerForFixture(),
			},
		},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	cluster := cnpg.NewCluster()
	key := types.NamespacedName{Namespace: "reporting", Name: "db02"}
	if err := fixture.client.Get(context.Background(), key, cluster); err != nil {
		t.Fatal(err)
	}
	externals, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "externalClusters")
	other := map[string]any{
		"name": "other", "connectionParameters": map[string]any{"host": "other", "user": "other"},
		"password": map[string]any{"name": "other-credentials", "key": "password"},
	}
	if err := unstructured.SetNestedSlice(cluster.Object, append([]any{other}, externals...), "spec", "externalClusters"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Fatalf("RequeueAfter = %s", result.RequeueAfter)
	}
	if fmt.Sprint(fixture.mutator.calls) != "[username:db02/1]" {
		t.Fatalf("username patch calls = %#v", fixture.mutator.calls)
	}
}

func TestVerificationFailureNeverRevokesCurrentLease(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expires := now.Add(768 * time.Hour)
	currentExpires := now.Add(700 * time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "cluster-uid", CurrentLeaseID: "current", CurrentExpiresAt: &currentExpires,
			Pending: &state.PendingRotation{LeaseID: "pending", Username: "new-user", ExpiresAt: expires, Stage: state.StageReconnectPending, StageDeadline: now.Add(5 * time.Minute), TriggerID: triggerForFixture()},
		},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	fixture.mutator.active = false
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("verification error = nil")
	}
	if len(fixture.vault.revoked) != 0 {
		t.Fatalf("verification failure revoked leases: %#v", fixture.vault.revoked)
	}
	got, _ := fixture.repository.Read(context.Background())
	if got.Clusters["reporting/db02"].CurrentLeaseID != "current" || got.Clusters["reporting/db02"].Pending.LeaseID != "pending" {
		t.Fatalf("verification failure changed lease state: %#v", got.Clusters["reporting/db02"])
	}
}

func TestStaleClusterIncarnationIsCleanedBeforeNewMutation(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {ClusterUID: "old-uid", CurrentLeaseID: "old-current", CurrentExpiresAt: &expires},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fixture.vault.revoked) != "[old-current]" || fixture.vault.issues != 0 {
		t.Fatalf("stale cleanup revoked=%#v issues=%d", fixture.vault.revoked, fixture.vault.issues)
	}
	if len(fixture.mutator.calls) != 0 {
		t.Fatalf("stale cleanup mutated new Cluster: %#v", fixture.mutator.calls)
	}
}

func TestBootstrapTransitionAndFirstPodAreOneRotationEpisode(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "cluster-uid", CurrentLeaseID: "current", CurrentExpiresAt: &expires,
			LastEvent: "cluster-uid/source/db01/primary/db02-1/transition/2026-09-08T12:00:00Z",
		},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fixture.vault.issues != 0 || len(fixture.mutator.calls) != 0 {
		t.Fatalf("first Pod duplicated bootstrap rotation: issues=%d calls=%#v", fixture.vault.issues, fixture.mutator.calls)
	}
	got, _ := fixture.repository.Read(context.Background())
	if !strings.Contains(got.Clusters["reporting/db02"].LastEvent, "/pod/") {
		t.Fatalf("lastEvent was not normalized to Pod trigger: %q", got.Clusters["reporting/db02"].LastEvent)
	}
}

func TestEstablishedReplicaWaitsForObservableReplacementPod(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "cluster-uid", CurrentLeaseID: "current", CurrentExpiresAt: &expires,
			LastEvent: triggerForFixture(),
		},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	if err := fixture.client.Delete(context.Background(), testPod()); err != nil {
		t.Fatal(err)
	}
	cluster := cnpg.NewCluster()
	key := types.NamespacedName{Namespace: "reporting", Name: "db02"}
	if err := fixture.client.Get(context.Background(), key, cluster); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(cluster.Object, "2026-09-09T02:00:00Z", "status", "targetPrimaryTimestamp"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != observationRetry {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, observationRetry)
	}
	if fixture.vault.issues != 0 || len(fixture.mutator.calls) != 0 {
		t.Fatalf("Pod deletion alone caused work: issues=%d calls=%#v", fixture.vault.issues, fixture.mutator.calls)
	}
}

func TestPromotionCleansBothLeasesWithoutCredentialMutation(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/db02": {
			ClusterUID: "cluster-uid", CurrentLeaseID: "current", CurrentExpiresAt: &expires,
			Pending: &state.PendingRotation{LeaseID: "pending", Username: "user", ExpiresAt: expires, Stage: state.StageVerified, StageDeadline: expires, TriggerID: "trigger"},
		},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	cluster := cnpg.NewCluster()
	if err := fixture.client.Get(context.Background(), types.NamespacedName{Namespace: "reporting", Name: "db02"}, cluster); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(cluster.Object, false, "spec", "replica", "enabled")
	if err := fixture.client.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "reporting", Name: "db02"}}
	if _, err := fixture.reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fixture.vault.revoked) != "[pending current]" {
		t.Fatalf("revoked = %#v", fixture.vault.revoked)
	}
	got, _ := fixture.repository.Read(context.Background())
	if _, exists := got.Clusters["reporting/db02"]; exists {
		t.Fatal("promoted Cluster state was retained")
	}
	if len(fixture.mutator.calls) != 0 {
		t.Fatalf("promotion mutated credentials: %#v", fixture.mutator.calls)
	}
}

func TestSweeperRequiresTwoFreshAbsencesAndIgnoresUnwatchedNamespace(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/deleted": {ClusterUID: "uid", CurrentLeaseID: "lease", CurrentExpiresAt: &expires},
		"unwatched/keep":    {ClusterUID: "uid2", CurrentLeaseID: "keep", CurrentExpiresAt: &expires},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	sweeper, err := NewSweeper(fixture.client, fixture.reconciler, []string{"reporting"}, time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.vault.revoked) != 0 {
		t.Fatal("first absence revoked a lease")
	}
	if err := sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(fixture.vault.revoked) != "[lease]" {
		t.Fatalf("revoked = %#v", fixture.vault.revoked)
	}
	got, _ := fixture.repository.Read(context.Background())
	if _, ok := got.Clusters["reporting/deleted"]; ok {
		t.Fatal("confirmed orphan remains")
	}
	if _, ok := got.Clusters["unwatched/keep"]; !ok {
		t.Fatal("unwatched namespace entry was cleaned")
	}
}

func TestSweeperKeepsRunningWhenVaultCleanupFails(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	store := state.Store{Version: 1, Clusters: map[string]state.ClusterState{
		"reporting/deleted": {ClusterUID: "uid", CurrentLeaseID: "lease", CurrentExpiresAt: &expires},
	}}
	fixture := newReconcileFixtureAt(t, store, now)
	fixture.vault.revokeErr = fmt.Errorf("temporary Vault outage")
	sweeper, err := NewSweeper(fixture.client, fixture.reconciler, []string{"reporting"}, 10*time.Millisecond, 2)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sweeper.Start(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("sweeper stopped on retryable cleanup error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sweeper shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sweeper did not stop after cancellation")
	}
	got, err := fixture.repository.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Clusters["reporting/deleted"]; !ok {
		t.Fatal("failed Vault cleanup removed durable state")
	}
}

func newReconcileFixture(t *testing.T, store state.Store) *fixtureRuntime {
	return newReconcileFixtureAt(t, store, time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
}

type fixtureRuntime struct {
	now        time.Time
	calls      []string
	client     client.Client
	reconciler *Reconciler
	repository *state.Repository
	vault      *fakeVault
	mutator    *fakeMutator
}

func newReconcileFixtureAt(t *testing.T, store state.Store, now time.Time) *fixtureRuntime {
	t.Helper()
	scheme, err := kubesetup.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := state.Encode(store, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "system"}, Data: map[string][]byte{"state.json": encoded}}
	cluster := testCluster()
	pod := testPod()
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, cluster, pod).Build()
	repository, err := state.NewRepository(kubeClient, types.NamespacedName{Namespace: "system", Name: "state"}, "state.json", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fixtureRuntime{now: now, repository: repository}
	runtime.client = kubeClient
	runtime.vault = &fakeVault{runtime: runtime}
	runtime.mutator = &fakeMutator{runtime: runtime, active: true}
	reconciler, err := NewReconciler(kubeClient, repository, runtime.vault, runtime.mutator, &fakeMetrics{}, testWorkflow(), time.Hour, func() time.Time { return runtime.now })
	if err != nil {
		t.Fatal(err)
	}
	runtime.reconciler = reconciler
	return runtime
}

func emptyStore() state.Store {
	return state.Store{Version: 1, Clusters: map[string]state.ClusterState{}}
}

func testWorkflow() config.WorkflowConfig {
	return config.WorkflowConfig{
		PasswordPropagationDelay: 5 * time.Second, VerificationDelay: 2 * time.Minute,
		IssueStageTimeout: time.Minute, PasswordUsernameTimeout: time.Minute,
		ReconnectStageTimeout: 5 * time.Minute, OrphanSweepInterval: time.Minute,
		OrphanAbsenceSweeps: 2, StateMaxBytes: 1 << 20,
	}
}

func testCluster() *unstructured.Unstructured {
	cluster := cnpg.NewCluster()
	cluster.SetNamespace("reporting")
	cluster.SetName("db02")
	cluster.SetUID(types.UID("cluster-uid"))
	cluster.Object["spec"] = map[string]any{
		"replica": map[string]any{"enabled": true, "source": "db01"},
		"externalClusters": []any{map[string]any{
			"name": "db01", "connectionParameters": map[string]any{"host": "source", "user": "dummy"},
			"password": map[string]any{"name": "credentials", "key": "password"},
		}},
	}
	cluster.Object["status"] = map[string]any{"currentPrimary": "db02-1", "targetPrimary": "db02-1"}
	return cluster
}

func testPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "db02-1", UID: types.UID("pod-uid"), Labels: map[string]string{cnpg.ClusterLabel: "db02"}}, Status: corev1.PodStatus{
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "postgres", RestartCount: 0}},
	}}
}

func triggerForFixture() string {
	observation, _ := cnpg.Observe(testCluster(), testPod())
	return observation.TriggerID()
}

type fakeVault struct {
	runtime   *fixtureRuntime
	issues    int
	revoked   []string
	issueErr  error
	revokeErr error
}

func (vaultClient *fakeVault) IssueDatabaseCredential(_ context.Context, role string) (vault.Credential, error) {
	vaultClient.runtime.calls = append(vaultClient.runtime.calls, "issue:"+role)
	if vaultClient.issueErr != nil {
		return vault.Credential{}, vaultClient.issueErr
	}
	vaultClient.issues++
	index := vaultClient.issues
	return vault.Credential{Username: fmt.Sprintf("user-%d", index), Password: fmt.Sprintf("password-%d", index), LeaseID: fmt.Sprintf("lease-%d", index), LeaseDuration: 768 * time.Hour, ExpiresAt: vaultClient.runtime.now.Add(768 * time.Hour)}, nil
}

func (vaultClient *fakeVault) RevokeLease(_ context.Context, leaseID string) error {
	vaultClient.runtime.calls = append(vaultClient.runtime.calls, "revoke:"+leaseID)
	if vaultClient.revokeErr != nil {
		return vaultClient.revokeErr
	}
	vaultClient.revoked = append(vaultClient.revoked, leaseID)
	return nil
}

type fakeMutator struct {
	runtime      *fixtureRuntime
	calls        []string
	secretErrors []error
	active       bool
}

func (mutator *fakeMutator) PatchSecretPassword(_ context.Context, _, name, key, _ string) error {
	call := "secret:" + name + "/" + key
	mutator.calls = append(mutator.calls, call)
	mutator.runtime.calls = append(mutator.runtime.calls, call)
	if len(mutator.secretErrors) != 0 {
		err := mutator.secretErrors[0]
		mutator.secretErrors = mutator.secretErrors[1:]
		return err
	}
	return nil
}

func (mutator *fakeMutator) PatchClusterUsername(_ context.Context, _, name string, index int, _ string) error {
	call := fmt.Sprintf("username:%s/%d", name, index)
	mutator.calls = append(mutator.calls, call)
	mutator.runtime.calls = append(mutator.runtime.calls, call)
	return nil
}

func (mutator *fakeMutator) WalReceiverActive(_ context.Context, _, pod string) (bool, error) {
	call := "status:" + pod
	mutator.calls = append(mutator.calls, call)
	mutator.runtime.calls = append(mutator.runtime.calls, call)
	return mutator.active, nil
}

type fakeMetrics struct{}

func (*fakeMetrics) RecordRotation(string, string) error { return nil }
func (*fakeMetrics) RecordVault(string, string) error    { return nil }
func (*fakeMetrics) SetState(state.Store)                {}
