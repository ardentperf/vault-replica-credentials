package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

const maxStatusBytes = 1 << 20

// Mutator is the exact write-only and Pod-proxy Kubernetes boundary used by
// reconciliation. It intentionally has no target Secret read operation.
type Mutator interface {
	PatchSecretPassword(ctx context.Context, namespace, name, key, password string) error
	PatchClusterUsername(ctx context.Context, namespace, name string, externalIndex int, username string) error
	WalReceiverActive(ctx context.Context, namespace, pod string) (bool, error)
}

// API performs direct requests so target Secret mutation cannot accidentally
// turn into a get-then-update sequence.
type API struct {
	core    rest.Interface
	dynamic dynamic.Interface
}

// NewAPI builds the narrow Kubernetes API boundary.
func NewAPI(config *rest.Config) (*API, error) {
	if config == nil {
		return nil, errors.New("Kubernetes REST configuration is required")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes core client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}
	return &API{core: clientset.CoreV1().RESTClient(), dynamic: dynamicClient}, nil
}

// PatchSecretPassword sends only PATCH and ignores the returned Secret body.
func (api *API) PatchSecretPassword(ctx context.Context, namespace, name, key, password string) error {
	if namespace == "" || name == "" || key == "" {
		return errors.New("patch target Secret: reference is incomplete")
	}
	patch, err := json.Marshal(map[string]any{
		"data": map[string]string{key: base64.StdEncoding.EncodeToString([]byte(password))},
	})
	if err != nil {
		return errors.New("patch target Secret: encode request")
	}
	err = api.core.Patch(types.MergePatchType).
		Namespace(namespace).
		Resource("secrets").
		Name(name).
		Body(patch).
		Do(ctx).
		Error()
	if err != nil {
		return safeAPIError("patch target Secret", err)
	}
	return nil
}

// PatchClusterUsername changes only the selected array element's user field.
func (api *API) PatchClusterUsername(ctx context.Context, namespace, name string, externalIndex int, username string) error {
	if namespace == "" || name == "" || externalIndex < 0 || username == "" {
		return errors.New("patch Cluster username: input is incomplete")
	}
	patch, err := json.Marshal([]map[string]any{{
		"op": "add", "path": "/spec/externalClusters/" + strconv.Itoa(externalIndex) + "/connectionParameters/user", "value": username,
	}})
	if err != nil {
		return errors.New("patch Cluster username: encode request")
	}
	_, err = api.dynamic.Resource(cnpg.ClusterGVR).Namespace(namespace).Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return safeAPIError("patch Cluster username", err)
	}
	return nil
}

// WalReceiverActive reads the instance-manager status solely through the
// Kubernetes pods/proxy subresource.
func (api *API) WalReceiverActive(ctx context.Context, namespace, pod string) (bool, error) {
	if namespace == "" || pod == "" {
		return false, errors.New("read instance status: Pod reference is incomplete")
	}
	response, err := api.core.Get().
		Namespace(namespace).
		Resource("pods").
		Name("https:"+pod+":8000").
		SubResource("proxy").
		Suffix("pg", "status").
		Do(ctx).
		Raw()
	if err != nil {
		return false, safeAPIError("read instance status", err)
	}
	if len(response) > maxStatusBytes {
		return false, errors.New("read instance status: response is too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(response)))
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return false, errors.New("read instance status: malformed response")
	}
	active, found := findBool(payload, "isWalReceiverActive")
	if !found {
		return false, errors.New("read instance status: WAL receiver field is missing")
	}
	return active, nil
}

func findBool(value any, key string) (bool, bool) {
	switch typed := value.(type) {
	case map[string]any:
		if result, ok := typed[key].(bool); ok {
			return result, true
		}
		for _, child := range typed {
			if result, ok := findBool(child, key); ok {
				return result, true
			}
		}
	case []any:
		for _, child := range typed {
			if result, ok := findBool(child, key); ok {
				return result, true
			}
		}
	}
	return false, false
}

func safeAPIError(operation string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s: %w", operation, ErrNotFound)
	}
	if reason := apierrors.ReasonForError(err); reason != "" {
		return fmt.Errorf("%s failed: %s", operation, reason)
	}
	return fmt.Errorf("%s failed", operation)
}

// ErrNotFound is the safe sentinel used for write-only Secret retry handling.
var ErrNotFound = errors.New("resource not found")
