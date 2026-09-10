// Package cnpg contains the small portion of the CloudNativePG API used by
// this controller. Keeping these types local avoids importing the CNPG
// controller implementation into the operator.
package cnpg

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var GroupVersion = schema.GroupVersion{Group: "postgresql.cnpg.io", Version: "v1"}

// Cluster is the subset of a CNPG Cluster object required for credential
// rotation. Unknown CRD fields are preserved by the API server when patches
// are sent as JSON patches, so this deliberately small type is safe to use.
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterSpec   `json:"spec,omitempty"`
	Status            ClusterStatus `json:"status,omitempty"`
}

type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

type ClusterSpec struct {
	Replica          ReplicaConfiguration `json:"replica,omitempty"`
	ExternalClusters []ExternalCluster    `json:"externalClusters,omitempty"`
}

type ReplicaConfiguration struct {
	Enabled        bool   `json:"enabled,omitempty"`
	Source         string `json:"source,omitempty"`
	Self           string `json:"self,omitempty"`
	Primary        string `json:"primary,omitempty"`
	PromotionToken string `json:"promotionToken,omitempty"`
}

type ExternalCluster struct {
	Name                 string              `json:"name"`
	ConnectionParameters map[string]string   `json:"connectionParameters,omitempty"`
	Password             *SecretKeyReference `json:"password,omitempty"`
}

type SecretKeyReference struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type ClusterStatus struct {
	CurrentPrimary string `json:"currentPrimary,omitempty"`
	TargetPrimary  string `json:"targetPrimary,omitempty"`
	ReadyInstances int32  `json:"readyInstances,omitempty"`
	Phase          string `json:"phase,omitempty"`
}

func (c *Cluster) DeepCopyObject() runtime.Object {
	if c == nil {
		return (*Cluster)(nil)
	}
	out := new(Cluster)
	*out = *c
	out.ObjectMeta = *c.ObjectMeta.DeepCopy()
	out.Spec.ExternalClusters = append([]ExternalCluster(nil), c.Spec.ExternalClusters...)
	for i := range out.Spec.ExternalClusters {
		if c.Spec.ExternalClusters[i].ConnectionParameters != nil {
			out.Spec.ExternalClusters[i].ConnectionParameters = map[string]string{}
			for k, v := range c.Spec.ExternalClusters[i].ConnectionParameters {
				out.Spec.ExternalClusters[i].ConnectionParameters[k] = v
			}
		}
		if c.Spec.ExternalClusters[i].Password != nil {
			ref := *c.Spec.ExternalClusters[i].Password
			out.Spec.ExternalClusters[i].Password = &ref
		}
	}
	return out
}

func (c *ClusterList) DeepCopyObject() runtime.Object {
	if c == nil {
		return (*ClusterList)(nil)
	}
	out := new(ClusterList)
	*out = *c
	out.Items = make([]Cluster, len(c.Items))
	for i := range c.Items {
		copy := c.Items[i].DeepCopyObject().(*Cluster)
		out.Items[i] = *copy
	}
	return out
}

func AddToScheme(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &Cluster{}, &ClusterList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

func (c *Cluster) IsReplica() bool {
	if c == nil || stringsTrim(c.Spec.Replica.Source) == "" {
		return false
	}
	// Standalone replicas use enabled/source. Distributed topology uses
	// primary/source (and deliberately does not set enabled); a Cluster whose
	// identity matches primary is the global primary.
	primary := stringsTrim(c.Spec.Replica.Primary)
	if primary == "" {
		return c.Spec.Replica.Enabled
	}
	self := stringsTrim(c.Spec.Replica.Self)
	if self == "" {
		self = c.Name
	}
	return primary != self
}

func (c *Cluster) ExternalCluster() (*ExternalCluster, error) {
	if c == nil {
		return nil, fmt.Errorf("cluster is nil")
	}
	if stringsTrim(c.Spec.Replica.Source) == "" {
		return nil, fmt.Errorf("replica source is required")
	}
	for i := range c.Spec.ExternalClusters {
		if c.Spec.ExternalClusters[i].Name == c.Spec.Replica.Source {
			return &c.Spec.ExternalClusters[i], nil
		}
	}
	return nil, fmt.Errorf("external cluster %q is not configured", c.Spec.Replica.Source)
}

func (c *Cluster) CredentialReference() (string, string, error) {
	external, err := c.ExternalCluster()
	if err != nil {
		return "", "", err
	}
	if external.Password == nil || external.Password.Name == "" {
		return "", "", fmt.Errorf("external cluster %q has no password Secret reference", external.Name)
	}
	key := external.Password.Key
	if key != "password" {
		return "", "", fmt.Errorf("external cluster %q password key must be password", external.Name)
	}
	return external.Password.Name, key, nil
}

func (c *Cluster) SourceRole() (string, error) {
	external, err := c.ExternalCluster()
	if err != nil {
		return "", err
	}
	return external.Name, nil
}

func (c *Cluster) Username() (string, error) {
	external, err := c.ExternalCluster()
	if err != nil {
		return "", err
	}
	return external.ConnectionParameters["user"], nil
}

func (c *Cluster) NamespacedName() types.NamespacedName {
	return types.NamespacedName{Namespace: c.Namespace, Name: c.Name}
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
