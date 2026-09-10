// Package cnpg contains the small unstructured CloudNativePG observation
// surface needed by the external controller. It intentionally does not own or
// create Cluster resources.
package cnpg

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	ClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	ClusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
)

const ClusterLabel = "cnpg.io/cluster"

// ExternalCluster is the selected source connection configuration. Password
// is a reference only; the Secret itself is never read by the controller.
type ExternalCluster struct {
	Index      int
	Name       string
	SecretName string
	SecretKey  string
}

// Observation is the current mutation precondition and durable trigger input.
type Observation struct {
	Namespace         string
	Name              string
	UID               string
	Replica           bool
	Source            string
	External          ExternalCluster
	CurrentPrimary    string
	TargetPrimary     string
	PrimaryPodUID     string
	PrimaryReady      bool
	PostgresRestarts  int32
	PrimaryTransition string
}

// NewCluster creates an unstructured object with the CNPG Cluster GVK.
func NewCluster() *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(ClusterGVK)
	return cluster
}

// NewClusterList creates the list type used by fresh orphan-sweep reads.
func NewClusterList() *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(ClusterGVK.GroupVersion().WithKind("ClusterList"))
	return list
}

// Observe validates the current Cluster and designated-primary Pod.
func Observe(cluster *unstructured.Unstructured, pod *corev1.Pod) (Observation, error) {
	if cluster == nil {
		return Observation{}, errors.New("Cluster is required")
	}
	replica, source, err := ReplicaMode(cluster)
	if err != nil {
		return Observation{}, err
	}
	targetPrimary, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimary")
	targetTimestamp, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimaryTimestamp")
	designatedPrimary := DesignatedPrimary(cluster)
	observation := Observation{
		Namespace: cluster.GetNamespace(), Name: cluster.GetName(), UID: string(cluster.GetUID()),
		Replica: replica, Source: source, CurrentPrimary: designatedPrimary, TargetPrimary: targetPrimary,
		PrimaryTransition: targetTimestamp,
	}
	if !replica {
		return observation, nil
	}
	if source == "" {
		return Observation{}, errors.New("replica Cluster has no source")
	}
	external, err := selectedExternal(cluster, source)
	if err != nil {
		return Observation{}, err
	}
	observation.External = external
	if designatedPrimary == "" {
		return Observation{}, errors.New("replica Cluster has no designated primary")
	}
	if pod == nil {
		if targetPrimary != "" && targetTimestamp != "" {
			return observation, nil
		}
		return Observation{}, errors.New("designated-primary Pod is not observable")
	}
	if pod.Namespace != cluster.GetNamespace() || pod.Name != designatedPrimary {
		return Observation{}, errors.New("designated-primary Pod is not observable")
	}
	observation.PrimaryPodUID = string(pod.UID)
	observation.PrimaryReady = PodReady(pod)
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "postgres" {
			observation.PostgresRestarts = status.RestartCount
			break
		}
	}
	if observation.PrimaryPodUID == "" {
		return Observation{}, errors.New("designated-primary Pod has no UID")
	}
	return observation, nil
}

// DesignatedPrimary returns CNPG's target primary during a transition and
// falls back to the current primary when no target has been reported. This
// keeps one topology fingerprint from target selection through convergence.
func DesignatedPrimary(cluster *unstructured.Unstructured) string {
	if cluster == nil {
		return ""
	}
	target, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimary")
	if target != "" {
		return target
	}
	current, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	return current
}

// ReplicaMode supports both standalone replica clusters (`enabled`) and CNPG
// distributed topology (`primary` compared with `self`, defaulting to name).
func ReplicaMode(cluster *unstructured.Unstructured) (bool, string, error) {
	if cluster == nil {
		return false, "", errors.New("Cluster is required")
	}
	replicaSpec, found, err := unstructured.NestedMap(cluster.Object, "spec", "replica")
	if err != nil {
		return false, "", errors.New("Cluster replica mode is malformed")
	}
	if !found {
		return false, "", nil
	}
	source, _ := replicaSpec["source"].(string)
	if enabledValue, present := replicaSpec["enabled"]; present {
		enabled, ok := enabledValue.(bool)
		if !ok {
			return false, "", errors.New("Cluster replica mode is malformed")
		}
		return enabled, source, nil
	}
	primary, _ := replicaSpec["primary"].(string)
	self, _ := replicaSpec["self"].(string)
	if self == "" {
		self = cluster.GetName()
	}
	if primary == "" {
		return false, source, nil
	}
	return primary != self, source, nil
}

