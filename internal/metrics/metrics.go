// Package metrics contains the small, bounded domain metric set required by
// MONITORING.md. It deliberately has no labels derived from credentials.
package metrics

import (
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

const (
	RotationSuccess = "success"
	RotationFailure = "failure"
	VaultIssue      = "issue"
	VaultRevoke     = "revoke"
)

// Metrics owns domain collectors. A caller can use a private registry for
// tests, or controller-runtime's registry in the running manager.
type Metrics struct {
	rotations *prometheus.CounterVec
	vaultOps  *prometheus.CounterVec
	pending   *prometheus.GaugeVec
	leases    *leaseCollector
}

func New(registerer prometheus.Registerer, now func() time.Time) *Metrics {
	if now == nil {
		now = time.Now
	}
	m := &Metrics{
		rotations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vault_replica_rotations_total",
			Help: "Completed replica credential rotation outcomes.",
		}, []string{"result", "trigger"}),
		vaultOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vault_replica_vault_operations_total",
			Help: "Vault credential issue and lease revoke outcomes.",
		}, []string{"operation", "result"}),
		pending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vault_replica_pending_workflows",
			Help: "Current non-terminal replica credential workflows by phase.",
		}, []string{"phase"}),
		leases: newLeaseCollector(now),
	}
	if registerer != nil {
		m.rotations = registerCounter(registerer, m.rotations)
		m.vaultOps = registerCounter(registerer, m.vaultOps)
		m.pending = registerGauge(registerer, m.pending)
		m.leases = registerLeases(registerer, m.leases)
	}
	return m
}

func registerCounter(registerer prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := registerer.Register(collector); err != nil {
		if registered, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := registered.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
	}
	return collector
}

func registerGauge(registerer prometheus.Registerer, collector *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := registerer.Register(collector); err != nil {
		if registered, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := registered.ExistingCollector.(*prometheus.GaugeVec); ok {
				return existing
			}
		}
	}
	return collector
}

func registerLeases(registerer prometheus.Registerer, collector *leaseCollector) *leaseCollector {
	if err := registerer.Register(collector); err != nil {
		if registered, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := registered.ExistingCollector.(*leaseCollector); ok {
				return existing
			}
		}
	}
	return collector
}

func (m *Metrics) Rotation(result, trigger string) {
	if m == nil || (result != RotationSuccess && result != RotationFailure) {
		return
	}
	m.rotations.WithLabelValues(result, boundedTrigger(trigger)).Inc()
}

func (m *Metrics) VaultOperation(operation, result string) {
	if m == nil || (operation != VaultIssue && operation != VaultRevoke) || (result != RotationSuccess && result != RotationFailure) {
		return
	}
	m.vaultOps.WithLabelValues(operation, result).Inc()
}

// ObserveState replaces derived gauges atomically. Time-to-expiration is
// calculated by the collector at scrape time, not only at reconciliation time.
func (m *Metrics) ObserveState(store state.Store) {
	if m == nil {
		return
	}
	phaseCounts := make(map[string]float64)
	leases := make(map[string]time.Time)
	for key, entry := range store.Clusters {
		if entry.Pending != nil {
			phaseCounts[string(entry.Pending.Stage)]++
		}
		if entry.CurrentExpiresAt != nil {
			leases[key] = *entry.CurrentExpiresAt
		}
	}
	m.pending.Reset()
	for phase, count := range phaseCounts {
		m.pending.WithLabelValues(phase).Set(count)
	}
	m.leases.replace(leases)
}

func boundedTrigger(trigger string) string {
	switch trigger {
	case "initialization", "topology_change", "pod_replacement":
		return trigger
	default:
		return "topology_change"
	}
}

type leaseCollector struct {
	desc *prometheus.Desc
	now  func() time.Time
	mu   sync.RWMutex
	data map[string]time.Time
}

func newLeaseCollector(now func() time.Time) *leaseCollector {
	return &leaseCollector{
		desc: prometheus.NewDesc("vault_replica_current_lease_time_to_expiration_seconds", "Remaining lifetime of each active replica Vault lease.", []string{"namespace", "cluster"}, nil),
		now:  now,
		data: map[string]time.Time{},
	}
}

func (c *leaseCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *leaseCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := c.now()
	for key, expiration := range c.data {
		namespace, cluster, ok := strings.Cut(key, "/")
		if !ok || namespace == "" || cluster == "" {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, expiration.Sub(now).Seconds(), namespace, cluster)
	}
}

func (c *leaseCollector) replace(data map[string]time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = make(map[string]time.Time, len(data))
	for key, value := range data {
		c.data[key] = value
	}
}
