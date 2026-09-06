// Package vault defines the narrow Vault boundary needed by the future
// reconciler. It has no network implementation in the scaffolding phase.
package vault

import (
	"context"
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