// Package state defines the compact, password-free state journal described by
// DESIGN.md. It contains data types only; persistence and reconciliation are
// intentionally not implemented in the initial scaffold.
package state

import "time"

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