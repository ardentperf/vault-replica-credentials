// Package vault implements the narrow, redacting Vault HTTP boundary used by
// the reconciler. It deliberately supports issuance and revocation only.
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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const maxResponseBytes = 1 << 20

// Credential is the response from the Vault database role endpoint. Password
// is process-memory-only and must never be serialized, logged, or emitted.
type Credential struct {
	Username      string
	Password      string
	LeaseID       string
	LeaseDuration time.Duration
	ExpiresAt     time.Time
}

// Client is the complete Vault API needed by the controller. Lease renewal is
// intentionally absent.
type Client interface {
	IssueDatabaseCredential(ctx context.Context, role string) (Credential, error)
	RevokeLease(ctx context.Context, leaseID string) error
}

// HTTPConfig configures a bounded HTTP client. Token is kept private by the
// implementation and is never included in an error.
type HTTPConfig struct {
	Address        string
	Token          string
	MinLease       time.Duration
	SafetyMargin   time.Duration
	RequestTimeout time.Duration
	RetryAttempts  int
	RetryBackoff   time.Duration
	HTTPClient     *http.Client
	Now            func() time.Time
	Wait           func(context.Context, time.Duration) error
}

// HTTPClient talks to Vault's JSON API without depending on the broad Vault
// SDK. Returned errors contain only operation and HTTP status information.
type HTTPClient struct {
	baseURL        *url.URL
	token          string
	minLease       time.Duration
	safetyMargin   time.Duration
	requestTimeout time.Duration
	retryAttempts  int
	retryBackoff   time.Duration
	httpClient     *http.Client
	now            func() time.Time
	wait           func(context.Context, time.Duration) error
}

// NewHTTPClient validates configuration and returns a ready client.
func NewHTTPClient(config HTTPConfig) (*HTTPClient, error) {
	baseURL, err := url.Parse(config.Address)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("Vault address must be an absolute URL")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("Vault token is required")
	}
	if config.MinLease <= 0 || config.SafetyMargin <= 0 || config.SafetyMargin >= config.MinLease {
		return nil, errors.New("Vault lease bounds are invalid")
	}
	if config.RequestTimeout <= 0 || config.RetryAttempts <= 0 || config.RetryBackoff <= 0 {
		return nil, errors.New("Vault request retry policy is invalid")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Wait == nil {
		config.Wait = waitContext
	}
	return &HTTPClient{
		baseURL:        baseURL,
		token:          config.Token,
		minLease:       config.MinLease,
		safetyMargin:   config.SafetyMargin,
		requestTimeout: config.RequestTimeout,
		retryAttempts:  config.RetryAttempts,
		retryBackoff:   config.RetryBackoff,
		httpClient:     config.HTTPClient,
		now:            config.Now,
		wait:           config.Wait,
	}, nil
}

// IssueDatabaseCredential issues from the role named by the selected source
// CNPG Cluster. Role is constrained to a Kubernetes DNS label so it is exactly
// one Vault path segment.
func (client *HTTPClient) IssueDatabaseCredential(ctx context.Context, role string) (Credential, error) {
	if errs := validation.IsDNS1123Label(role); len(errs) != 0 {
		return Credential{}, errors.New("Vault database role is invalid")
	}

	response, err := client.do(ctx, http.MethodGet, "v1/database/creds/"+url.PathEscape(role), nil)
	if err != nil {
		return Credential{}, fmt.Errorf("issue Vault database credential: %w", err)
	}
	defer response.Body.Close()

	var payload struct {
		LeaseID       string          `json:"lease_id"`
		LeaseDuration json.Number     `json:"lease_duration"`
		Data          json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return Credential{}, errors.New("issue Vault database credential: malformed response")
	}
	var data struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(payload.Data, &data); err != nil || data.Username == "" || data.Password == "" || payload.LeaseID == "" {
		return Credential{}, errors.New("issue Vault database credential: incomplete response")
	}
	seconds, err := payload.LeaseDuration.Int64()
	if err != nil || seconds <= 0 {
		return Credential{}, errors.New("issue Vault database credential: invalid lease duration")
	}
	duration := time.Duration(seconds) * time.Second
	if duration < client.minLease || duration <= client.safetyMargin {
		return Credential{}, errors.New("issue Vault database credential: returned lease is too short")
	}
	now := client.now().UTC()
	return Credential{
		Username:      data.Username,
		Password:      data.Password,
		LeaseID:       payload.LeaseID,
		LeaseDuration: duration,
		ExpiresAt:     now.Add(duration),
	}, nil
}

// RevokeLease revokes a complete lease ID. Vault's successful response is
// empty; the request body is never included in an error.
func (client *HTTPClient) RevokeLease(ctx context.Context, leaseID string) error {
	if strings.TrimSpace(leaseID) == "" {
		return errors.New("revoke Vault lease: lease ID is required")
	}
	body, err := json.Marshal(map[string]string{"lease_id": leaseID})
	if err != nil {
		return errors.New("revoke Vault lease: encode request")
	}
	response, err := client.do(ctx, http.MethodPut, "v1/sys/leases/revoke", body)
	if err != nil {
		return fmt.Errorf("revoke Vault lease: %w", err)
	}
	response.Body.Close()
	return nil
}

func (client *HTTPClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	target := *client.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + path

	var lastStatus int
	for attempt := 0; attempt < client.retryAttempts; attempt++ {
		requestContext, cancel := context.WithTimeout(ctx, client.requestTimeout)
		request, err := http.NewRequestWithContext(requestContext, method, target.String(), bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, errors.New("construct request")
		}
		request.Header.Set("X-Vault-Token", client.token)
		request.Header.Set("Accept", "application/json")
		if len(body) != 0 {
			request.Header.Set("Content-Type", "application/json")
		}

		response, requestErr := client.httpClient.Do(request)
		if requestErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
			response.Body.Close()
			cancel()
			if readErr != nil || len(responseBody) > maxResponseBytes {
				return nil, errors.New("response body is invalid")
			}
			response.Body = io.NopCloser(bytes.NewReader(responseBody))
			return response, nil
		}
		cancel()
		if response != nil {
			lastStatus = response.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
			response.Body.Close()
			if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
				return nil, fmt.Errorf("HTTP status %d", response.StatusCode)
			}
		}
		if attempt+1 < client.retryAttempts {
			if err := client.wait(ctx, client.retryBackoff*time.Duration(1<<attempt)); err != nil {
				return nil, errors.New("request canceled")
			}
		}
	}
	if lastStatus != 0 {
		return nil, fmt.Errorf("HTTP status %d after bounded retries", lastStatus)
	}
	return nil, errors.New("request failed after bounded retries")
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
