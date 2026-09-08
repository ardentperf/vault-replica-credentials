package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
	kube "github.com/ardentperf/vault-replica-credentials/internal/kubernetes"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/ardentperf/vault-replica-credentials/internal/telemetry"
	"github.com/ardentperf/vault-replica-credentials/internal/vault"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type fakeVault struct {
	now             *time.Time
	issued, revoked int
	revokeFail      bool
	rejectIssue     bool
}

func (v *fakeVault) IssueDatabaseCredential(context.Context, string) (vault.Credential, error) {
	v.issued++
	if v.rejectIssue {
		return vault.Credential{LeaseID: "rejected-lease", Username: "rejected-user", ExpiresAt: v.now.Add(time.Hour)}, errors.New("lease rejected")
	}
	return vault.Credential{LeaseID: time.Duration(v.issued).String(), Username: "private-user", Password: "private-password", ExpiresAt: v.now.Add(768 * time.Hour)}, nil
}

func TestRejectedCredentialLeaseIsJournaledForCleanup(t *testing.T) {
	f := newFixture(t)
	f.v.rejectIssue = true
	f.step(t)
	s := f.entry(t)
	if s.Pending == nil || s.Pending.LeaseID != "rejected-lease" || s.Pending.Stage != state.StageReplacementBackoff {
		t.Fatal("known rejected lease lost instead of journaled")
	}
	if f.api.passwordPatches != 0 {
		t.Fatal("rejected credential patched")
	}
	f.v.rejectIssue = false
	f.finish(t)
	if f.v.revoked != 1 {
		t.Fatal("rejected lease was not revoked before retry")
	}
}
func (v *fakeVault) RevokeLease(context.Context, string) error {
	if v.revokeFail {
		return errors.New("private-error")
	}
	v.revoked++
	return nil
}

type fakeAPI struct {
	c                            client.Client
	journal                      *state.Journal
	passwordPatches, userPatches int
	missing, active              bool
}

func (a *fakeAPI) PatchPassword(ctx context.Context, o cnpg.Observation, password string) error {
	s, _ := a.journal.Load(ctx)
	if s.Clusters["ns/db"].Pending == nil {
		panic("password patched before pending persistence")
	}
	if a.missing {
		return kube.ErrNotFound
	}
	a.passwordPatches++
	return nil
}
func (a *fakeAPI) PatchUsername(ctx context.Context, c *unstructured.Unstructured, o cnpg.Observation, user string) error {
	a.userPatches++
	entries, _, _ := unstructured.NestedSlice(c.Object, "spec", "externalClusters")
	entries[o.ExternalIndex].(map[string]any)["connectionParameters"].(map[string]any)["user"] = user
	_ = unstructured.SetNestedSlice(c.Object, entries, "spec", "externalClusters")
	return a.c.Update(ctx, c)
}
func (a *fakeAPI) WALReceiver(context.Context, string, string) (bool, error) { return a.active, nil }

