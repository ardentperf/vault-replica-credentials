package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

var ErrNotFound = errors.New("Kubernetes object not found")

type API struct {
	host string
	http *http.Client
}

func NewAPI(cfg *rest.Config) (*API, error) {
	c := rest.CopyConfig(cfg)
	c.Timeout = 10 * time.Second
	h, err := rest.HTTPClientFor(c)
	if err != nil {
		return nil, err
	}
	// A 301/302 must never transform a blind Secret PATCH into a GET.
	h.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &API{host: strings.TrimRight(c.Host, "/"), http: h}, nil
}

// PatchPassword deliberately discards the response body. Even a successful
// PATCH response contains Secret data that the controller does not consume.
func (a *API) PatchPassword(ctx context.Context, o cnpg.Observation, password string) error {
	body, _ := json.Marshal(map[string]any{"data": map[string]string{o.PasswordKey: base64.StdEncoding.EncodeToString([]byte(password))}})
	_, err := a.request(ctx, "PATCH", "/api/v1/namespaces/"+url.PathEscape(o.Namespace)+"/secrets/"+url.PathEscape(o.Secret), "application/merge-patch+json", body, false)
	return err
}

func (a *API) PatchUsername(ctx context.Context, c *unstructured.Unstructured, o cnpg.Observation, username string) error {
	base := fmt.Sprintf("/spec/externalClusters/%d", o.ExternalIndex)
	// Resolve the index by name from the fresh object; test resourceVersion and
	// name atomically so reordering or promotion cannot redirect this patch.
	body, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": c.GetResourceVersion()},
		{"op": "test", "path": base + "/name", "value": o.Source},
		{"op": "add", "path": base + "/connectionParameters/user", "value": username},
	})
	_, err := a.request(ctx, "PATCH", "/apis/postgresql.cnpg.io/v1/namespaces/"+url.PathEscape(o.Namespace)+"/clusters/"+url.PathEscape(o.Name), "application/json-patch+json", body, false)
	return err
}

func (a *API) WALReceiver(ctx context.Context, namespace, pod string) (bool, error) {
	if pod == "" {
		return false, errors.New("designated primary unavailable")
	}
	raw, err := a.request(ctx, "GET", "/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods/https:"+url.PathEscape(pod)+":8000/proxy/pg/status", "", nil, true)
	if err != nil {
		return false, err
	}
	var status struct {
		Active *bool `json:"isWalReceiverActive"`
	}
	if json.Unmarshal(raw, &status) != nil || status.Active == nil {
		return false, errors.New("invalid instance status")
	}
	return *status.Active, nil
}

func (a *API) request(ctx context.Context, method, path, contentType string, body []byte, read bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.host+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Kubernetes request failed")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errors.New("Kubernetes request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Kubernetes HTTP status %d", resp.StatusCode)
	}
	if !read {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return nil, errors.New("instance status read failed")
	}
	return raw, nil
}
