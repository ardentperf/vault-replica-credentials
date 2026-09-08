// Package vault implements dynamic database issuance and lease revocation.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/config"
)

// Credential is the response from the Vault database role endpoint. Password
// is process-memory-only and must never be serialized, logged, or emitted.
type Credential struct {
	Username      string `json:"-"`
	Password      string `json:"-"`
	LeaseID       string `json:"-"`
	LeaseDuration time.Duration
	ExpiresAt     time.Time
}

func (Credential) String() string     { return "Credential{redacted}" }
func (c Credential) GoString() string { return c.String() }

type HTTPClient struct {
	cfg  config.VaultConfig
	http *http.Client
}

func New(cfg config.VaultConfig) *HTTPClient {
	return &HTTPClient{cfg: cfg, http: &http.Client{Timeout: cfg.RequestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

var roleName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// Issuance is non-idempotent: never automatically replay a possibly successful
// request. The reconciler owns bounded, timed retries and the durable journal.
func (c *HTTPClient) IssueDatabaseCredential(ctx context.Context, role string) (Credential, error) {
	if !roleName.MatchString(role) || strings.Contains(role, "..") {
		return Credential{}, errors.New("invalid Vault role")
	}
	started := time.Now()
	raw, err := c.request(ctx, http.MethodGet, "database/creds/"+role, nil)
	if err != nil {
		return Credential{}, err
	}
	var response struct {
		LeaseID string `json:"lease_id"`
		Seconds int64  `json:"lease_duration"`
		Data    struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return Credential{}, errors.New("invalid Vault credential response")
	}
	if response.LeaseID == "" || response.Data.Username == "" || response.Data.Password == "" || response.Seconds <= 0 || response.Seconds > math.MaxInt64/int64(time.Second) || time.Duration(response.Seconds)*time.Second < c.cfg.MinLease || time.Duration(response.Seconds)*time.Second <= c.cfg.SafetyMargin {
		// Return only cleanup metadata with the error. The reconciler journals
		// the known rejected lease so a revocation outage cannot lose its ID.
		expires := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		if response.Seconds > 0 && response.Seconds <= math.MaxInt64/int64(time.Second) {
			expires = started.Add(time.Duration(response.Seconds) * time.Second).UTC()
		}
		return Credential{LeaseID: response.LeaseID, Username: response.Data.Username, ExpiresAt: expires}, errors.New("Vault credential or lease duration rejected")
	}
	duration := time.Duration(response.Seconds) * time.Second
	return Credential{Username: response.Data.Username, Password: response.Data.Password, LeaseID: response.LeaseID, LeaseDuration: duration, ExpiresAt: started.Add(duration).UTC()}, nil
}

func (c *HTTPClient) RevokeLease(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"lease_id": id})
	_, err := c.request(ctx, http.MethodPut, "sys/leases/revoke", body)
	return err
}

func (c *HTTPClient) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.Address, "/")+"/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("Vault request configuration failed")
	}
	req.Header.Set("X-Vault-Token", c.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Vault request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Vault HTTP status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return nil, errors.New("Vault response read failed")
	}
	return raw, nil
}

// Client is the minimum external API contract: database credential issuance
// and full lease revocation. Lease renewal is intentionally absent.
type Client interface {
	IssueDatabaseCredential(ctx context.Context, role string) (Credential, error)
	RevokeLease(ctx context.Context, leaseID string) error
}
