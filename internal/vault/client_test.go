package vault

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientIssuesAndRevokesWithoutLeakingResponseBody(t *testing.T) {
	var revokePayload string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "test-token" {
			t.Errorf("missing Vault token header")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/database/creds/db01":
			fmt.Fprint(w, `{"request_id":"secret-request","lease_id":"lease-1","lease_duration":3600,"data":{"username":"v-user","password":"v-password"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sys/leases/revoke":
			buffer, _ := io.ReadAll(r.Body)
			revokePayload = string(buffer)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	client, err := NewHTTPClient(HTTPConfig{Address: server.URL, AllowInsecureHTTP: true, Token: "test-token", MinLease: time.Hour, SafetyMargin: time.Minute, MaxRetries: 0, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := client.IssueDatabaseCredential(context.Background(), "db01")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Username != "v-user" || credential.Password != "v-password" || credential.ExpiresAt != now.Add(time.Hour) {
		t.Fatalf("credential = %#v", credential)
	}
	if err := client.RevokeLease(context.Background(), credential.LeaseID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(revokePayload, `"lease_id":"lease-1"`) {
		t.Fatalf("revoke payload = %s", revokePayload)
	}
}

func TestHTTPClientRejectsShortLeaseAndRetriesServerFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		if call < 3 {
			http.Error(w, "sensitive-password-body", http.StatusServiceUnavailable)
			return
		}
		if call == 3 {
			fmt.Fprint(w, `{"lease_id":"lease-1","lease_duration":3600,"data":{"username":"u","password":"p"}}`)
			return
		}
		fmt.Fprint(w, `{"lease_id":"lease-1","lease_duration":60,"data":{"username":"u","password":"p"}}`)
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{Address: server.URL, AllowInsecureHTTP: true, Token: "token", MinLease: time.Minute + time.Second, SafetyMargin: time.Second, MaxRetries: 3, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.IssueDatabaseCredential(context.Background(), "db01")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("request count = %d, want 3", calls.Load())
	}
	short, err := NewHTTPClient(HTTPConfig{Address: server.URL, AllowInsecureHTTP: true, Token: "token", MinLease: time.Hour, SafetyMargin: time.Minute, MaxRetries: 0})
	if err != nil {
		t.Fatal(err)
	}
	// The next response is still valid but intentionally too short.
	_, err = short.IssueDatabaseCredential(context.Background(), "db01")
	if err == nil || strings.Contains(err.Error(), "sensitive-password-body") {
		t.Fatalf("short lease error = %v", err)
	}
}
