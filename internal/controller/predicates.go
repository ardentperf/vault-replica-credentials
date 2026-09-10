package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

// ClusterPredicate admits lifecycle and contract-relevant changes while
// ignoring generic status noise and the controller's own username patch.
func ClusterPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(create event.CreateEvent) bool {
			_, ok := create.Object.(*unstructured.Unstructured)
			return ok
		},
		DeleteFunc: func(deleted event.DeleteEvent) bool {
			_, ok := deleted.Object.(*unstructured.Unstructured)
			return ok
		},
		UpdateFunc: func(update event.UpdateEvent) bool {
			oldCluster, oldOK := update.ObjectOld.(*unstructured.Unstructured)
			newCluster, newOK := update.ObjectNew.(*unstructured.Unstructured)
			return oldOK && newOK && cnpg.RelevantClusterChanged(oldCluster, newCluster)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// PodPredicate filters ordinary Pod noise. Deletion is only an enqueue hint;
// reconciliation waits until a replacement designated primary is observable.
func PodPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(create event.CreateEvent) bool {
			pod, ok := create.Object.(*corev1.Pod)
			return ok && cnpg.MapPodClusterName(pod) != "" && cnpg.PodReady(pod)
		},
		DeleteFunc: func(deleted event.DeleteEvent) bool {
			pod, ok := deleted.Object.(*corev1.Pod)
			return ok && cnpg.MapPodClusterName(pod) != ""
		},
		UpdateFunc: func(update event.UpdateEvent) bool {
			oldPod, oldOK := update.ObjectOld.(*corev1.Pod)
			newPod, newOK := update.ObjectNew.(*corev1.Pod)
			return oldOK && newOK && cnpg.MapPodClusterName(newPod) != "" && cnpg.RelevantPodChanged(oldPod, newPod)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// MapPodToCluster maps only the CNPG cluster label to the stable queue key.
func MapPodToCluster(_ context.Context, object client.Object) []reconcile.Request {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return nil
	}
	name := cnpg.MapPodClusterName(pod)
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: pod.Namespace, Name: name}}}
}

var _ handler.MapFunc = MapPodToCluster
