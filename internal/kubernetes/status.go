package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"

	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// PodStatusReader is the controller's only health-verification boundary. It
// intentionally represents the Kubernetes pods/proxy request, never a direct
// PostgreSQL connection.
type PodStatusReader interface {
	WalReceiverActive(ctx context.Context, namespace, pod string) (bool, error)
}

type podStatusReader struct{ pods corev1client.PodsGetter }

func NewPodStatusReader(pods corev1client.PodsGetter) PodStatusReader {
	return &podStatusReader{pods: pods}
}

func (r *podStatusReader) WalReceiverActive(ctx context.Context, namespace, pod string) (bool, error) {
	if r == nil || r.pods == nil || namespace == "" || pod == "" {
		return false, fmt.Errorf("namespace and pod are required for /pg/status")
	}
	// CNPG exposes the instance-manager status API on its HTTPS status port.
	// Supplying both scheme and port is essential: leaving them empty makes the
	// API server proxy to PostgreSQL's first declared port (5432), which returns
	// an EOF rather than the status JSON.
	raw, err := r.pods.Pods(namespace).ProxyGet("https", pod, "8000", "pg/status", nil).DoRaw(ctx)
	if err != nil {
		return false, fmt.Errorf("get Pod proxy /pg/status: %w", err)
	}
	var response struct {
		IsWalReceiverActive bool `json:"isWalReceiverActive"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, fmt.Errorf("decode Pod proxy /pg/status: %w", err)
	}
	return response.IsWalReceiverActive, nil
}
