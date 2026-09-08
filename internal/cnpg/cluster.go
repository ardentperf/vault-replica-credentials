package cnpg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var GVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

func NewCluster() *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(GVK)
	return c
}
func NewClusterList() *unstructured.UnstructuredList {
	c := &unstructured.UnstructuredList{}
	c.SetGroupVersionKind(GVK.GroupVersion().WithKind("ClusterList"))
	return c
}

// Observation is reconstructed from the current CR at every mutation boundary.
// It must not be serialized into the durable journal or logged wholesale.
type Observation struct {
	Namespace, Name, UID, Source, Secret, PasswordKey, Username, Primary, Target, Relationship string
	Replica                                                                                    bool
	ExternalIndex                                                                              int
}

func Observe(c *unstructured.Unstructured) (Observation, error) {
	o := Observation{Namespace: c.GetNamespace(), Name: c.GetName(), UID: string(c.GetUID()), Primary: field(c, "status", "currentPrimary"), Target: field(c, "status", "targetPrimary")}
	enabled, _, _ := unstructured.NestedBool(c.Object, "spec", "replica", "enabled")
	primary := field(c, "spec", "replica", "primary")
	self := field(c, "spec", "replica", "self")
	if self == "" {
		self = o.Name
	}
	o.Replica = enabled || (primary != "" && primary != self)
	if !o.Replica {
		return o, nil
	}
	o.Source = field(c, "spec", "replica", "source")
	if len(validation.IsDNS1123Subdomain(o.Source)) > 0 || o.Source == "" {
		return o, errors.New("replica source is invalid")
	}
	entries, _, _ := unstructured.NestedSlice(c.Object, "spec", "externalClusters")
	found := false
	for i, item := range entries {
		e, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if e["name"] != o.Source {
			continue
		}
		if found {
			return o, errors.New("duplicate source entry")
		}
		found = true
		o.ExternalIndex = i
		o.Secret, _, _ = unstructured.NestedString(e, "password", "name")
		o.PasswordKey, _, _ = unstructured.NestedString(e, "password", "key")
		o.Username, _, _ = unstructured.NestedString(e, "connectionParameters", "user")
		parameters, _, _ := unstructured.NestedMap(e, "connectionParameters")
		delete(parameters, "user")
		o.Relationship = hash([]any{o.UID, o.Source, o.Secret, o.PasswordKey, parameters})
	}
	if !found || len(validation.IsDNS1123Subdomain(o.Secret)) > 0 || o.Secret == "" || o.PasswordKey != "password" {
		return o, errors.New("replica password reference is invalid")
	}
	return o, nil
}

func field(c *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(c.Object, path...)
	return v
}
func hash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (o Observation) Trigger(p *corev1.Pod) string {
	if p == nil {
		return o.Relationship + "/initialization"
	}
	return o.Relationship + "/" + hash([]string{o.Primary, string(p.UID), strconv.Itoa(int(Restarts(p)))})
}

func Restarts(p *corev1.Pod) int32 {
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == "postgres" {
			return s.RestartCount
		}
	}
	return 0
}
func Ready(p *corev1.Pod) bool {
	if p == nil || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func ClusterPredicate() predicate.Predicate {
	return predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		a, ok := e.ObjectOld.(*unstructured.Unstructured)
		if !ok {
			return false
		}
		b, ok := e.ObjectNew.(*unstructured.Unstructured)
		if !ok {
			return false
		}
		x, xe := Observe(a)
		y, ye := Observe(b)
		x.Username = ""
		y.Username = ""
		return !reflect.DeepEqual(x, y) || (xe == nil) != (ye == nil) || !reflect.DeepEqual(a.GetDeletionTimestamp(), b.GetDeletionTimestamp())
	}}
}

func PodPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return e.Object.GetLabels()["cnpg.io/cluster"] != "" },
		DeleteFunc: func(e event.DeleteEvent) bool { return e.Object.GetLabels()["cnpg.io/cluster"] != "" },
		UpdateFunc: func(e event.UpdateEvent) bool {
			a, ok := e.ObjectOld.(*corev1.Pod)
			if !ok {
				return false
			}
			b, ok := e.ObjectNew.(*corev1.Pod)
			if !ok || b.Labels["cnpg.io/cluster"] == "" {
				return false
			}
			return a.UID != b.UID || Ready(a) != Ready(b) || Restarts(a) != Restarts(b) || !reflect.DeepEqual(a.DeletionTimestamp, b.DeletionTimestamp)
		},
	}
}
