package cnpg

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func fixture() *unstructured.Unstructured {
	c := NewCluster()
	c.SetName("db02")
	c.SetNamespace("ns")
	c.SetUID("cluster-uid")
	c.Object["spec"] = map[string]any{"replica": map[string]any{"primary": "db01", "source": "db01"}, "externalClusters": []any{map[string]any{"name": "db01", "connectionParameters": map[string]any{"host": "gateway", "user": "private-user"}, "password": map[string]any{"name": "credential", "key": "password"}}}}
	return c
}

func TestObservationIdentityAndPredicates(t *testing.T) {
	c := fixture()
	o, err := Observe(c)
	if err != nil || !o.Replica || o.Source != "db01" {
		t.Fatal("distributed replica not recognized")
	}
	other := c.DeepCopy()
	_ = unstructured.SetNestedField(other.Object, "db02", "spec", "replica", "primary")
	primary, err := Observe(other)
	if err != nil || primary.Replica {
		t.Fatal("distributed primary treated as replica")
	}
	own := c.DeepCopy()
	entries, _, _ := unstructured.NestedSlice(own.Object, "spec", "externalClusters")
	entries[0].(map[string]any)["connectionParameters"].(map[string]any)["user"] = "new-private-user"
	_ = unstructured.SetNestedSlice(own.Object, entries, "spec", "externalClusters")
	if ClusterPredicate().Update(event.UpdateEvent{ObjectOld: c, ObjectNew: own}) {
		t.Fatal("own username triggers observation")
	}
	changed := c.DeepCopy()
	_ = unstructured.SetNestedField(changed.Object, "db02-2", "status", "targetPrimary")
	if !ClusterPredicate().Update(event.UpdateEvent{ObjectOld: c, ObjectNew: changed}) {
		t.Fatal("primary transition ignored")
	}
	if !ClusterPredicate().Update(event.UpdateEvent{ObjectOld: c, ObjectNew: other}) {
		t.Fatal("promotion ignored")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db02-1", UID: "pod-uid"}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if !Ready(pod) {
		t.Fatal("Ready Pod rejected")
	}
	deleting := metav1.Now()
	pod.DeletionTimestamp = &deleting
	if Ready(pod) {
		t.Fatal("deleting Pod accepted")
	}
}

func TestInvalidRelationship(t *testing.T) {
	c := fixture()
	entries, _, _ := unstructured.NestedSlice(c.Object, "spec", "externalClusters")
	entries[0].(map[string]any)["password"].(map[string]any)["key"] = "username"
	_ = unstructured.SetNestedSlice(c.Object, entries, "spec", "externalClusters")
	if _, err := Observe(c); err == nil {
		t.Fatal("username Secret key accepted")
	}
}
