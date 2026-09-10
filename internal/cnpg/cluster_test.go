package cnpg

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestInspectAndTriggerUsesDesignatedReadyPrimary(t *testing.T) {
	cluster := NewClusterObject()
	cluster.SetNamespace("reporting")
	cluster.SetName("replica")
	cluster.SetUID(types.UID("cluster-uid"))
	cluster.Object["spec"] = map[string]any{
		"replica": map[string]any{"enabled": true, "source": "source-db"},
		"externalClusters": []any{map[string]any{
			"name": "source-db", "connectionParameters": map[string]any{"user": "old-user"},
			"password": map[string]any{"name": "replication", "key": "password"},
		}},
	}
	cluster.Object["status"] = map[string]any{"targetPrimary": "replica-1"}
	info, err := Inspect(cluster)
	if err != nil {
		t.Fatal(err)
	}
	if info.SourceName != "source-db" || info.SecretName != "replication" || info.PrimaryPod != "replica-1" {
		t.Fatalf("Inspect() = %#v", info)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "replica-1", UID: "pod-uid"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 2}}}}
	trigger, ok := TriggerForPod(info, pod)
	if !ok || trigger != "cluster-uid/pod/pod-uid/r2" {
		t.Fatalf("TriggerForPod() = %q, %t", trigger, ok)
	}
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, ok := TriggerForPod(info, pod); ok {
		t.Fatal("not-ready primary produced trigger")
	}
}

func TestRelevantChangeIgnoresOnlyUsernamePatch(t *testing.T) {
	oldCluster := testReplicaCluster("old-user", "source.example")
	newCluster := testReplicaCluster("new-user", "source.example")
	if RelevantChange(oldCluster, newCluster) {
		t.Fatal("controller's username-only patch should not enqueue rotation")
	}
	changedEndpoint := testReplicaCluster("new-user", "other.example")
	if !RelevantChange(newCluster, changedEndpoint) {
		t.Fatal("relevant external-cluster endpoint change was ignored")
	}
}

func TestInspectDistinguishesDistributedPrimaryAndReplica(t *testing.T) {
	primary := testDistributedCluster("db01", "db01", "db02")
	info, err := Inspect(primary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Replica {
		t.Fatalf("distributed primary inspected as replica: %#v", info)
	}

	replica := testDistributedCluster("db02", "db01", "db01")
	info, err = Inspect(replica)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Replica || info.SourceName != "db01" {
		t.Fatalf("distributed replica inspected incorrectly: %#v", info)
	}

	promoted := testDistributedCluster("db02", "db02", "db01")
	if !RelevantChange(replica, promoted) {
		t.Fatal("distributed promotion did not qualify as a relevant change")
	}
}

func testDistributedCluster(name, primary, source string) *unstructured.Unstructured {
	cluster := NewClusterObject()
	cluster.SetNamespace("reporting")
	cluster.SetName(name)
	cluster.SetUID(types.UID(name + "-uid"))
	cluster.Object["spec"] = map[string]any{
		"replica": map[string]any{"primary": primary, "source": source},
		"externalClusters": []any{map[string]any{
			"name": source, "connectionParameters": map[string]any{"user": "old-user", "host": "source.example"},
			"password": map[string]any{"name": "replication", "key": "password"},
		}},
	}
	cluster.Object["status"] = map[string]any{"targetPrimary": name + "-1"}
	return cluster
}

func testReplicaCluster(user, host string) *unstructured.Unstructured {
	cluster := NewClusterObject()
	cluster.SetNamespace("reporting")
	cluster.SetName("replica")
	cluster.SetUID(types.UID("cluster-uid"))
	cluster.Object["spec"] = map[string]any{
		"replica": map[string]any{"enabled": true, "source": "source-db"},
		"externalClusters": []any{map[string]any{
			"name": "source-db", "connectionParameters": map[string]any{"user": user, "host": host},
			"password": map[string]any{"name": "replication", "key": "password"},
		}},
	}
	cluster.Object["status"] = map[string]any{"targetPrimary": "replica-1"}
	return cluster
}
