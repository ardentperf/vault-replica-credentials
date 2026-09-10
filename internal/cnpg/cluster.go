package cnpg

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var ClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

// ClusterInfo is the only configuration a rotation needs from the current CR.
// It intentionally never includes Secret data.
type ClusterInfo struct {
	Namespace     string
	Name          string
	UID           string
	Replica       bool
	SourceName    string
	SecretName    string
	SecretKey     string
	ExternalIndex int
	PrimaryPod    string
}

func NewClusterObject() *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(ClusterGVK)
	return object
}

// Inspect validates the subset of Cluster configuration owned by this
// controller. A non-replica Cluster is represented without an error so its
// known leases can be safely cleaned up.
func Inspect(object *unstructured.Unstructured) (ClusterInfo, error) {
	if object == nil {
		return ClusterInfo{}, fmt.Errorf("Cluster is required")
	}
	info := ClusterInfo{Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID())}
	if info.Namespace == "" || info.Name == "" || info.UID == "" {
		return ClusterInfo{}, fmt.Errorf("Cluster has incomplete metadata")
	}
	replica, err := isReplica(object)
	if err != nil {
		return ClusterInfo{}, err
	}
	if !replica {
		return info, nil
	}
	info.Replica = true
	source, found, err := unstructured.NestedString(object.Object, "spec", "replica", "source")
	if err != nil || !found || strings.TrimSpace(source) == "" {
		return ClusterInfo{}, fmt.Errorf("replica Cluster has no spec.replica.source")
	}
	info.SourceName = source
	externalClusters, found, err := unstructured.NestedSlice(object.Object, "spec", "externalClusters")
	if err != nil || !found {
		return ClusterInfo{}, fmt.Errorf("replica Cluster has no spec.externalClusters")
	}
	for index, candidate := range externalClusters {
		entry, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		if name != source {
			continue
		}
		password, ok := entry["password"].(map[string]any)
		if !ok {
			return ClusterInfo{}, fmt.Errorf("external cluster %q has no password reference", source)
		}
		secretName, _ := password["name"].(string)
		secretKey, _ := password["key"].(string)
		if strings.TrimSpace(secretName) == "" || strings.TrimSpace(secretKey) == "" {
			return ClusterInfo{}, fmt.Errorf("external cluster %q has an incomplete password reference", source)
		}
		connection, ok := entry["connectionParameters"].(map[string]any)
		if !ok {
			return ClusterInfo{}, fmt.Errorf("external cluster %q has no connectionParameters", source)
		}
		if _, ok := connection["user"].(string); !ok {
			return ClusterInfo{}, fmt.Errorf("external cluster %q has no connectionParameters.user", source)
		}
		info.SecretName, info.SecretKey, info.ExternalIndex = secretName, secretKey, index
		break
	}
	if info.SecretName == "" {
		return ClusterInfo{}, fmt.Errorf("replica source %q has no matching external cluster", source)
	}
	info.PrimaryPod = primaryName(object)
	return info, nil
}

// isReplica supports both CNPG replica forms. A standalone replica explicitly
// sets replica.enabled. In a distributed topology the Cluster whose
// replica.primary matches its self/name identity is the current source;
// every other member is in recovery from replica.source.
func isReplica(object *unstructured.Unstructured) (bool, error) {
	if object == nil {
		return false, fmt.Errorf("Cluster is required")
	}
	replica, found, err := unstructured.NestedMap(object.Object, "spec", "replica")
	if err != nil {
		return false, fmt.Errorf("read spec.replica: %w", err)
	}
	if !found {
		return false, nil
	}
	if enabled, exists := replica["enabled"]; exists {
		value, ok := enabled.(bool)
		if !ok {
			return false, fmt.Errorf("spec.replica.enabled must be a boolean")
		}
		return value, nil
	}
	primary, ok := replica["primary"].(string)
	if !ok || strings.TrimSpace(primary) == "" {
		return false, fmt.Errorf("distributed replica Cluster has no spec.replica.primary")
	}
	self, _ := replica["self"].(string)
	if strings.TrimSpace(self) == "" {
		self = object.GetName()
	}
	return primary != self, nil
}

// TriggerForPod builds a durable identity only when this is the current
// designated primary and it is ready. A deleted Pod never produces a trigger.
func TriggerForPod(info ClusterInfo, pod *corev1.Pod) (string, bool) {
	if pod == nil || info.PrimaryPod == "" || pod.Name != info.PrimaryPod || pod.Namespace != info.Namespace || !podReady(pod) || pod.UID == "" {
		return "", false
	}
	restarts := int32(0)
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}
	return info.UID + "/pod/" + string(pod.UID) + "/r" + strconv.FormatInt(int64(restarts), 10), true
}

func primaryName(object *unstructured.Unstructured) string {
	for _, field := range [][]string{{"status", "targetPrimary"}, {"status", "currentPrimary"}} {
		value, found, err := unstructured.NestedString(object.Object, field...)
		if err == nil && found && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// RelevantChange filters generic status churn and the controller's own user
// patch. The reconciler still reads the current Cluster before every mutation.
func RelevantChange(oldObject, newObject *unstructured.Unstructured) bool {
	if oldObject == nil || newObject == nil {
		return true
	}
	oldInfo, oldErr := Inspect(oldObject)
	newInfo, newErr := Inspect(newObject)
	if oldErr != nil || newErr != nil {
		return true
	}
	if oldInfo.Replica != newInfo.Replica || oldInfo.SourceName != newInfo.SourceName || oldInfo.SecretName != newInfo.SecretName || oldInfo.SecretKey != newInfo.SecretKey || oldInfo.PrimaryPod != newInfo.PrimaryPod {
		return true
	}
	// A change to the selected external connection (for example the source
	// endpoint) is relevant, but the controller's own user patch is not.
	return !reflect.DeepEqual(externalWithoutUser(oldObject, oldInfo.SourceName), externalWithoutUser(newObject, newInfo.SourceName))
}

func externalWithoutUser(object *unstructured.Unstructured, source string) map[string]any {
	entries, _, err := unstructured.NestedSlice(object.Object, "spec", "externalClusters")
	if err != nil {
		return nil
	}
	for _, candidate := range entries {
		entry, ok := candidate.(map[string]any)
		if !ok || entry["name"] != source {
			continue
		}
		copy, ok := runtimeDeepCopy(entry).(map[string]any)
		if !ok {
			return nil
		}
		if connection, ok := copy["connectionParameters"].(map[string]any); ok {
			delete(connection, "user")
		}
		return copy
	}
	return nil
}

func runtimeDeepCopy(value any) any {
	// unstructured's JSON-compatible deep copy is preferable to reflection so
	// comparison cannot mutate the cached object supplied by a predicate.
	switch typed := value.(type) {
	case map[string]any:
		copy := make(map[string]any, len(typed))
		for key, child := range typed {
			copy[key] = runtimeDeepCopy(child)
		}
		return copy
	case []any:
		copy := make([]any, len(typed))
		for index, child := range typed {
			copy[index] = runtimeDeepCopy(child)
		}
		return copy
	default:
		return typed
	}
}
