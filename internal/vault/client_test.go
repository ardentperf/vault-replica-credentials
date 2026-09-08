package vault

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/config"
)

func TestVaultProtocol(t *testing.T) {
	issued, revoked := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "private-token" {
			t.Error("missing authentication")
		}
		switch r.URL.Path {
		case "/v1/database/creds/db01":
			issued++
			if r.Method != "GET" {
				t.Error("wrong issuance verb")
			}
			fmt.Fprint(w, `{"lease_id":"database/creds/db01/lease","lease_duration":2764800,"data":{"username":"private-user","password":"private-password"}}`)
		case "/v1/sys/leases/revoke":
			revoked++
			w.WriteHeader(204)
		default:
			t.Error("unexpected path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := New(config.VaultConfig{Address: server.URL, Token: "private-token", RequestTimeout: time.Second, MinLease: 768 * time.Hour, SafetyMargin: 24 * time.Hour})
	credential, err := c.IssueDatabaseCredential(context.Background(), "db01")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Password != "private-password" || credential.LeaseDuration != 768*time.Hour {
		t.Fatal("incorrect credential")
	}
	if err := c.RevokeLease(context.Background(), credential.LeaseID); err != nil {
		t.Fatal(err)
	}
	if issued != 1 || revoked != 1 {
		t.Fatal("unexpected operation count")
	}
	if strings.Contains(fmt.Sprintf("%+v", credential), "private-") {
		t.Fatal("credential formatting leaks")
	}
}

func TestVaultFailuresRedactedAndNotRetried(t *testing.T) {
	for _, body := range []string{`private-password`, `{"lease_duration":1,"data":{"password":"private-password"}}`, `{"lease_id":"lease","lease_duration":1,"data":{"username":"private-user","password":"private-password"}}`, `{"lease_id":"lease","lease_duration":9223372036854775807,"data":{"username":"private-user","password":"private-password"}}`} {
		calls := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, body) }))
		c := New(config.VaultConfig{Address: s.URL, Token: "private-token", RequestTimeout: time.Second, MinLease: 768 * time.Hour})
		_, err := c.IssueDatabaseCredential(context.Background(), "db01")
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatal("failure missing or sensitive")
		}
		// Malformed or rejected issuance is never silently retried. A known
		// rejected lease is revoked once, when its ID can be decoded.
		if calls > 2 {
			t.Fatal("unbounded retry")
		}
		s.Close()
	}
}

func TestVaultTimeoutAndRedirectDoNotReplayIssuance(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer s.Close()
		c := New(config.VaultConfig{Address: s.URL, Token: "private-token", RequestTimeout: 10 * time.Millisecond})
		_, err := c.IssueDatabaseCredential(context.Background(), "db01")
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("timeout is missing or leaks")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		calls := 0
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(307)
		}))
		defer s.Close()
		c := New(config.VaultConfig{Address: s.URL, Token: "private-token", RequestTimeout: time.Second})
		_, err := c.IssueDatabaseCredential(context.Background(), "db01")
		if err == nil || calls != 1 {
			t.Fatal("redirect forwarded token/replayed request")
		}
	})
}
