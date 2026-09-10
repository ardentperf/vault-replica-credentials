// Package vault implements the deliberately small Vault boundary used by the
// reconciler. It only issues database credentials and revokes full leases.
package vault

import (
	"bytes"
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

// Config configures an HTTP Vault client. Token is intentionally not included
// in errors or logs; callers must not print this struct.
type Config struct {
	Address        string
	Token          string
	RequestTimeout time.Duration
	MaxRetries     int
	RetryDelay     time.Duration
	MinLease       time.Duration
	Now            func() time.Time
	HTTPClient     *http.Client
}

type httpClient struct {
	baseURL        *url.URL
	token          string
	requestTimeout time.Duration
	maxRetries     int
	retryDelay     time.Duration
	minLease       time.Duration
	now            func() time.Time
	client         *http.Client
}

// NewHTTPClient validates the HTTP boundary without sending a request.
func NewHTTPClient(config Config) (Client, error) {
	if strings.TrimSpace(config.Address) == "" || strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("Vault address and token are required")
	}
	baseURL, err := url.Parse(config.Address)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("Vault address must be an absolute URL")
	}
	if config.RequestTimeout <= 0 {
		return nil, errors.New("Vault request timeout must be positive")
	}
	if config.MaxRetries < 0 {
		return nil, errors.New("Vault max retries must not be negative")
	}
	if config.RetryDelay <= 0 {
		return nil, errors.New("Vault retry delay must be positive")
	}
	if config.MinLease <= 0 {
		return nil, errors.New("Vault minimum lease must be positive")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	return &httpClient{
		baseURL:        baseURL,
		token:          config.Token,
		requestTimeout: config.RequestTimeout,
		maxRetries:     config.MaxRetries,
		retryDelay:     config.RetryDelay,
		minLease:       config.MinLease,
		now:            config.Now,
		client:         config.HTTPClient,
	}, nil
}

// IssueDatabaseCredential gets /v1/database/creds/<role>. The role is a name,
// not an arbitrary path, so path escaping prevents accidental endpoint escape.
func (c *httpClient) IssueDatabaseCredential(ctx context.Context, role string) (Credential, error) {
	if strings.TrimSpace(role) == "" || strings.Contains(role, "/") {
		return Credential{}, errors.New("Vault database role must be a non-empty name")
	}
	requestURL := c.endpoint("v1", "database", "creds", role)
	response, status, err := c.do(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return Credential{}, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return Credential{}, fmt.Errorf("Vault issue credential returned HTTP %d", status)
	}
	var payload issueResponse
	if err := decodeJSON(response, &payload); err != nil {
		return Credential{}, fmt.Errorf("decode Vault credential response: %w", err)
	}
	if strings.TrimSpace(payload.Data.Username) == "" || payload.Data.Password == "" || strings.TrimSpace(payload.LeaseID) == "" {
		return Credential{}, errors.New("Vault credential response is missing username, password, or lease ID")
	}
	if payload.LeaseDuration <= 0 {
		return Credential{}, errors.New("Vault credential response has non-positive lease duration")
	}
	duration := time.Duration(payload.LeaseDuration) * time.Second
	if duration < c.minLease {
		return Credential{}, fmt.Errorf("Vault lease duration %s is below configured minimum %s", duration, c.minLease)
	}
	return Credential{
		Username:      payload.Data.Username,
		Password:      payload.Data.Password,
		LeaseID:       payload.LeaseID,
		LeaseDuration: duration,
		ExpiresAt:     c.now().UTC().Add(duration),
	}, nil
}

// RevokeLease synchronously revokes the complete lease ID. Vault otherwise
// queues revocation work and can acknowledge a request before a database plugin
// has finished (or failed) the source-side DROP ROLE. A synchronous request
// makes a successful response mean the credential is actually unusable. Vault's
// not-found style responses are success because that postcondition already
// holds.
func (c *httpClient) RevokeLease(ctx context.Context, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return errors.New("Vault lease ID is required")
	}
	body, err := json.Marshal(struct {
		LeaseID string `json:"lease_id"`
		Sync    bool   `json:"sync"`
	}{LeaseID: leaseID, Sync: true})
	if err != nil {
		return fmt.Errorf("encode Vault lease revocation: %w", err)
	}
	_, status, err := c.do(ctx, http.MethodPost, c.endpoint("v1", "sys", "leases", "revoke"), body)
	if err != nil {
		return err
	}
	if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusNoContent || (status >= http.StatusOK && status < http.StatusMultipleChoices) {
		return nil
	}
	return fmt.Errorf("Vault revoke lease returned HTTP %d", status)
}

type issueResponse struct {
	LeaseID       string `json:"lease_id"`
	LeaseDuration int64  `json:"lease_duration"`
	Data          struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"data"`
}

func (c *httpClient) endpoint(parts ...string) string {
	copyURL := *c.baseURL
	all := append([]string{copyURL.Path}, parts...)
	copyURL.Path = path.Join(all...)
	return copyURL.String()
}

func (c *httpClient) do(ctx context.Context, method, requestURL string, body []byte) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		requestContext, cancel := context.WithTimeout(ctx, c.requestTimeout)
		request, err := http.NewRequestWithContext(requestContext, method, requestURL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, 0, fmt.Errorf("create Vault request: %w", err)
		}
		request.Header.Set("X-Vault-Token", c.token)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, requestErr := c.client.Do(request)
		cancel()
		if requestErr == nil {
			responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("read Vault response: %w", readErr)
			} else if response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
				return responseBody, response.StatusCode, nil
			} else {
				lastErr = fmt.Errorf("Vault request returned retryable HTTP %d", response.StatusCode)
			}
		} else {
			lastErr = fmt.Errorf("perform Vault request: %w", requestErr)
		}
		if attempt == c.maxRetries {
			break
		}
		if err := wait(ctx, c.retryDelay*time.Duration(1<<attempt)); err != nil {
			return nil, 0, err
		}
	}
	return nil, 0, lastErr
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	return decoder.Decode(target)
}
