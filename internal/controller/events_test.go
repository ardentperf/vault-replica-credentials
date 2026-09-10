package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

func TestPredicatesIgnoreControllerUsernameAndGenericStatusChanges(t *testing.T) {
	old := testCluster()
	newCluster := old.DeepCopyObject().(*cnpg.Cluster)
	newCluster.Spec.ExternalClusters[0].ConnectionParameters["user"] = "new-user"
	if RelevantClusterChange(old, newCluster) {
		t.Fatal("username-only change was considered a rotation trigger")
	}
	newCluster.Status.Phase = "Cluster in healthy state"
	if RelevantClusterChange(old, newCluster) {
		t.Fatal("generic status change was considered a rotation trigger")
	}
	newCluster.Status.CurrentPrimary = "db-replica-2"
	if !RelevantClusterChange(old, newCluster) {
		t.Fatal("primary transition was ignored")
	}
	if !ClusterPredicate().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newCluster}) {
		t.Fatal("predicate ignored primary transition")
	}
}

func TestPodDeletionIsOnlyAHintUntilReplacementIsReady(t *testing.T) {
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "db-1", UID: types.UID("old"), Labels: map[string]string{"cnpg.io/cluster": "replica"}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	newPod := old.DeepCopy()
	newPod.Name = "db-2"
	newPod.UID = types.UID("new")
	newPod.Status.Conditions[0].Status = corev1.ConditionFalse
	if !PodPredicate().Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("primary deletion was not enqueued as a hint")
	}
	if !PodPredicate().Create(event.CreateEvent{Object: newPod}) {
		t.Fatal("replacement was not observed")
	}
	if podReadiness(newPod) {
		t.Fatal("unready replacement reported ready")
	}
}

func TestDistributedPrimaryIsNotTreatedAsReplica(t *testing.T) {
	cluster := &cnpg.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "db02"}, Spec: cnpg.ClusterSpec{
		Replica: cnpg.ReplicaConfiguration{Enabled: true, Source: "db01", Self: "db02", Primary: "db02"},
	}}
	if cluster.IsReplica() {
		t.Fatal("distributed primary was treated as a replica")
	}
	cluster.Spec.Replica.Primary = "db01"
	if !cluster.IsReplica() {
		t.Fatal("distributed secondary was not treated as a replica")
	}
}

func testCluster() *cnpg.Cluster {
	return &cnpg.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replica", UID: types.UID("cluster-uid")}, Spec: cnpg.ClusterSpec{
		Replica:          cnpg.ReplicaConfiguration{Enabled: true, Source: "db01"},
		ExternalClusters: []cnpg.ExternalCluster{{Name: "db01", ConnectionParameters: map[string]string{"host": "source", "user": "old-user"}, Password: &cnpg.SecretKeyReference{Name: "replica-credentials", Key: "password"}}},
	}, Status: cnpg.ClusterStatus{CurrentPrimary: "replica-1"}}
}