type fixture struct {
	r   *Reconciler
	v   *fakeVault
	api *fakeAPI
	now time.Time
	key ctrl.Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), key: ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ns", Name: "db"}}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := cnpg.NewCluster()
	c.SetName("db")
	c.SetNamespace("ns")
	c.SetUID("uid")
	c.Object["spec"] = map[string]any{"replica": map[string]any{"enabled": true, "source": "source"}, "externalClusters": []any{map[string]any{"name": "source", "connectionParameters": map[string]any{"user": "dummy"}, "password": map[string]any{"name": "credential", "key": "password"}}}}
	c.Object["status"] = map[string]any{"currentPrimary": "db-1", "targetPrimary": "db-1"}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "ns", UID: "pod-uid", Labels: map[string]string{"cnpg.io/cluster": "db"}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "state", Namespace: "cnpg-system"}, Data: map[string][]byte{"state.json": []byte(`{"version":1,"clusters":{}}`)}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, p, s).Build()
	j := &state.Journal{Client: cl, Reader: cl, Key: client.ObjectKeyFromObject(s), DataKey: "state.json", MaxBytes: 262144}
	cfg, err := config.Load(func(k string) (string, bool) {
		v, ok := map[string]string{"WATCH_NAMESPACE": "ns", "VAULT_ADDR": "https://vault.example", "VAULT_TOKEN": "private-token"}[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	f.v = &fakeVault{now: &f.now}
	f.api = &fakeAPI{c: cl, journal: j, active: true}
	f.r = New(cl, cl, j, f.api, f.v, cfg, telemetry.New(func() time.Time { return f.now }))
	f.r.Now = func() time.Time { return f.now }
	return f
}
func (f *fixture) step(t *testing.T) ctrl.Result {
	t.Helper()
	r, err := f.r.Reconcile(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *fixture) entry(t *testing.T) state.ClusterState {
	t.Helper()
	s, err := f.r.Journal.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.Clusters["ns/db"]
}
func (f *fixture) finish(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		r := f.step(t)
		s := f.entry(t)
		if s.CurrentLeaseID != "" && s.Pending == nil {
			return
		}
		f.now = f.now.Add(r.RequeueAfter)
	}
	t.Fatal("workflow did not finish")
}

func TestOrderedRotationAndDuplicateObservation(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	if f.v.issued != 1 || f.api.passwordPatches != 1 || f.api.userPatches != 1 {
		t.Fatal("rotation side effects not exactly once")
	}
	for i := 0; i < 5; i++ {
		f.step(t)
	}
	if f.v.issued != 1 {
		t.Fatal("duplicate observation issued lease")
	}
}
func TestRestartAfterPasswordPatchHonorsDelay(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4 && f.entry(t).Pending == nil; i++ {
		f.step(t)
	}
	for f.entry(t).Pending.Stage != state.StagePasswordPatched {
		f.step(t)
	}
	p := f.entry(t).Pending
	f.r.passwords = map[string]string{}
	f.step(t)
	if f.api.userPatches != 0 {
		t.Fatal("restart skipped durable propagation delay")
	}
	f.now = *p.NextActionAt
	f.finish(t)
	if f.v.issued != 1 || f.api.passwordPatches != 1 {
		t.Fatal("restart repeated issuance/password patch")
	}
}
func TestMissingSecretAndLostPasswordRecovery(t *testing.T) {
	f := newFixture(t)
	f.api.missing = true
	for i := 0; i < 4; i++ {
		f.step(t)
	}
	if f.v.issued != 1 || f.entry(t).Pending.Stage != state.StageWaitingForSecret {
		t.Fatal("missing Secret issued duplicates")
	}
	f.r.passwords = map[string]string{}
	f.api.missing = false
	f.finish(t)
	if f.v.issued != 2 || f.v.revoked != 1 {
		t.Fatal("lost password did not replace tracked lease")
	}
}
func TestVerificationNeverUsesReadyAlone(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	// Change the primary's restart count to create a new qualifying episode.
	p := &corev1.Pod{}
	_ = f.r.Reader.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "db-1"}, p)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", RestartCount: 1}}
	_ = f.r.Client.Status().Update(context.Background(), p)
	f.api.active = false
	for i := 0; i < 10; i++ {
		r := f.step(t)
		f.now = f.now.Add(r.RequeueAfter)
	}
	if f.v.revoked != 0 || f.entry(t).Pending == nil || f.entry(t).Pending.Stage != state.StageReconnectPending {
		t.Fatal("inactive WAL receiver committed or revoked")
	}
	f.api.active = true
	f.finish(t)
	if f.v.revoked != 1 {
		t.Fatal("old lease not revoked after verification")
	}
}

func TestMutationRecoveryAcrossJournalWriteFailures(t *testing.T) {
	for _, stage := range []state.Stage{state.StagePasswordPatched, state.StageReconnectPending, state.StageVerified} {
		t.Run(string(stage), func(t *testing.T) {
			f := newFixture(t)
			failed := false
			f.r.Journal.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				secret := obj.(*corev1.Secret)
				journal, err := state.Decode(secret.Data["state.json"], 262144)
				if err != nil {
					return err
				}
				p := journal.Clusters["ns/db"].Pending
				if !failed && p != nil && p.Stage == stage {
					failed = true
					return errors.New("simulated state write outage")
				}
				return c.Update(ctx, obj, opts...)
			}})
			for i := 0; i < 12 && !failed; i++ {
				result := f.step(t)
				f.now = f.now.Add(result.RequeueAfter)
			}
			if !failed {
				t.Fatal("failure boundary not exercised")
			}
			if stage != state.StagePasswordPatched {
				// The password already has a durable completion record; process
				// loss must not issue another credential after later mutations.
				f.r.passwords = map[string]string{}
			}
			f.finish(t)
			if f.v.issued != 1 || f.v.revoked != 0 {
				t.Fatal("journal write retry lost or duplicated the issued lease")
			}
			if stage == state.StagePasswordPatched && f.api.passwordPatches != 2 {
				t.Fatal("unacknowledged password patch did not safely repeat")
			}
		})
	}
}

func TestRecreatedClusterCannotInheritOldLease(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	old := f.entry(t).CurrentLeaseID
	c := cnpg.NewCluster()
	_ = f.r.Client.Get(context.Background(), f.key.NamespacedName, c)
	_ = f.r.Client.Delete(context.Background(), c)
	c.SetResourceVersion("")
	c.SetUID("replacement-uid")
	if err := f.r.Client.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.v.revokeFail = true
	f.step(t)
	if f.v.issued != 1 || f.entry(t).CurrentLeaseID != old {
		t.Fatal("new identity advanced before old-lease cleanup")
	}
	f.v.revokeFail = false
	f.finish(t)
	if f.entry(t).ClusterUID != "replacement-uid" || f.entry(t).CurrentLeaseID == old || f.v.revoked != 1 {
		t.Fatal("replacement inherited stale credentials")
	}
}
func TestPromotionAndNamespaceRemoval(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	f.r.Config.WatchNamespaces = []string{"other"}
	f.step(t)
	if err := f.r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.v.revoked != 0 {
		t.Fatal("unwatched lease revoked")
	}
	f.r.Config.WatchNamespaces = []string{"ns"}
	c := cnpg.NewCluster()
	_ = f.r.Reader.Get(context.Background(), f.key.NamespacedName, c)
	unstructured.RemoveNestedField(c.Object, "spec", "replica")
	_ = f.r.Client.Update(context.Background(), c)
	f.api.active = false
	f.step(t)
	if f.entry(t).ClusterUID != "" || f.v.revoked != 1 {
		t.Fatal("promotion did not clean lease")
	}
}

