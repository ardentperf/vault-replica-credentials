package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

// SecretPatcher is intentionally narrower than client.Client. Its only
// target-namespace operation is a blind Secret patch; there is no read path.
type SecretPatcher interface {
	PatchPassword(ctx context.Context, namespace, name, key, password string) error
}

type ClientSecretPatcher struct{ Client client.Client }

func PatchSecretPassword(ctx context.Context, c client.Client, namespace, name, key, password string) error {
	return (ClientSecretPatcher{Client: c}).PatchPassword(ctx, namespace, name, key, password)
}

func (p ClientSecretPatcher) PatchPassword(ctx context.Context, namespace, name, key, password string) error {
	if p.Client == nil {
		return fmt.Errorf("Kubernetes client is nil")
	}
	if namespace == "" || name == "" || key != "password" || password == "" {
		return fmt.Errorf("invalid target Secret password patch")
	}
	// RawPatch deliberately sends PATCH /api/v1/namespaces/{ns}/secrets/{name}
	// without first reading the object. Only data.password is changed.
	patch, err := json.Marshal(map[string]any{"data": map[string]string{key: passwordBase64(password)}})
	if err != nil {
		return fmt.Errorf("encode Secret patch: %w", err)
	}
	secret := &corev1.Secret{}
	secret.Namespace, secret.Name = namespace, name
	return p.Client.Patch(ctx, secret, client.RawPatch(types.MergePatchType, patch))
}

// passwordBase64 is kept separate to make it obvious that the value sent to
// the API is Secret data. It never stores or returns the Kubernetes object.
func passwordBase64(password string) string {
	// encoding/base64 is intentionally used through a tiny helper so callers
	// cannot accidentally construct a Secret object containing other fields.
	return base64.StdEncoding.EncodeToString([]byte(password))
}

// PatchClusterUsername emits a JSON patch containing only the selected
// external-cluster username. It never rewrites the full Cluster spec.
func PatchClusterUsername(ctx context.Context, c client.Client, cluster *cnpg.Cluster, externalName, username string) error {
	if c == nil || cluster == nil {
		return fmt.Errorf("Kubernetes client and Cluster are required")
	}
	if username == "" {
		return fmt.Errorf("Vault username is required")
	}
	index := -1
	for i := range cluster.Spec.ExternalClusters {
		if cluster.Spec.ExternalClusters[i].Name == externalName {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("external cluster %q is not configured", externalName)
	}
	base := fmt.Sprintf("/spec/externalClusters/%d/connectionParameters", index)
	var ops []jsonPatchOperation
	if cluster.Spec.ExternalClusters[index].ConnectionParameters == nil {
		ops = append(ops, jsonPatchOperation{Op: "add", Path: base, Value: map[string]string{"user": username}})
	} else if _, ok := cluster.Spec.ExternalClusters[index].ConnectionParameters["user"]; ok {
		ops = append(ops, jsonPatchOperation{Op: "replace", Path: base + "/user", Value: username})
	} else {
		ops = append(ops, jsonPatchOperation{Op: "add", Path: base + "/user", Value: username})
	}
	encoded, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("encode Cluster username patch: %w", err)
	}
	return c.Patch(ctx, cluster, client.RawPatch(types.JSONPatchType, encoded))
}

type jsonPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// StateStore persists only the named state Secret in the system namespace.
type StateStore interface {
	Load(ctx context.Context) (state.Store, error)
	Save(ctx context.Context, value state.Store) error
}

// KubernetesStateStore uses a normal cached/read-write controller client for
// the dedicated system Secret. It is not usable for target credential data.
type KubernetesStateStore struct {
	Client               client.Client
	Namespace, Name, Key string
	MaxBytes             int
}

func (s KubernetesStateStore) Load(ctx context.Context) (state.Store, error) {
	if s.Client == nil {
		return state.Store{}, fmt.Errorf("state Kubernetes client is nil")
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, secret); err != nil {
		return state.Store{}, err
	}
	return state.LoadSecret(secret, s.Key, s.MaxBytes)
}

func (s KubernetesStateStore) Save(ctx context.Context, value state.Store) error {
	if s.Client == nil {
		return fmt.Errorf("state Kubernetes client is nil")
	}
	raw, err := value.Marshal(s.MaxBytes)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		secret := &corev1.Secret{}
		err = s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, secret)
		if err != nil {
			return err
		}
		if string(secret.Data[s.Key]) == string(raw) {
			return nil
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[s.Key] = append([]byte(nil), raw...)
		err = s.Client.Update(ctx, secret)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("state Secret update conflict retries exhausted")
}

// ProxyStatusReader obtains instance-manager status through the Kubernetes
// API pods/proxy subresource. It has no PostgreSQL or Secret access.
//
// Recent CNPG releases serve the otherwise-public status endpoint over TLS.
// The Kubernetes pod proxy speaks plain HTTP to its backend and consequently
// cannot reach those Pods. In that case, Read uses the Pod IP only for the
// same /pg/status endpoint over TLS; it never opens a PostgreSQL connection.
type ProxyStatusReader struct {
	Client kubernetes.Interface
	Config *rest.Config
}

func NewProxyStatusReader(restConfig *rest.Config) (*ProxyStatusReader, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("Kubernetes REST config is nil")
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes status client: %w", err)
	}
	return &ProxyStatusReader{Client: clientset, Config: rest.CopyConfig(restConfig)}, nil
}

type InstanceStatus struct {
	IsWalReceiverActive bool `json:"isWalReceiverActive"`
}

func (r *ProxyStatusReader) Read(ctx context.Context, namespace, podName string) (InstanceStatus, error) {
	if r == nil || r.Client == nil {
		return InstanceStatus{}, fmt.Errorf("Kubernetes proxy client is nil")
	}
	if namespace == "" || podName == "" {
		return InstanceStatus{}, fmt.Errorf("namespace and Pod name are required")
	}
	data, err := r.Client.CoreV1().RESTClient().Get().Namespace(namespace).Resource("pods").Name(podName).SubResource("proxy").Suffix("pg/status").Do(ctx).Raw()
	if err != nil {
		data, err = r.readTLSStatus(ctx, namespace, podName, err)
		if err != nil {
			return InstanceStatus{}, fmt.Errorf("read instance-manager status: %w", err)
		}
	}
	var status InstanceStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return InstanceStatus{}, fmt.Errorf("decode instance-manager status")
	}
	return status, nil
}

func (r *ProxyStatusReader) readTLSStatus(ctx context.Context, namespace, podName string, proxyErr error) ([]byte, error) {
	if r.Config == nil {
		return nil, proxyErr
	}
	pod, err := r.Client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get instance-manager Pod: %w", err)
	}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("instance-manager Pod has no IP (proxy error: %v)", proxyErr)
	}
	config := rest.CopyConfig(r.Config)
	config.Host = "https://" + net.JoinHostPort(pod.Status.PodIP, "8000")
	config.TLSClientConfig.Insecure = true
	config.TLSClientConfig.CAFile = ""
	config.TLSClientConfig.CAData = nil
	config.Timeout = 10 * time.Second
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("create instance-manager TLS client: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, config.Host+"/pg/status", nil)
	if err != nil {
		return nil, fmt.Errorf("create instance-manager status request: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET instance-manager TLS status: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("instance-manager TLS status returned HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 64<<10))
}
