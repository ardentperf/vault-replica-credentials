// Package metrics contains only domain metrics that are not supplied by
// controller-runtime, client-go, or the Go runtime.
package metrics

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

var allowedTriggers = map[string]struct{}{"initialization": {}, "topology_change": {}, "pod_replacement": {}}
var allowedResults = map[string]struct{}{"success": {}, "failure": {}}
var allowedOperations = map[string]struct{}{"issue": {}, "revoke": {}}

type Metrics struct {
	rotations *prometheus.CounterVec
	vaultOps  *prometheus.CounterVec
	pending   *prometheus.GaugeVec
	leases    *leaseCollector
	mu        sync.Mutex
	pendingBy map[string]string
}

// NewMetrics is the descriptive alias used by integration tests and
// embedding applications.
func NewMetrics(registerer prometheus.Registerer, now func() time.Time) (*Metrics, error) {
	return New(registerer, now)
}

func New(registerer prometheus.Registerer, now func() time.Time) (*Metrics, error) {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	if now == nil {
		now = time.Now
	}
	m := &Metrics{
		rotations: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vault_replica_rotations_total", Help: "Completed Vault replica credential rotations."}, []string{"result", "trigger"}),
		vaultOps:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vault_replica_vault_operations_total", Help: "Vault issue and revoke operations."}, []string{"operation", "result"}),
		pending:   prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "vault_replica_pending_workflows", Help: "Current non-terminal Vault replica workflows."}, []string{"phase"}),
		leases:    newLeaseCollector(now), pendingBy: make(map[string]string),
	}
	if existing, err := registerOrExisting(registerer, m.rotations); err != nil {
		return nil, fmt.Errorf("register rotations metric: %w", err)
	} else if existing != nil {
		m.rotations = existing.(*prometheus.CounterVec)
	}
	if existing, err := registerOrExisting(registerer, m.vaultOps); err != nil {
		return nil, fmt.Errorf("register Vault metric: %w", err)
	} else if existing != nil {
		m.vaultOps = existing.(*prometheus.CounterVec)
	}
	if existing, err := registerOrExisting(registerer, m.pending); err != nil {
		return nil, fmt.Errorf("register pending metric: %w", err)
	} else if existing != nil {
		m.pending = existing.(*prometheus.GaugeVec)
	}
	if existing, err := registerOrExisting(registerer, m.leases); err != nil {
		return nil, fmt.Errorf("register lease metric: %w", err)
	} else if existing != nil {
		m.leases = existing.(*leaseCollector)
	}
	return m, nil
}

func registerOrExisting(registerer prometheus.Registerer, collector prometheus.Collector) (prometheus.Collector, error) {
	if err := registerer.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector, nil
		}
		return nil, err
	}
	return nil, nil
}

func (m *Metrics) RecordRotation(result, trigger string) {
	if m == nil {
		return
	}
	if _, ok := allowedResults[result]; !ok {
		result = "failure"
	}
	if _, ok := allowedTriggers[trigger]; !ok {
		trigger = "topology_change"
	}
	m.rotations.WithLabelValues(result, trigger).Inc()
}

func (m *Metrics) RecordVault(operation, result string) {
	if m == nil {
		return
	}
	if _, ok := allowedOperations[operation]; !ok {
		return
	}
	if _, ok := allowedResults[result]; !ok {
		result = "failure"
	}
	m.vaultOps.WithLabelValues(operation, result).Inc()
}

func (m *Metrics) SetPending(key string, phase state.Stage) {
	if m == nil || key == "" {
		return
	}
	if !state.ValidStage(phase) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.pendingBy[key]; ok && old == string(phase) {
		return
	}
	if old, ok := m.pendingBy[key]; ok {
		m.pending.WithLabelValues(old).Dec()
	}
	m.pendingBy[key] = string(phase)
	m.pending.WithLabelValues(string(phase)).Inc()
}

func (m *Metrics) ClearPending(key string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.pendingBy[key]; ok {
		m.pending.WithLabelValues(old).Dec()
		delete(m.pendingBy, key)
	}
}

func (m *Metrics) SetLease(namespace, cluster string, expiresAt time.Time) {
	if m == nil {
		return
	}
	m.leases.set(types.NamespacedName{Namespace: namespace, Name: cluster}, expiresAt)
}

func (m *Metrics) DeleteLease(namespace, cluster string) {
	if m == nil {
		return
	}
	m.leases.delete(types.NamespacedName{Namespace: namespace, Name: cluster})
}

type leaseCollector struct {
	desc  *prometheus.Desc
	now   func() time.Time
	mu    sync.RWMutex
	items map[types.NamespacedName]time.Time
}

func newLeaseCollector(now func() time.Time) *leaseCollector {
	return &leaseCollector{desc: prometheus.NewDesc("vault_replica_current_lease_time_to_expiration_seconds", "Remaining lifetime of the current Vault lease.", []string{"namespace", "cluster"}, nil), now: now, items: map[types.NamespacedName]time.Time{}}
}
func (c *leaseCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c *leaseCollector) Collect(ch chan<- prometheus.Metric) {
	now := c.now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	for identity, expiry := range c.items {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, expiry.Sub(now).Seconds(), identity.Namespace, identity.Name)
	}
}
func (c *leaseCollector) set(identity types.NamespacedName, expiry time.Time) {
	c.mu.Lock()
	c.items[identity] = expiry
	c.mu.Unlock()
}
func (c *leaseCollector) delete(identity types.NamespacedName) {
	c.mu.Lock()
	delete(c.items, identity)
	c.mu.Unlock()
}
