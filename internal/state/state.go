// Package state defines and validates the compact, password-free state journal
// described by DESIGN.md.
package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const CurrentVersion = 1

// Stage identifies a durable pending workflow phase.
type Stage string

const (
	StageIssued             Stage = "issued"
	StageWaitingForSecret   Stage = "waiting-secret"
	StagePasswordPatched    Stage = "password-patched"
	StageReconnectPending   Stage = "reconnect-pending"
	StageVerified           Stage = "verified"
	StageReplacementBackoff Stage = "replacement-backoff"
)

// Store is the decoded state.json payload. It deliberately does not duplicate
// Cluster configuration that can be reconstructed from Kubernetes.
type Store struct {
	Version  int                     `json:"version"`
	Clusters map[string]ClusterState `json:"clusters"`
}

// ClusterState is the durable identity and lease state for one Cluster.
type ClusterState struct {
	ClusterUID       string           `json:"clusterUID"`
	CurrentLeaseID   string           `json:"currentLeaseID,omitempty"`
	CurrentExpiresAt *time.Time       `json:"currentExpiresAt,omitempty"`
	Pending          *PendingRotation `json:"pending,omitempty"`
	LastEvent        string           `json:"lastEvent,omitempty"`
}

// PendingRotation contains only the metadata needed to recover a workflow.
// Passwords must never be added to this type.
type PendingRotation struct {
	LeaseID       string     `json:"leaseID"`
	Username      string     `json:"username"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	Stage         Stage      `json:"stage"`
	StageDeadline time.Time  `json:"stageDeadline"`
	TriggerID     string     `json:"triggerID"`
	NextActionAt  *time.Time `json:"nextActionAt,omitempty"`
}

var validStages = map[Stage]struct{}{
	StageIssued: {}, StageWaitingForSecret: {}, StagePasswordPatched: {},
	StageReconnectPending: {}, StageVerified: {}, StageReplacementBackoff: {},
}

// ValidStage reports whether a stage is part of the durable workflow schema.
func ValidStage(stage Stage) bool { _, ok := validStages[stage]; return ok }

func IsValidStage(stage Stage) bool { return ValidStage(stage) }

// New returns an empty, valid state document.
func New() Store { return Store{Version: CurrentVersion, Clusters: map[string]ClusterState{}} }

// Validate rejects old or malformed documents before they can influence
// Kubernetes or Vault. It also prevents accidental expansion of the durable
// schema with credential material or reconstructed resource fields.
func (s Store) Validate() error {
	if s.Version != CurrentVersion {
		return fmt.Errorf("unsupported state version %d", s.Version)
	}
	if s.Clusters == nil {
		return fmt.Errorf("state clusters map is required")
	}
	for key, cluster := range s.Clusters {
		if !validIdentityKey(key) {
			return fmt.Errorf("invalid cluster state key %q", key)
		}
		if cluster.ClusterUID == "" {
			return fmt.Errorf("cluster %q has no clusterUID", key)
		}
		if cluster.CurrentLeaseID == "" && cluster.CurrentExpiresAt != nil {
			return fmt.Errorf("cluster %q has expiry without current lease", key)
		}
		if cluster.CurrentExpiresAt != nil && cluster.CurrentExpiresAt.IsZero() {
			return fmt.Errorf("cluster %q has empty current expiry", key)
		}
		if cluster.CurrentLeaseID != "" && cluster.CurrentExpiresAt == nil {
			return fmt.Errorf("cluster %q has current lease without expiry", key)
		}
		if cluster.Pending == nil {
			continue
		}
		p := cluster.Pending
		if p.LeaseID == "" || p.Username == "" || p.ExpiresAt.IsZero() || p.StageDeadline.IsZero() || p.TriggerID == "" {
			return fmt.Errorf("cluster %q has incomplete pending rotation", key)
		}
		if _, ok := validStages[p.Stage]; !ok {
			return fmt.Errorf("cluster %q has invalid pending stage %q", key, p.Stage)
		}
		if p.NextActionAt != nil && p.NextActionAt.IsZero() {
			return fmt.Errorf("cluster %q has empty nextActionAt", key)
		}
	}
	return nil
}

// Marshal validates and encodes state. maxBytes is an application ceiling on
// decoded JSON, as required by the design; zero means no caller-specific
// ceiling.
func (s Store) Marshal(maxBytes int) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal state: %w", err)
	}
	if maxBytes > 0 && len(raw) > maxBytes {
		return nil, fmt.Errorf("state JSON is %d bytes, exceeds %d-byte ceiling", len(raw), maxBytes)
	}
	return raw, nil
}

// Unmarshal decodes one state document with unknown-field rejection. Strict
// decoding is intentional: state is a recovery journal, not an extension
// point, and silently accepting a credential-bearing field would be unsafe.
func Unmarshal(raw []byte, maxBytes int) (Store, error) {
	if maxBytes > 0 && len(raw) > maxBytes {
		return Store{}, fmt.Errorf("state JSON exceeds %d-byte ceiling", maxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var store Store
	if err := decoder.Decode(&store); err != nil {
		return Store{}, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Store{}, fmt.Errorf("state contains trailing JSON")
	}
	// Version zero was the pre-journal representation and had the same
	// password-free fields but omitted the envelope version. Migrate it in
	// memory before validation; no legacy fields are accepted.
	if store.Version == 0 {
		store.Version = CurrentVersion
	}
	if err := store.Validate(); err != nil {
		return Store{}, err
	}
	return store, nil
}

// Migrate upgrades an already decoded state document. Keeping migration
// explicit makes future schema changes reviewable and prevents callers from
// bypassing validation.
func Migrate(store Store) (Store, error) {
	if store.Version == 0 {
		store.Version = CurrentVersion
	}
	if err := store.Validate(); err != nil {
		return Store{}, err
	}
	return store, nil
}

// LoadSecret reads the state key from a Secret. This helper is only for the
// dedicated controller state Secret; callers must never use it for target
// credential Secrets.
func LoadSecret(secret *corev1.Secret, key string, maxBytes int) (Store, error) {
	if secret == nil {
		return Store{}, fmt.Errorf("state Secret is nil")
	}
	if key == "" {
		return Store{}, fmt.Errorf("state Secret key is required")
	}
	if secret.Data == nil {
		return Store{}, fmt.Errorf("state Secret has no data")
	}
	raw, ok := secret.Data[key]
	if !ok || len(raw) == 0 {
		return Store{}, fmt.Errorf("state Secret key %q is missing", key)
	}
	return Unmarshal(raw, maxBytes)
}

func (s Store) SecretData(key string, maxBytes int) (map[string][]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("state Secret key is required")
	}
	raw, err := s.Marshal(maxBytes)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{key: raw}, nil
}

func validIdentityKey(value string) bool {
	if strings.Count(value, "/") != 1 {
		return false
	}
	parts := strings.SplitN(value, "/", 2)
	return len(validation.IsDNS1123Label(parts[0])) == 0 && len(validation.IsDNS1123Subdomain(parts[1])) == 0
}
