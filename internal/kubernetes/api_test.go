package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestAPIUsesContractualPatchAndPodProxyRequests(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		switch request.URL.Path {
		case "/api/v1/namespaces/reporting/secrets/credentials":
			if request.Method != http.MethodPatch || request.Header.Get("Content-Type") != "application/merge-patch+json" {
				t.Errorf("unexpected Secret request: %s %s", request.Method, request.Header.Get("Content-Type"))
			}
			body, _ := io.ReadAll(request.Body)
			var patch map[string]map[string]string
			if err := json.Unmarshal(body, &patch); err != nil {
				t.Fatal(err)
			}
			if patch["data"]["password"] != base64.StdEncoding.EncodeToString([]byte("new-password")) {
				t.Errorf("Secret patch = %s", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"kind":"Secret","apiVersion":"v1","data":{"unrelated":"must-not-be-used"}}`)
		case "/apis/postgresql.cnpg.io/v1/namespaces/reporting/clusters/db02":
			if request.Method != http.MethodPatch || request.Header.Get("Content-Type") != "application/json-patch+json" {
				t.Errorf("unexpected Cluster request: %s %s", request.Method, request.Header.Get("Content-Type"))
			}
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), `"path":"/spec/externalClusters/1/connectionParameters/user"`) {
				t.Errorf("Cluster patch = %s", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"kind":"Cluster","apiVersion":"postgresql.cnpg.io/v1","metadata":{"name":"db02","namespace":"reporting"}}`)
		case "/api/v1/namespaces/reporting/pods/https:db02-1:8000/proxy/pg/status":
			if request.Method != http.MethodGet {
				t.Errorf("unexpected status method %s", request.Method)
			}
			_, _ = io.WriteString(writer, `{"instance":{"isWalReceiverActive":true}}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	api, err := NewAPI(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := api.PatchSecretPassword(ctx, "reporting", "credentials", "password", "new-password"); err != nil {
		t.Fatal(err)
	}
	if err := api.PatchClusterUsername(ctx, "reporting", "db02", 1, "new-user"); err != nil {
		t.Fatal(err)
	}
	active, err := api.WalReceiverActive(ctx, "reporting", "db02-1")
	if err != nil || !active {
		t.Fatalf("WalReceiverActive() = %v, %v", active, err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestAPIStatusRequiresWalReceiverField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer server.Close()
	api, err := NewAPI(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.WalReceiverActive(context.Background(), "reporting", "pod"); err == nil {
		t.Fatal("WalReceiverActive() accepted missing field")
	}
}
