// Package telemetry exposes the four bounded domain metrics in MONITORING.md.
package telemetry

import (
	"strings"
	"sync"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	mu               sync.RWMutex
	now              func() time.Time
	rotations, vault *prometheus.CounterVec
	pending, lease   *prometheus.Desc
	phases           map[state.Stage]int
	expires          map[string]time.Time
}

var stages = []state.Stage{state.StageIssued, state.StageWaitingForSecret, state.StagePasswordPatched, state.StageReconnectPending, state.StageVerified, state.StageReplacementBackoff}

func New(now func() time.Time) *Metrics {
	return &Metrics{now: now, rotations: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vault_replica_rotations_total", Help: "Completed rotation outcomes."}, []string{"result", "trigger"}), vault: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vault_replica_vault_operations_total", Help: "Vault operation outcomes."}, []string{"operation", "result"}), pending: prometheus.NewDesc("vault_replica_pending_workflows", "Pending workflows by phase.", []string{"phase"}, nil), lease: prometheus.NewDesc("vault_replica_current_lease_time_to_expiration_seconds", "Current lease lifetime remaining at collection time.", []string{"namespace", "cluster"}, nil), phases: map[state.Stage]int{}, expires: map[string]time.Time{}}
}
func (m *Metrics) Rotation(result, trigger string) {
	if result != "success" && result != "failure" {
		return
	}
	if trigger != "initialization" && trigger != "topology_change" && trigger != "pod_replacement" {
		return
	}
	m.rotations.WithLabelValues(result, trigger).Inc()
}
func (m *Metrics) Vault(operation, result string) {
	if operation != "issue" && operation != "revoke" {
		return
	}
	if result != "success" && result != "failure" {
		return
	}
	m.vault.WithLabelValues(operation, result).Inc()
}
func (m *Metrics) Set(s state.Store, namespaces []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phases = map[state.Stage]int{}
	m.expires = map[string]time.Time{}
	allowed := map[string]bool{}
	for _, ns := range namespaces {
		allowed[ns] = true
	}
	for key, c := range s.Clusters {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 || !allowed[parts[0]] {
			continue
		}
		if c.CurrentLeaseID != "" && c.CurrentExpiresAt != nil {
			m.expires[key] = *c.CurrentExpiresAt
		}
		if c.Pending != nil {
			m.phases[c.Pending.Stage]++
		}
	}
}
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	m.rotations.Describe(ch)
	m.vault.Describe(ch)
	ch <- m.pending
	ch <- m.lease
}
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.rotations.Collect(ch)
	m.vault.Collect(ch)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, stage := range stages {
		ch <- prometheus.MustNewConstMetric(m.pending, prometheus.GaugeValue, float64(m.phases[stage]), string(stage))
	}
	for key, expiry := range m.expires {
		parts := strings.SplitN(key, "/", 2)
		ch <- prometheus.MustNewConstMetric(m.lease, prometheus.GaugeValue, expiry.Sub(m.now()).Seconds(), parts[0], parts[1])
	}
}
