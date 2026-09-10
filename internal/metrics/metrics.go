// Package metrics exposes only the domain metrics required by DESIGN.md.
package metrics

import (
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

var (
	rotationResults  = map[string]bool{"success": true, "failure": true}
	rotationTriggers = map[string]bool{"initialization": true, "topology_change": true, "pod_replacement": true}
	vaultOperations  = map[string]bool{"issue": true, "revoke": true}
	workflowPhases   = []state.Stage{
		state.StageIssued, state.StageWaitingForSecret, state.StagePasswordPatched,
		state.StageReconnectPending, state.StageVerified, state.StageReplacementBackoff,
	}
)

// Recorder owns bounded counters and a scrape-time state collector.
type Recorder struct {
	rotations *prometheus.CounterVec
	vault     *prometheus.CounterVec
	state     *stateCollector
}

// New registers a fresh metric set with the supplied registry.
func New(registerer prometheus.Registerer, now func() time.Time) (*Recorder, error) {
	if registerer == nil {
		return nil, errors.New("metrics registerer is required")
	}
	if now == nil {
		now = time.Now
	}
	recorder := &Recorder{
		rotations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vault_replica_rotations_total", Help: "Completed credential rotation outcomes.",
		}, []string{"result", "trigger"}),
		vault: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vault_replica_vault_operations_total", Help: "Vault operation outcomes.",
		}, []string{"operation", "result"}),
		state: newStateCollector(now),
	}
	for _, collector := range []prometheus.Collector{recorder.rotations, recorder.vault, recorder.state} {
		if err := registerer.Register(collector); err != nil {
			return nil, err
		}
	}
	return recorder, nil
}

// RecordRotation increments one finite-label rotation outcome.
func (recorder *Recorder) RecordRotation(result, trigger string) error {
	if !rotationResults[result] || !rotationTriggers[trigger] {
		return errors.New("invalid rotation metric label")
	}
	recorder.rotations.WithLabelValues(result, trigger).Inc()
	return nil
}

// RecordVault increments one finite-label Vault operation outcome.
func (recorder *Recorder) RecordVault(operation, result string) error {
	if !vaultOperations[operation] || !rotationResults[result] {
		return errors.New("invalid Vault metric label")
	}
	recorder.vault.WithLabelValues(operation, result).Inc()
	return nil
}

// SetState replaces the in-memory metric view after every journal read/write.
func (recorder *Recorder) SetState(store state.Store) {
	recorder.state.set(store)
}

type leaseExpiry struct {
	namespace string
	cluster   string
	expiresAt time.Time
}

type stateCollector struct {
	mu          sync.RWMutex
	now         func() time.Time
	pending     map[state.Stage]float64
	leases      []leaseExpiry
	pendingDesc *prometheus.Desc
	leaseDesc   *prometheus.Desc
}

func newStateCollector(now func() time.Time) *stateCollector {
	return &stateCollector{
		now: now, pending: map[state.Stage]float64{},
		pendingDesc: prometheus.NewDesc("vault_replica_pending_workflows", "Non-terminal workflows by bounded phase.", []string{"phase"}, nil),
		leaseDesc:   prometheus.NewDesc("vault_replica_current_lease_time_to_expiration_seconds", "Remaining current lease lifetime calculated at scrape time.", []string{"namespace", "cluster"}, nil),
	}
}

func (collector *stateCollector) set(store state.Store) {
	pending := map[state.Stage]float64{}
	for _, phase := range workflowPhases {
		pending[phase] = 0
	}
	leases := make([]leaseExpiry, 0, len(store.Clusters))
	for key, cluster := range store.Clusters {
		if cluster.Pending != nil {
			pending[cluster.Pending.Stage]++
		}
		if cluster.CurrentExpiresAt != nil {
			namespace, name := splitKey(key)
			if namespace != "" && name != "" {
				leases = append(leases, leaseExpiry{namespace: namespace, cluster: name, expiresAt: *cluster.CurrentExpiresAt})
			}
		}
	}
	collector.mu.Lock()
	collector.pending = pending
	collector.leases = leases
	collector.mu.Unlock()
}

func (collector *stateCollector) Describe(channel chan<- *prometheus.Desc) {
	channel <- collector.pendingDesc
	channel <- collector.leaseDesc
}

func (collector *stateCollector) Collect(channel chan<- prometheus.Metric) {
	collector.mu.RLock()
	pending := make(map[state.Stage]float64, len(collector.pending))
	for phase, value := range collector.pending {
		pending[phase] = value
	}
	leases := append([]leaseExpiry(nil), collector.leases...)
	collector.mu.RUnlock()
	for _, phase := range workflowPhases {
		channel <- prometheus.MustNewConstMetric(collector.pendingDesc, prometheus.GaugeValue, pending[phase], string(phase))
	}
	now := collector.now()
	for _, lease := range leases {
		channel <- prometheus.MustNewConstMetric(collector.leaseDesc, prometheus.GaugeValue, lease.expiresAt.Sub(now).Seconds(), lease.namespace, lease.cluster)
	}
}

func splitKey(key string) (string, string) {
	for index := range key {
		if key[index] == '/' {
			return key[:index], key[index+1:]
		}
	}
	return "", ""
}