func TestPromotionWaitsUntilReplicationCredentialIsUnused(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	c := cnpg.NewCluster()
	_ = f.r.Reader.Get(context.Background(), f.key.NamespacedName, c)
	unstructured.RemoveNestedField(c.Object, "spec", "replica")
	_ = f.r.Client.Update(context.Background(), c)
	f.step(t)
	if f.v.revoked != 0 || f.entry(t).CurrentLeaseID == "" {
		t.Fatal("promotion intent revoked a credential still used by the WAL receiver")
	}
	f.api.active = false
	f.step(t)
	if f.v.revoked != 1 {
		t.Fatal("completed promotion did not clean up")
	}
}
func TestTwoFreshSweepAbsences(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	c := cnpg.NewCluster()
	_ = f.r.Reader.Get(context.Background(), f.key.NamespacedName, c)
	_ = f.r.Client.Delete(context.Background(), c)
	f.step(t)
	if f.v.revoked != 0 {
		t.Fatal("deletion event cleaned before sweep confirmation")
	}
	if err := f.r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.v.revoked != 0 {
		t.Fatal("one absence revoked")
	}
	if err := f.r.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.v.revoked != 1 || f.entry(t).ClusterUID != "" {
		t.Fatal("two absences did not clean up")
	}
}

func TestReconnectDeadlineReplacesFailedCredential(t *testing.T) {
	f := newFixture(t)
	f.api.active = false
	for i := 0; i < 12; i++ {
		result := f.step(t)
		f.now = f.now.Add(result.RequeueAfter)
		s := f.entry(t)
		if s.Pending != nil && s.Pending.Stage == state.StageReconnectPending {
			f.now = s.Pending.StageDeadline
			break
		}
	}
	for i := 0; i < 4; i++ {
		f.step(t)
	}
	if f.v.issued != 2 || f.v.revoked != 1 {
		t.Fatal("failed reconnect did not replace credential after deadline")
	}
}

func TestRevocationFailureRetainsVerifiedPending(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	p := &corev1.Pod{}
	_ = f.r.Reader.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "db-1"}, p)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", RestartCount: 1}}
	_ = f.r.Client.Status().Update(context.Background(), p)
	f.v.revokeFail = true
	old := f.entry(t).CurrentLeaseID
	for i := 0; i < 12; i++ {
		res := f.step(t)
		f.now = f.now.Add(res.RequeueAfter)
	}
	s := f.entry(t)
	if s.CurrentLeaseID != old || s.Pending == nil || s.Pending.Stage != state.StageVerified || f.v.issued != 2 {
		t.Fatal("revocation failure lost workflow")
	}
	f.v.revokeFail = false
	f.finish(t)
	if f.v.issued != 2 {
		t.Fatal("revocation retry issued duplicate")
	}
}

func TestVerifiedLeaseWaitsForSettledTargetBeforeRevocation(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	old := f.entry(t).CurrentLeaseID
	c := cnpg.NewCluster()
	// Change the primary episode while keeping the same live relationship.
	p := &corev1.Pod{}
	_ = f.r.Client.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "db-1"}, p)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "postgres", RestartCount: 1}}
	if err := f.r.Client.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		r := f.step(t)
		if s := f.entry(t); s.Pending != nil && s.Pending.Stage == state.StageVerified {
			break
		}
		f.now = f.now.Add(r.RequeueAfter)
	}
	if s := f.entry(t); s.Pending == nil || s.Pending.Stage != state.StageVerified {
		t.Fatal("did not reach verified boundary")
	}
	_ = f.r.Client.Get(context.Background(), f.key.NamespacedName, c)
	_ = unstructured.SetNestedField(c.Object, "db-2", "status", "targetPrimary")
	if err := f.r.Client.Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.step(t)
	if f.v.revoked != 0 || f.entry(t).CurrentLeaseID != old {
		t.Fatal("revoked current lease during an unsettled target transition")
	}
}

func TestReappearingClusterResetsAbsenceConfirmation(t *testing.T) {
	f := newFixture(t)
	f.finish(t)
	c := cnpg.NewCluster()
	_ = f.r.Reader.Get(context.Background(), f.key.NamespacedName, c)
	_ = f.r.Client.Delete(context.Background(), c)
	_ = f.r.Sweep(context.Background())
	c.SetResourceVersion("")
	_ = f.r.Client.Create(context.Background(), c)
	_ = f.r.Sweep(context.Background())
	_ = f.r.Client.Delete(context.Background(), c)
	_ = f.r.Sweep(context.Background())
	if f.v.revoked != 0 {
		t.Fatal("nonconsecutive absences revoked lease")
	}
}
