package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

// ClusterPredicate accepts creation and meaningful topology/configuration
// changes, while ignoring generic status churn and the controller's own
// username patch. It does not watch Events or Nodes.
func ClusterPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { _, ok := e.Object.(*cnpg.Cluster); return ok },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCluster, oldOK := e.ObjectOld.(*cnpg.Cluster)
			newCluster, newOK := e.ObjectNew.(*cnpg.Cluster)
			return oldOK && newOK && RelevantClusterChange(oldCluster, newCluster)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { _, ok := e.Object.(*cnpg.Cluster); return ok },
		GenericFunc: func(e event.GenericEvent) bool { _, ok := e.Object.(*cnpg.Cluster); return ok },
	}
}

// PodPredicate treats deletion as a hint only. The reconciler requires the
// replacement/designated primary to be observable before issuing credentials.
func PodPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return relevantPod(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, oldOK := e.ObjectOld.(*corev1.Pod)
			newPod, newOK := e.ObjectNew.(*corev1.Pod)
			if !oldOK || !newOK || !relevantPod(newPod) {
				return false
			}
			return podReadiness(oldPod) != podReadiness(newPod) || restartCount(oldPod) != restartCount(newPod) || primaryLabel(oldPod) != primaryLabel(newPod)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevantPod(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevantPod(e.Object) },
	}
}

func RelevantClusterChange(oldCluster, newCluster *cnpg.Cluster) bool {
	if oldCluster == nil || newCluster == nil {
		return false
	}
	if oldCluster.Spec.Replica != newCluster.Spec.Replica {
		return true
	}
	if oldCluster.Status.CurrentPrimary != newCluster.Status.CurrentPrimary || oldCluster.Status.TargetPrimary != newCluster.Status.TargetPrimary {
		return true
	}
	return !sameWithoutUser(oldCluster.Spec.ExternalClusters, newCluster.Spec.ExternalClusters)
}

func sameWithoutUser(left, right []cnpg.ExternalCluster) bool {
	copyExternal := func(input []cnpg.ExternalCluster) []cnpg.ExternalCluster {
		out := make([]cnpg.ExternalCluster, len(input))
		copy(out, input)
		for i := range out {
			if out[i].ConnectionParameters != nil {
				parameters := make(map[string]string, len(out[i].ConnectionParameters))
				for key, value := range out[i].ConnectionParameters {
					if key != "user" {
						parameters[key] = value
					}
				}
				out[i].ConnectionParameters = parameters
			}
		}
		return out
	}
	leftRaw, _ := json.Marshal(copyExternal(left))
	rightRaw, _ := json.Marshal(copyExternal(right))
	return string(leftRaw) == string(rightRaw)
}

func relevantPod(object client.Object) bool {
	pod, ok := object.(*corev1.Pod)
	if !ok || pod.Namespace == "" {
		return false
	}
	return podClusterName(pod) != ""
}

func podClusterName(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, key := range []string{"cnpg.io/cluster", "postgresql.cnpg.io/cluster"} {
		if value := pod.Labels[key]; value != "" {
			return value
		}
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Cluster" {
			return owner.Name
		}
	}
	return ""
}

func primaryLabel(pod *corev1.Pod) bool {
	for _, key := range []string{"cnpg.io/instanceRole", "cnpg.io/instance-role", "postgresql.cnpg.io/instanceRole"} {
		if pod.Labels[key] == "primary" {
			return true
		}
	}
	return false
}

func podReadiness(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func restartCount(pod *corev1.Pod) int32 {
	var result int32
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		if status.RestartCount > result {
			result = status.RestartCount
		}
	}
	return result
}

func triggerFor(cluster *cnpg.Cluster, pod *corev1.Pod) string {
	if cluster == nil {
		return ""
	}
	clusterUID := string(cluster.UID)
	if clusterUID == "" {
		clusterUID = cluster.Name
	}
	configuration := struct {
		Replica  cnpg.ReplicaConfiguration `json:"replica"`
		External []cnpg.ExternalCluster    `json:"external"`
		Current  string                    `json:"current"`
		Target   string                    `json:"target"`
	}{Replica: cluster.Spec.Replica, External: cluster.Spec.ExternalClusters, Current: cluster.Status.CurrentPrimary, Target: cluster.Status.TargetPrimary}
	// User is controller-owned after the first rotation; excluding it prevents
	// the username patch from recursively creating a new trigger.
	for i := range configuration.External {
		if configuration.External[i].ConnectionParameters != nil {
			parameters := make(map[string]string, len(configuration.External[i].ConnectionParameters))
			for key, value := range configuration.External[i].ConnectionParameters {
				if key != "user" {
					parameters[key] = value
				}
			}
			configuration.External[i].ConnectionParameters = parameters
		}
	}
	serialized, _ := json.Marshal(configuration)
	digest := sha256.Sum256(serialized)
	configID := hex.EncodeToString(digest[:])[:16]
	if pod != nil {
		podUID := string(pod.UID)
		if podUID == "" {
			podUID = pod.Name
		}
		return fmt.Sprintf("%s/pod-replacement/%s/r%d/%s", clusterUID, podUID, restartCount(pod), configID)
	}
	return fmt.Sprintf("%s/topology/%s", clusterUID, configID)
}