func selectedExternal(cluster *unstructured.Unstructured, source string) (ExternalCluster, error) {
	externals, found, err := unstructured.NestedSlice(cluster.Object, "spec", "externalClusters")
	if err != nil || !found {
		return ExternalCluster{}, errors.New("replica Cluster external configuration is missing")
	}
	for index, raw := range externals {
		external, ok := raw.(map[string]any)
		if !ok || external["name"] != source {
			continue
		}
		password, ok := external["password"].(map[string]any)
		if !ok {
			return ExternalCluster{}, errors.New("selected external Cluster has no password reference")
		}
		name, _ := password["name"].(string)
		key, _ := password["key"].(string)
		if name == "" || key == "" {
			return ExternalCluster{}, errors.New("selected external Cluster password reference is incomplete")
		}
		return ExternalCluster{Index: index, Name: source, SecretName: name, SecretKey: key}, nil
	}
	return ExternalCluster{}, errors.New("selected external Cluster is missing")
}

// TriggerID is stable for duplicate observations and changes for source,
// topology, Pod replacement, or in-place postgres-container restart episodes.
func (observation Observation) TriggerID() string {
	if observation.PrimaryPodUID == "" {
		return fmt.Sprintf("%s/source/%s/primary/%s/transition/%s",
			observation.UID, observation.Source, observation.CurrentPrimary, observation.PrimaryTransition)
	}
	return fmt.Sprintf("%s/source/%s/primary/%s/pod/%s/restart/%d/target/%s",
		observation.UID, observation.Source, observation.CurrentPrimary,
		observation.PrimaryPodUID, observation.PostgresRestarts, observation.TargetPrimary)
}

// TriggerKind returns a bounded metric label.
func TriggerKind(previous, current string) string {
	if previous == "" {
		return "initialization"
	}
	if triggerValue(previous, "source") != triggerValue(current, "source") ||
		triggerValue(previous, "primary") != triggerValue(current, "primary") {
		return "topology_change"
	}
	return "pod_replacement"
}

func triggerValue(trigger, name string) string {
	parts := strings.Split(trigger, "/")
	for index := 0; index+1 < len(parts); index++ {
		if parts[index] == name {
			return parts[index+1]
		}
	}
	return ""
}

// PodReady reports the actual Ready condition, not merely Pod phase.
func PodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// RelevantClusterChanged excludes the controller's own username patch while
// detecting role, source, reference, connection, and primary topology changes.
func RelevantClusterChanged(oldCluster, newCluster *unstructured.Unstructured) bool {
	return clusterProjection(oldCluster) != clusterProjection(newCluster)
}

func clusterProjection(cluster *unstructured.Unstructured) string {
	if cluster == nil {
		return ""
	}
	replica, source, _ := ReplicaMode(cluster)
	current, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	target, _, _ := unstructured.NestedString(cluster.Object, "status", "targetPrimary")
	externals, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "externalClusters")
	var selected map[string]any
	for _, raw := range externals {
		item, ok := raw.(map[string]any)
		if ok && item["name"] == source {
			selected = map[string]any{}
			for key, value := range item {
				if key == "connectionParameters" {
					parameters, _ := value.(map[string]any)
					copyParameters := map[string]any{}
					for parameter, parameterValue := range parameters {
						if parameter != "user" {
							copyParameters[parameter] = parameterValue
						}
					}
					selected[key] = copyParameters
					continue
				}
				selected[key] = value
			}
			break
		}
	}
	encoded, _ := json.Marshal([]any{replica, source, selected, current, target, cluster.GetDeletionTimestamp() != nil})
	return string(encoded)
}

// RelevantPodChanged returns true only for readiness, deletion, or postgres
// restart changes. Ordinary metadata/status noise does not enqueue work.
func RelevantPodChanged(oldPod, newPod *corev1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return true
	}
	if PodReady(oldPod) != PodReady(newPod) || !timestampsEqual(oldPod.DeletionTimestamp, newPod.DeletionTimestamp) {
		return true
	}
	return postgresRestarts(oldPod) != postgresRestarts(newPod)
}

func postgresRestarts(pod *corev1.Pod) int32 {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "postgres" {
			return status.RestartCount
		}
	}
	return 0
}

func timestampsEqual(left, right *metav1.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(right)
}

// MapPodClusterName accepts the documented CNPG cluster label only.
func MapPodClusterName(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	return pod.Labels[ClusterLabel]
}

// SortedNamespaces returns stable display-only namespace configuration.
func SortedNamespaces(namespaces []string) []string {
	result := slices.Clone(namespaces)
	slices.Sort(result)
	return result
}
