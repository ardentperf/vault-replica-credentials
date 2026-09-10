package vault

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientIssuesValidCredentialAndRevokesLease(t *testing.T) {
	var revokeBody struct {
		LeaseID string `json:"lease_id"`
		Sync    bool   `json:"sync"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Vault-Token") != "test-token" {
			t.Error("Vault token header was not sent")
		}
		switch request.URL.Path {
		case "/v1/database/creds/source":
			_, _ = io.WriteString(writer, `{"lease_id":"database/creds/source/abc","lease_duration":3600,"data":{"username":"v-user","password":"v-password"}}`)
		case "/v1/sys/leases/revoke":
			if request.Method != http.MethodPost {
				t.Errorf("revoke method = %s, want POST", request.Method)
			}
			if err := json.NewDecoder(request.Body).Decode(&revokeBody); err != nil {
				t.Errorf("decode revoke request: %v", err)
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	client, err := NewHTTPClient(Config{Address: server.URL, Token: "test-token", RequestTimeout: time.Second, MaxRetries: 0, RetryDelay: time.Millisecond, MinLease: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := client.IssueDatabaseCredential(context.Background(), "source")
	if err != nil {
		t.Fatal(err)
	}
	if credential.ExpiresAt != now.Add(time.Hour) || credential.Password != "v-password" {
		t.Fatalf("credential = %#v, want calculated expiry and password", credential)
	}
	if err := client.RevokeLease(context.Background(), credential.LeaseID); err != nil {
		t.Fatal(err)
	}
	if revokeBody.LeaseID != credential.LeaseID || !revokeBody.Sync {
		t.Fatalf("revoke payload = %#v, want lease ID and sync=true", revokeBody)
	}
}

func TestHTTPClientReturnsSynchronousRevokeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/sys/leases/revoke" || request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := NewHTTPClient(Config{Address: server.URL, Token: "token", RequestTimeout: time.Second, MaxRetries: 0, RetryDelay: time.Millisecond, MinLease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RevokeLease(context.Background(), "database/creds/source/lease"); err == nil {
		t.Fatal("RevokeLease() accepted a synchronous revocation failure")
	}
}

func TestHTTPClientRejectsShortLeaseAndRetriesTemporaryFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, `{"lease_id":"lease","lease_duration":1,"data":{"username":"user","password":"password"}}`)
	}))
	defer server.Close()
	client, err := NewHTTPClient(Config{Address: server.URL, Token: "token", RequestTimeout: time.Second, MaxRetries: 1, RetryDelay: time.Millisecond, MinLease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.IssueDatabaseCredential(context.Background(), "role"); err == nil {
		t.Fatal("IssueDatabaseCredential() accepted a short lease")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want retry", calls.Load())
	}
}
