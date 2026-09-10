package cnpg

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestObserveBuildsStableTriggerAndReadsOnlyReference(t *testing.T) {
	cluster := fixtureCluster(true, "old-user")
	pod := fixturePod()
	observation, err := Observe(cluster, pod)
	if err != nil {
		t.Fatal(err)
	}
	if observation.External.SecretName != "credentials" || observation.External.SecretKey != "password" {
		t.Fatalf("external = %#v", observation.External)
	}
	if observation.TriggerID() == "" || !observation.PrimaryReady {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestObserveUsesTargetPrimaryThroughoutTransition(t *testing.T) {
	cluster := fixtureCluster(true, "old-user")
	cluster.Object["status"] = map[string]any{
		"currentPrimary":         "db02-1",
		"targetPrimary":          "db02-2",
		"targetPrimaryTimestamp": "2026-09-09T02:00:00Z",
	}
	pod := fixturePod()
	pod.Name = "db02-2"
	pod.UID = types.UID("target-pod-uid")

	observation, err := Observe(cluster, pod)
	if err != nil {
		t.Fatal(err)
	}
	if observation.CurrentPrimary != "db02-2" {
		t.Fatalf("designated primary = %q, want target db02-2", observation.CurrentPrimary)
	}
	if got := observation.TriggerID(); !strings.Contains(got, "/primary/db02-2/pod/target-pod-uid/") {
		t.Fatalf("transition trigger uses the old primary: %q", got)
	}
}

func TestReplicaModeSupportsDistributedTopology(t *testing.T) {
	cluster := fixtureCluster(true, "user")
	cluster.Object["spec"].(map[string]any)["replica"] = map[string]any{"primary": "db01", "source": "db01"}
	replica, source, err := ReplicaMode(cluster)
	if err != nil || !replica || source != "db01" {
		t.Fatalf("ReplicaMode() = %v, %q, %v", replica, source, err)
	}
	cluster.Object["spec"].(map[string]any)["replica"] = map[string]any{"primary": "db02", "source": "db01"}
	replica, _, err = ReplicaMode(cluster)
	if err != nil || replica {
		t.Fatalf("distributed primary ReplicaMode() = %v, %v", replica, err)
	}
}

func TestRelevantClusterChangedIgnoresOnlyUsername(t *testing.T) {
	oldCluster := fixtureCluster(true, "old-user")
	newCluster := fixtureCluster(true, "new-user")
	if RelevantClusterChanged(oldCluster, newCluster) {
		t.Fatal("username-only controller patch was considered a new trigger")
	}
	changed := newCluster.DeepCopy()
	_ = unstructured.SetNestedField(changed.Object, "db03", "spec", "replica", "source")
	if !RelevantClusterChanged(newCluster, changed) {
		t.Fatal("source relationship change was ignored")
	}
}

func TestRelevantPodChangedFiltersNoise(t *testing.T) {
	oldPod := fixturePod()
	newPod := oldPod.DeepCopy()
	newPod.Annotations = map[string]string{"noise": "value"}
	if RelevantPodChanged(oldPod, newPod) {
		t.Fatal("metadata noise was considered relevant")
	}
	newPod.Status.ContainerStatuses[0].RestartCount++
	if !RelevantPodChanged(oldPod, newPod) {
		t.Fatal("postgres restart was ignored")
	}
}

func TestTriggerKindDistinguishesTopologyAndPodEpisodes(t *testing.T) {
	base := "uid/source/db01/primary/db02-1/pod/pod-1/restart/0/target/db02-1"
	tests := []struct {
		name     string
		previous string
		current  string
		want     string
	}{
		{name: "initial", current: base, want: "initialization"},
		{name: "source", previous: base, current: "uid/source/db03/primary/db02-1/pod/pod-1/restart/0/target/db02-1", want: "topology_change"},
		{name: "primary", previous: base, current: "uid/source/db01/primary/db02-2/pod/pod-2/restart/0/target/db02-2", want: "topology_change"},
		{name: "pod", previous: base, current: "uid/source/db01/primary/db02-1/pod/pod-2/restart/0/target/db02-1", want: "pod_replacement"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := TriggerKind(test.previous, test.current); got != test.want {
				t.Fatalf("TriggerKind() = %q, want %q", got, test.want)
			}
		})
	}
}

func fixtureCluster(replica bool, username string) *unstructured.Unstructured {
	cluster := NewCluster()
	cluster.SetNamespace("reporting")
	cluster.SetName("db02")
	cluster.SetUID(types.UID("cluster-uid"))
	cluster.Object["spec"] = map[string]any{
		"replica": map[string]any{"enabled": replica, "source": "db01"},
		"externalClusters": []any{map[string]any{
			"name": "db01", "connectionParameters": map[string]any{"host": "source", "user": username},
			"password": map[string]any{"name": "credentials", "key": "password"},
		}},
	}
	cluster.Object["status"] = map[string]any{"currentPrimary": "db02-1", "targetPrimary": "db02-1"}
	return cluster
}

func fixturePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "db02-1", UID: types.UID("pod-uid"), Labels: map[string]string{ClusterLabel: "db02"}},
		Status: corev1.PodStatus{
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "postgres", RestartCount: 2}},
		},
	}
}
