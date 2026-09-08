package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"k8s.io/client-go/rest"
)

func TestWriteOnlySecretAndProxyProtocol(t *testing.T) {
	patches, proxies := 0, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/ns/secrets/credential":
			patches++
			if r.Method != "PATCH" || r.Header.Get("Content-Type") != "application/merge-patch+json" {
				t.Error("not a blind merge PATCH")
			}
			var body map[string]map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 1 || len(body["data"]) != 1 || body["data"]["password"] != "cHJpdmF0ZQ==" {
				t.Error("incorrect password-only patch")
			}
			fmt.Fprint(w, `{"data":{"password":"do-not-read-response"}}`)
		case "/api/v1/namespaces/ns/pods/https:db02-1:8000/proxy/pg/status":
			proxies++
			if r.Method != "GET" {
				t.Error("wrong status verb")
			}
			fmt.Fprint(w, `{"isWalReceiverActive":true}`)
		default:
			t.Error("unexpected API request")
			w.WriteHeader(403)
		}
	}))
	defer s.Close()
	a, err := NewAPI(&rest.Config{Host: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	o := cnpg.Observation{Namespace: "ns", Secret: "credential", PasswordKey: "password", Primary: "db02-1"}
	if err := a.PatchPassword(context.Background(), o, "private"); err != nil {
		t.Fatal(err)
	}
	active, err := a.WALReceiver(context.Background(), o.Namespace, o.Primary)
	if err != nil || !active {
		t.Fatal("status failed")
	}
	if patches != 1 || proxies != 1 {
		t.Fatal("unexpected request count")
	}
}

func TestStatusFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"isWalReceiverActive":false}`, `{"isWalReceiverActive":"true"}`, `not-json`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		a, _ := NewAPI(&rest.Config{Host: s.URL})
		active, _ := a.WALReceiver(context.Background(), "ns", "pod")
		if active {
			t.Fatal("invalid status accepted")
		}
		s.Close()
	}
}

func TestSecretPatchNeverFollowsRedirectIntoGet(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			http.Redirect(w, r, "/private-secret", http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"data":{"password":"must-not-read"}}`)
	}))
	defer s.Close()
	a, _ := NewAPI(&rest.Config{Host: s.URL})
	err := a.PatchPassword(context.Background(), cnpg.Observation{Namespace: "ns", Secret: "credential", PasswordKey: "password"}, "private")
	if err == nil || requests != 1 {
		t.Fatal("Secret patch followed a redirect, violating write-only access")
	}
}

func TestUsernamePatchTestsFreshVersionAndNamedEntry(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.Path != "/apis/postgresql.cnpg.io/v1/namespaces/ns/clusters/db" || r.Header.Get("Content-Type") != "application/json-patch+json" {
			t.Error("incorrect Cluster patch protocol")
		}
		var ops []map[string]any
		if json.NewDecoder(r.Body).Decode(&ops) != nil || len(ops) != 3 {
			t.Fatal("invalid JSON patch")
		}
		if ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/resourceVersion" || ops[0]["value"] != "42" {
			t.Error("missing atomic resourceVersion guard")
		}
		if ops[1]["path"] != "/spec/externalClusters/2/name" || ops[1]["value"] != "source" {
			t.Error("missing selected name guard")
		}
		if ops[2]["path"] != "/spec/externalClusters/2/connectionParameters/user" {
			t.Error("patch changes unrelated fields")
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	a, _ := NewAPI(&rest.Config{Host: s.URL})
	c := cnpg.NewCluster()
	c.SetResourceVersion("42")
	if err := a.PatchUsername(context.Background(), c, cnpg.Observation{Namespace: "ns", Name: "db", Source: "source", ExternalIndex: 2}, "private-user"); err != nil {
		t.Fatal(err)
	}
}
