// Package state defines and persists the compact, password-free state journal
// described by DESIGN.md.
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

// Repository reads and atomically updates the one pre-created state Secret.
// Mutations use normal Kubernetes optimistic concurrency retries.
type Repository struct {
	client   client.Client
	secret   types.NamespacedName
	key      string
	maxBytes int
}

// NewRepository creates a state repository. It never creates the Secret.
func NewRepository(kubeClient client.Client, secret types.NamespacedName, key string, maxBytes int) (*Repository, error) {
	if kubeClient == nil || secret.Namespace == "" || secret.Name == "" || strings.TrimSpace(key) == "" || maxBytes <= 0 {
		return nil, errors.New("state repository configuration is invalid")
	}
	return &Repository{client: kubeClient, secret: secret, key: key, maxBytes: maxBytes}, nil
}

// Read loads, decodes, migrates, and validates the current journal.
func (repository *Repository) Read(ctx context.Context) (Store, error) {
	secret := &corev1.Secret{}
	if err := repository.client.Get(ctx, repository.secret, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return Store{}, errors.New("state Secret is not installed")
		}
		return Store{}, fmt.Errorf("read state Secret: %w", err)
	}
	data, ok := secret.Data[repository.key]
	if !ok {
		return Store{}, errors.New("state Secret key is missing")
	}
	return Decode(data, repository.maxBytes)
}

// Mutate applies a pure mutation and writes the whole compact journal. The
// callback can run more than once when another writer wins a resource-version
// race, so callers must not perform external side effects in it.
func (repository *Repository) Mutate(ctx context.Context, mutate func(*Store) error) (Store, error) {
	if mutate == nil {
		return Store{}, errors.New("state mutation is required")
	}
	var result Store
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		secret := &corev1.Secret{}
		if err := repository.client.Get(ctx, repository.secret, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return errors.New("state Secret is not installed")
			}
			return err
		}
		data, ok := secret.Data[repository.key]
		if !ok {
			return errors.New("state Secret key is missing")
		}
		store, err := Decode(data, repository.maxBytes)
		if err != nil {
			return err
		}
		if err := mutate(&store); err != nil {
			return err
		}
		store.Version = CurrentVersion
		if store.Clusters == nil {
			store.Clusters = map[string]ClusterState{}
		}
		encoded, err := Encode(store, repository.maxBytes)
		if err != nil {
			return err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[repository.key] = encoded
		if err := repository.client.Update(ctx, secret); err != nil {
			return err
		}
		result = store
		return nil
	})
	if err != nil {
		return Store{}, fmt.Errorf("update state journal: %w", err)
	}
	return result, nil
}

// Decode strictly decodes a journal. Version zero is the only supported
// migration: the original unversioned empty/journal shape becomes version 1.
func Decode(data []byte, maxBytes int) (Store, error) {
	if maxBytes <= 0 || len(data) > maxBytes {
		return Store{}, errors.New("state journal exceeds configured size ceiling")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var store Store
	if err := decoder.Decode(&store); err != nil {
		return Store{}, errors.New("state journal is malformed")
	}
	if err := ensureEOF(decoder); err != nil {
		return Store{}, err
	}
	if store.Version == 0 {
		store.Version = CurrentVersion
	}
	if store.Clusters == nil {
		store.Clusters = map[string]ClusterState{}
	}
	if err := Validate(store); err != nil {
		return Store{}, err
	}
	return store, nil
}

// Encode validates and encodes a compact journal under its application size
// ceiling.
func Encode(store Store, maxBytes int) ([]byte, error) {
	if err := Validate(store); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(store)
	if err != nil {
		return nil, errors.New("encode state journal")
	}
	if maxBytes <= 0 || len(encoded) > maxBytes {
		return nil, errors.New("state journal exceeds configured size ceiling")
	}
	return encoded, nil
}

// Validate enforces the complete durable schema and field relationships.
func Validate(store Store) error {
	if store.Version != CurrentVersion {
		return fmt.Errorf("unsupported state journal version %d", store.Version)
	}
	validStages := map[Stage]bool{
		StageIssued: true, StageWaitingForSecret: true, StagePasswordPatched: true,
		StageReconnectPending: true, StageVerified: true, StageReplacementBackoff: true,
	}
	for key, cluster := range store.Clusters {
		parts := strings.Split(key, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || cluster.ClusterUID == "" {
			return errors.New("state journal contains an invalid Cluster identity")
		}
		if (cluster.CurrentLeaseID == "") != (cluster.CurrentExpiresAt == nil) {
			return errors.New("state journal current lease metadata is incomplete")
		}
		if cluster.CurrentExpiresAt != nil && cluster.CurrentExpiresAt.IsZero() {
			return errors.New("state journal current lease expiration is invalid")
		}
		if pending := cluster.Pending; pending != nil {
			if pending.LeaseID == "" || pending.Username == "" || pending.TriggerID == "" ||
				pending.ExpiresAt.IsZero() || pending.StageDeadline.IsZero() || !validStages[pending.Stage] {
				return errors.New("state journal pending workflow is incomplete")
			}
			if pending.NextActionAt != nil && pending.NextActionAt.IsZero() {
				return errors.New("state journal next action time is invalid")
			}
		}
	}
	return nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("state journal contains trailing data")
	}
	return nil
}
