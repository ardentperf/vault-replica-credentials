// Package vault implements the narrow Vault HTTP boundary used by the
// reconciler. Response bodies are decoded into private structs and are never
// included in errors or logs.
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Credential is the response from the Vault database role endpoint. Password
// is process-memory-only and must never be serialized, logged, or emitted.
type Credential struct {
	Username      string
	Password      string
	LeaseID       string
	LeaseDuration time.Duration
	ExpiresAt     time.Time
}

// Client is the minimum external API contract: database credential issuance
// and full lease revocation. Lease renewal is intentionally absent.
type Client interface {
	IssueDatabaseCredential(ctx context.Context, role string) (Credential, error)
	RevokeLease(ctx context.Context, leaseID string) error
}

type HTTPConfig struct {
	Address           string
	Token             string
	MinLease          time.Duration
	SafetyMargin      time.Duration
	RequestTimeout    time.Duration
	MaxRetries        int
	RetryBackoff      time.Duration
	AllowInsecureHTTP bool
	HTTPClient        *http.Client
	Now               func() time.Time
}

// Config is retained as the concise public name for HTTPConfig.
type Config = HTTPConfig

// HTTPClient is a small Vault database-secrets client. It intentionally does
// not implement authentication or lease renewal; the supplied token is the
// test/dev session or an already-authenticated deployment token.
type HTTPClient struct {
	base           *url.URL
	token          string
	minLease       time.Duration
	safetyMargin   time.Duration
	requestTimeout time.Duration
	maxRetries     int
	retryBackoff   time.Duration
	httpClient     *http.Client
	now            func() time.Time
}

func NewHTTPClient(cfg HTTPConfig) (*HTTPClient, error) {
	address := strings.TrimRight(strings.TrimSpace(cfg.Address), "/")
	if address == "" {
		return nil, errors.New("Vault address is required")
	}
	base, err := url.Parse(address)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("Vault address must be an absolute URL")
	}
	if base.Scheme != "https" && !(cfg.AllowInsecureHTTP && base.Scheme == "http") {
		return nil, errors.New("Vault address must use HTTPS")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("Vault token is required")
	}
	if cfg.MinLease <= 0 {
		return nil, errors.New("Vault minimum lease must be positive")
	}
	if cfg.SafetyMargin < 0 || cfg.SafetyMargin >= cfg.MinLease {
		return nil, errors.New("Vault lease safety margin must be non-negative and less than minimum lease")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if cfg.MaxRetries < 0 {
		return nil, errors.New("Vault max retries must not be negative")
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 250 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.RequestTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &HTTPClient{base: base, token: cfg.Token, minLease: cfg.MinLease, safetyMargin: cfg.SafetyMargin,
		requestTimeout: cfg.RequestTimeout, maxRetries: cfg.MaxRetries, retryBackoff: cfg.RetryBackoff,
		httpClient: cfg.HTTPClient, now: cfg.Now}, nil
}

func New(cfg HTTPConfig) (*HTTPClient, error) { return NewHTTPClient(cfg) }

// DatabaseRolePath is the Vault endpoint for a source Cluster role.
func DatabaseRolePath(sourceClusterName string) (string, error) {
	name := strings.TrimSpace(sourceClusterName)
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", errors.New("source Cluster name is invalid for a Vault role")
	}
	return path.Join("/v1/database/creds", url.PathEscape(name)), nil
}

func RolePath(sourceClusterName string) (string, error) { return DatabaseRolePath(sourceClusterName) }

func (c *HTTPClient) IssueDatabaseCredential(ctx context.Context, role string) (Credential, error) {
	rolePath, err := DatabaseRolePath(role)
	if err != nil {
		return Credential{}, err
	}
	var response issueResponse
	if err := c.doJSON(ctx, http.MethodGet, rolePath, nil, &response); err != nil {
		return Credential{}, fmt.Errorf("Vault issue request failed: %w", err)
	}
	if response.Data.Username == "" || response.Data.Password == "" || response.LeaseID == "" {
		return Credential{}, errors.New("Vault issue response is missing required fields")
	}
	if response.LeaseDuration <= 0 {
		return Credential{}, errors.New("Vault issue response has an invalid lease duration")
	}
	lease := time.Duration(response.LeaseDuration) * time.Second
	if lease < c.minLease {
		return Credential{}, fmt.Errorf("Vault lease duration %s is below configured minimum", lease)
	}
	if lease <= c.safetyMargin {
		return Credential{}, errors.New("Vault lease duration does not leave the configured safety margin")
	}
	now := c.now()
	return Credential{Username: response.Data.Username, Password: response.Data.Password, LeaseID: response.LeaseID,
		LeaseDuration: lease, ExpiresAt: now.Add(lease)}, nil
}

func (c *HTTPClient) RevokeLease(ctx context.Context, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return errors.New("Vault lease ID is required")
	}
	payload := struct {
		LeaseID string `json:"lease_id"`
	}{LeaseID: leaseID}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sys/leases/revoke", payload, nil); err != nil {
		// Revocation is deliberately idempotent for recovery. Vault returns 404
		// for a lease that has already been revoked; doJSON maps that response.
		if errors.Is(err, errLeaseGone) {
			return nil
		}
		return fmt.Errorf("Vault revoke request failed: %w", err)
	}
	return nil
}

var errLeaseGone = errors.New("Vault lease is already gone")

type issueResponse struct {
	Data struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"data"`
	LeaseID       string `json:"lease_id"`
	LeaseDuration int64  `json:"lease_duration"`
}

func (c *HTTPClient) doJSON(ctx context.Context, method, endpoint string, body any, result any) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return errors.New("encode Vault request")
		}
	}
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		requestContext, cancel := context.WithTimeout(ctx, c.requestTimeout)
		var reader io.Reader
		if encoded != nil {
			reader = strings.NewReader(string(encoded))
		}
		req, requestErr := http.NewRequestWithContext(requestContext, method, c.base.ResolveReference(&url.URL{Path: endpoint}).String(), reader)
		if requestErr != nil {
			cancel()
			return errors.New("build Vault request")
		}
		req.Header.Set("X-Vault-Token", c.token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, requestErr := c.httpClient.Do(req)
		if requestErr == nil {
			status := response.StatusCode
			if status == http.StatusNotFound && method == http.MethodPost {
				response.Body.Close()
				cancel()
				return errLeaseGone
			}
			if status >= 200 && status < 300 {
				if result != nil {
					decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
					response.Body.Close()
					cancel()
					if decodeErr != nil {
						return errors.New("decode Vault response")
					}
				} else {
					response.Body.Close()
					cancel()
				}
				return nil
			}
			response.Body.Close()
			requestErr = httpStatusError(status)
		}
		cancel()
		if attempt == c.maxRetries || !retryable(requestErr) {
			return requestErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.retryBackoff * time.Duration(1<<min(attempt, 5))):
		}
	}
	return errors.New("Vault request retries exhausted")
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var statusError httpStatusError
	if errors.As(err, &statusError) {
		return int(statusError) == http.StatusTooManyRequests || int(statusError) >= http.StatusInternalServerError
	}
	return true
}

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("Vault returned HTTP %d", int(e)) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
