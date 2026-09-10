// Package state defines and persists the compact, password-free journal used
// to recover rotation workflows after a retry or controller restart.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// New returns the empty current schema. Callers should always use this rather
// than constructing a Store with a nil map.
func New() Store {
	return Store{Version: CurrentVersion, Clusters: map[string]ClusterState{}}
}

// Decode validates a state payload before it is acted on. Disallowing unknown
// JSON fields makes accidental persistence of credential material fail closed.
func Decode(raw []byte, maxBytes int) (Store, error) {
	if maxBytes <= 0 {
		return Store{}, errors.New("state maximum size must be positive")
	}
	if len(raw) > maxBytes {
		return Store{}, fmt.Errorf("state is %d bytes, over configured %d byte limit", len(raw), maxBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Store{}, errors.New("state payload is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var store Store
	if err := decoder.Decode(&store); err != nil {
		return Store{}, fmt.Errorf("decode state: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Store{}, err
	}
	if err := Validate(store); err != nil {
		return Store{}, err
	}
	return store, nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("state contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing state JSON: %w", err)
	}
	return nil
}

// Encode validates and serializes state. It is deliberately the only writer
// used by Repository so the application size ceiling is enforced both ways.
func Encode(store Store, maxBytes int) ([]byte, error) {
	if err := Validate(store); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(store)
	if err != nil {
		return nil, fmt.Errorf("encode state: %w", err)
	}
	if maxBytes <= 0 || len(raw) > maxBytes {
		return nil, fmt.Errorf("encoded state is %d bytes, over configured %d byte limit", len(raw), maxBytes)
	}
	return raw, nil
}

// Validate permits only the minimal recovery schema defined by the design.
func Validate(store Store) error {
	if store.Version != CurrentVersion {
		return fmt.Errorf("unsupported state version %d", store.Version)
	}
	if store.Clusters == nil {
		return errors.New("state clusters must not be null")
	}
	for key, entry := range store.Clusters {
		if err := validateKey(key); err != nil {
			return err
		}
		if strings.TrimSpace(entry.ClusterUID) == "" {
			return fmt.Errorf("state entry %q has an empty clusterUID", key)
		}
		if (entry.CurrentLeaseID == "") != (entry.CurrentExpiresAt == nil) {
			return fmt.Errorf("state entry %q must have both current lease ID and expiration or neither", key)
		}
		if entry.Pending != nil {
			if err := validatePending(key, *entry.Pending); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateKey(key string) error {
	parts := strings.Split(key, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("state cluster key %q must be namespace/name", key)
	}
	return nil
}

func validatePending(key string, pending PendingRotation) error {
	if strings.TrimSpace(pending.LeaseID) == "" || strings.TrimSpace(pending.Username) == "" || strings.TrimSpace(pending.TriggerID) == "" {
		return fmt.Errorf("state entry %q has incomplete pending rotation", key)
	}
	if pending.ExpiresAt.IsZero() || pending.StageDeadline.IsZero() {
		return fmt.Errorf("state entry %q has pending rotation without expiry or deadline", key)
	}
	switch pending.Stage {
	case StageIssued, StageWaitingForSecret, StagePasswordPatched, StageReconnectPending, StageVerified, StageReplacementBackoff:
	default:
		return fmt.Errorf("state entry %q has invalid pending stage %q", key, pending.Stage)
	}
	return nil
}

// Repository owns updates to the one pre-created state Secret. It never
// creates the Secret, which preserves the RBAC and installation boundary.
type Repository struct {
	client    client.Client
	namespace string
	name      string
	key       string
	maxBytes  int
}

func NewRepository(c client.Client, namespace, name, key string, maxBytes int) (*Repository, error) {
	if c == nil || namespace == "" || name == "" || key == "" || maxBytes <= 0 {
		return nil, errors.New("state repository requires client, namespace, name, key, and positive size limit")
	}
	return &Repository{client: c, namespace: namespace, name: name, key: key, maxBytes: maxBytes}, nil
}

func (r *Repository) Load(ctx context.Context) (Store, error) {
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return Store{}, fmt.Errorf("state Secret %s/%s is not pre-created: %w", r.namespace, r.name, err)
		}
		return Store{}, fmt.Errorf("get state Secret: %w", err)
	}
	raw, ok := secret.Data[r.key]
	if !ok {
		return Store{}, fmt.Errorf("state Secret %s/%s does not contain %q", r.namespace, r.name, r.key)
	}
	return Decode(raw, r.maxBytes)
}

// Update retries API resource-version conflicts and writes only the configured
// state key. The mutator receives a value copy so it cannot retain a mutable
// object backed by a cache entry.
func (r *Repository) Update(ctx context.Context, mutate func(*Store) error) (Store, error) {
	if mutate == nil {
		return Store{}, errors.New("state mutator is required")
	}
	var updated Store
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret := &corev1.Secret{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.name}, secret); err != nil {
			return err
		}
		raw, ok := secret.Data[r.key]
		if !ok {
			return fmt.Errorf("state Secret %s/%s does not contain %q", r.namespace, r.name, r.key)
		}
		store, err := Decode(raw, r.maxBytes)
		if err != nil {
			return err
		}
		if err := mutate(&store); err != nil {
			return err
		}
		encoded, err := Encode(store, r.maxBytes)
		if err != nil {
			return err
		}
		if bytes.Equal(encoded, raw) {
			updated = store
			return nil
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[r.key] = encoded
		if err := r.client.Update(ctx, secret); err != nil {
			return err
		}
		updated = store
		return nil
	})
	if err != nil {
		return Store{}, fmt.Errorf("update state Secret: %w", err)
	}
	return updated, nil
}
