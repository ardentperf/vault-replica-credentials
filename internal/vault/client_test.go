package vault

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientIssueAndRevoke(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	var revoked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Vault-Token") != "test-token" {
			t.Error("request did not carry configured Vault token")
		}
		switch request.URL.Path {
		case "/v1/database/creds/db01":
			_, _ = io.WriteString(writer, `{"lease_id":"database/creds/db01/lease-sensitive","lease_duration":2764800,"data":{"username":"dynamic-sensitive","password":"password-sensitive"}}`)
		case "/v1/sys/leases/revoke":
			revoked.Store(true)
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, func() time.Time { return now })
	credential, err := client.IssueDatabaseCredential(context.Background(), "db01")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Username != "dynamic-sensitive" || credential.Password != "password-sensitive" {
		t.Fatal("credential response was not decoded")
	}
	if credential.ExpiresAt != now.Add(768*time.Hour) {
		t.Fatalf("ExpiresAt = %s", credential.ExpiresAt)
	}
	if err := client.RevokeLease(context.Background(), credential.LeaseID); err != nil {
		t.Fatal(err)
	}
	if !revoked.Load() {
		t.Fatal("revoke endpoint was not called")
	}
}

func TestHTTPClientRejectsMalformedAndShortLeasesWithoutLeaking(t *testing.T) {
	responses := []string{
		`not-json`,
		`{"lease_id":"lease-sensitive","lease_duration":1,"data":{"username":"user-sensitive","password":"password-sensitive"}}`,
		`{"lease_id":"lease-sensitive","lease_duration":2764800,"data":{"username":"","password":"password-sensitive"}}`,
	}
	for _, response := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, response)
		}))
		client := newTestClient(t, server.URL, time.Now)
		_, err := client.IssueDatabaseCredential(context.Background(), "db01")
		server.Close()
		if err == nil {
			t.Fatal("IssueDatabaseCredential() error = nil")
		}
		for _, sensitive := range []string{"lease-sensitive", "user-sensitive", "password-sensitive", "test-token"} {
			if strings.Contains(err.Error(), sensitive) {
				t.Fatalf("error leaked %q: %v", sensitive, err)
			}
		}
	}
}

func TestHTTPClientRetriesOnlyRetryableFailures(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(writer, "password-sensitive", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, `{"lease_id":"lease","lease_duration":2764800,"data":{"username":"user","password":"pass"}}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Now)
	if _, err := client.IssueDatabaseCredential(context.Background(), "db01"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestHTTPClientDoesNotRetryPermanentFailureOrExposeBody(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(writer, "password-sensitive", http.StatusForbidden)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Now)
	_, err := client.IssueDatabaseCredential(context.Background(), "db01")
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("error=%v attempts=%d", err, attempts.Load())
	}
	if strings.Contains(err.Error(), "password-sensitive") {
		t.Fatalf("error leaked response body: %v", err)
	}
}

func TestHTTPClientTimeoutIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Now)
	client.requestTimeout = 5 * time.Millisecond
	client.retryAttempts = 1
	started := time.Now()
	_, err := client.IssueDatabaseCredential(context.Background(), "db01")
	if err == nil || time.Since(started) > 80*time.Millisecond {
		t.Fatalf("timeout was not bounded: error=%v elapsed=%s", err, time.Since(started))
	}
}

func newTestClient(t *testing.T, address string, now func() time.Time) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(HTTPConfig{
		Address: address, Token: "test-token", MinLease: 768 * time.Hour,
		SafetyMargin: time.Hour, RequestTimeout: time.Second,
		RetryAttempts: 3, RetryBackoff: time.Millisecond, Now: now,
		Wait: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
