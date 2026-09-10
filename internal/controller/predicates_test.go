package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

func TestPodPredicateAndMapping(t *testing.T) {
	pod := testPod()
	predicate := PodPredicate()
	if !predicate.Create(event.CreateEvent{Object: pod}) {
		t.Fatal("ready CNPG Pod creation was filtered")
	}
	requests := MapPodToCluster(context.Background(), pod)
	if len(requests) != 1 || requests[0].Namespace != "reporting" || requests[0].Name != "db02" {
		t.Fatalf("requests = %#v", requests)
	}
	noise := pod.DeepCopy()
	noise.Annotations = map[string]string{"noise": "true"}
	if predicate.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: noise}) {
		t.Fatal("irrelevant Pod update was admitted")
	}
	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "reporting", Name: "unrelated"}}
	if predicate.Create(event.CreateEvent{Object: unrelated}) {
		t.Fatal("unrelated Pod was admitted")
	}
}

func TestClusterPredicateIgnoresUsernamePatch(t *testing.T) {
	oldCluster := testCluster()
	newCluster := oldCluster.DeepCopy()
	externals, _, _ := unstructured.NestedSlice(newCluster.Object, "spec", "externalClusters")
	external := externals[0].(map[string]any)
	external["connectionParameters"].(map[string]any)["user"] = "new-user"
	_ = unstructured.SetNestedSlice(newCluster.Object, externals, "spec", "externalClusters")
	if ClusterPredicate().Update(event.UpdateEvent{ObjectOld: oldCluster, ObjectNew: newCluster}) {
		t.Fatal("controller username patch was admitted as a trigger")
	}
	if cnpg.MapPodClusterName(testPod()) == "" {
		t.Fatal("fixture lost CNPG cluster label")
	}
}
