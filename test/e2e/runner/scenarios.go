package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/config"
	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

func (a *actor) phase(name string, fn func() error) error {
	fmt.Println("E2E:", name)
	started := time.Now()
	if err := fn(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	a.artifactWrite(strings.ReplaceAll(name, " ", "-")+".txt", []byte(fmt.Sprintf("PASS (%s)", time.Since(started))))
	return nil
}
func (a *actor) suite() error {
	if _, err := a.vault("POST", "sys/mounts/database", map[string]any{"type": "database", "config": map[string]string{"default_lease_ttl": "768h", "max_lease_ttl": "768h"}}); err != nil {
		return err
	}
	d1, d2 := a.allocate("us", "e2e-first"), a.allocate("eu", "e2e-first")
	d3, d4 := a.allocate("eu", "e2e-first"), a.allocate("us", "e2e-first")
	if err := a.phase("01 initial issuance", func() error {
		if err := a.watch("e2e-bootstrap,e2e-first", true); err != nil {
			return err
		}
		for _, pair := range [][2]*database{{d1, d2}, {d3, d4}} {
			if err := a.createDB(pair[0], nil); err != nil {
				return err
			}
			if err := a.onboard(pair[0]); err != nil {
				return err
			}
			if err := a.createDB(pair[1], pair[0]); err != nil {
				return err
			}
			if err := a.rotation(pair[1], ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := a.phase("02 replica failover", func() error {
		old := a.lease(d2)
		before, err := a.counters(d2.region)
		if err != nil {
			return err
		}
		if err := a.failover(d2); err != nil {
			return err
		}
		if err := a.rotation(d2, old); err != nil {
			return err
		}
		return a.counterDelta(d2.region, before, 1, 1, 1)
	}); err != nil {
		return err
	}
	if err := a.phase("03 primary Pod replacement", func() error {
		old := a.lease(d2)
		before, err := a.counters(d2.region)
		if err != nil {
			return err
		}
		p, err := a.primary(d2)
		if err != nil {
			return err
		}
		if _, err = a.k(d2.region, nil, "-n", d2.ns, "delete", "pod", p, "--wait=false"); err != nil {
			return err
		}
		if err := a.rotation(d2, old); err != nil {
			return err
		}
		return a.counterDelta(d2.region, before, 1, 1, 1)
	}); err != nil {
		return err
	}
	if err := a.phase("04 worker drain", func() error {
		old := a.lease(d2)
		before, err := a.counters(d2.region)
		if err != nil {
			return err
		}
		p, err := a.primary(d2)
		if err != nil {
			return err
		}
		pod, err := a.get(d2.region, d2.ns, "pod", p)
		if err != nil {
			return err
		}
		node := str(pod, "spec", "nodeName")
		defer func() { _, _ = a.k(d2.region, nil, "uncordon", node) }()
		if _, err = a.k(d2.region, nil, "drain", node, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=5m"); err != nil {
			return err
		}
		if err := a.wait("drained primary moved to another worker", 3*time.Minute, func() (bool, error) {
			current, err := a.primary(d2)
			if err != nil {
				return false, err
			}
			pod, err := a.get(d2.region, d2.ns, "pod", current)
			return current != p && primaryReady(pod) && str(pod, "spec", "nodeName") != node, err
		}); err != nil {
			return err
		}
		if err := a.rotationPrimary(d2, old); err != nil {
			return err
		}
		if err := a.counterDelta(d2.region, before, 1, 1, 1); err != nil {
			return err
		}
		if _, err := a.k(d2.region, nil, "uncordon", node); err != nil {
			return err
		}
		return a.ready(d2)
	}); err != nil {
		return err
	}
	if err := a.phase("05 distributed switchover", func() error { return a.switchover(d1, d2) }); err != nil {
		return err
	}
	if err := a.phase("06 standalone promotion", func() error {
		old := a.lease(d4)
		secret, err := a.get(d4.region, d4.ns, "secret", "credential-"+d4.name)
		if err != nil {
			return err
		}
		cluster, err := a.get(d4.region, d4.ns, "cluster", d4.name)
		if err != nil {
			return err
		}
		if err := a.patch(d4.region, d4.ns, "cluster", d4.name, map[string]any{"spec": map[string]any{"replica": nil}}); err != nil {
			return err
		}
		if err := a.ready(d4); err != nil {
			return err
		}
		if err := a.wait("promotion lease cleanup", 3*time.Minute, func() (bool, error) { s, err := a.entry(d4); return s.ClusterUID == "", err }); err != nil {
			return err
		}
		afterSecret, err := a.get(d4.region, d4.ns, "secret", "credential-"+d4.name)
		if err != nil {
			return err
		}
		afterCluster, err := a.get(d4.region, d4.ns, "cluster", d4.name)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(secret["data"], afterSecret["data"]) || !reflect.DeepEqual(nested(cluster, "spec", "externalClusters"), nested(afterCluster, "spec", "externalClusters")) {
			return fmt.Errorf("standalone promotion mutated credential targets")
		}
		if _, err := a.vault("PUT", "sys/leases/lookup", map[string]string{"lease_id": old}); err == nil {
			return fmt.Errorf("promoted replica lease remains")
		}
		return a.absentLeaseMetric(d4)
	}); err != nil {
		return err
	}
	d5, d6 := a.allocate("us", "e2e-first"), a.allocate("eu", "e2e-first")
	if err := a.phase("07 rebuild replicas", func() error {
		if err := a.onboard(d4); err != nil {
			return err
		}
		for _, pair := range [][2]*database{{d3, d5}, {d4, d6}} {
			if err := a.createDB(pair[1], pair[0]); err != nil {
				return err
			}
			if err := a.rotation(pair[1], ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	d7, d8 := a.allocate("us", "e2e-second"), a.allocate("eu", "e2e-second")
	if err := a.phase("08 second namespace", func() error {
		if err := a.watch("e2e-bootstrap,e2e-first,e2e-second", true); err != nil {
			return err
		}
		if err := a.createDB(d7, nil); err != nil {
			return err
		}
		if err := a.onboard(d7); err != nil {
			return err
		}
		if err := a.createDB(d8, d7); err != nil {
			return err
		}
		return a.rotation(d8, "")
	}); err != nil {
		return err
	}
	if err := a.phase("09 unwatched namespace failover", func() error {
		old := a.lease(d5)
		before := map[string]state.Store{}
		for _, region := range []string{"us", "eu"} {
			journal, err := a.journal(region)
			if err != nil {
				return err
			}
			before[region] = journal
		}
		if err := a.watch("e2e-bootstrap,e2e-second", false); err != nil {
			return err
		}
		if err := a.failover(d5); err != nil {
			return err
		}
		if err := a.ready(d5); err != nil {
			return err
		}
		if a.lease(d5) != old {
			return fmt.Errorf("unwatched namespace rotated")
		}
		if _, err := a.vault("PUT", "sys/leases/lookup", map[string]string{"lease_id": old}); err != nil {
			return err
		}
		for region, journal := range before {
			after, err := a.journal(region)
			if err != nil {
				return err
			}
			for key, entry := range journal.Clusters {
				if !strings.HasPrefix(key, "e2e-first/") {
					continue
				}
				if !reflect.DeepEqual(entry, after.Clusters[key]) {
					return fmt.Errorf("unwatched state changed in %s", region)
				}
				if entry.CurrentLeaseID != "" {
					if _, err := a.vault("PUT", "sys/leases/lookup", map[string]string{"lease_id": entry.CurrentLeaseID}); err != nil {
						return err
					}
				}
			}
		}
		cluster, err := a.get(d5.region, d5.ns, "cluster", d5.name)
		if err != nil {
			return err
		}
		if err := a.streaming(d5, selectedUsername(cluster, d5.source.name)); err != nil {
			return err
		}
		return a.absentLeaseMetric(d5)
	}); err != nil {
		return err
	}
	if err := a.phase("10 restore namespace", func() error {
		old := a.lease(d5)
		if err := a.watch("e2e-bootstrap,e2e-first,e2e-second", false); err != nil {
			return err
		}
		if err := a.rotation(d5, old); err != nil {
			return err
		}
		old = a.lease(d5)
		if err := a.failover(d5); err != nil {
			return err
		}
		return a.rotation(d5, old)
	}); err != nil {
		return err
	}
	if err := a.phase("11 restart and redaction", func() error { return a.safety(d8) }); err != nil {
		return err
	}
	return a.phase("12 database deletion", func() error {
		old := a.lease(d8)
		if _, err := a.k(d8.region, nil, "-n", d8.ns, "delete", "cluster", d8.name, "--wait=false"); err != nil {
			return err
		}
		if err := a.wait("two orphan sweeps", 12*time.Minute, func() (bool, error) { s, err := a.entry(d8); return s.ClusterUID == "", err }); err != nil {
			return err
		}
		if _, err := a.vault("PUT", "sys/leases/lookup", map[string]string{"lease_id": old}); err == nil {
			return fmt.Errorf("deleted replica lease remains")
		}
		if _, err := a.get(d8.region, d8.ns, "secret", "credential-"+d8.name); err != nil {
			return err
		}
		if err := a.absentLeaseMetric(d8); err != nil {
			return err
		}
		if _, err := a.k(d7.region, nil, "-n", d7.ns, "delete", "cluster", d7.name, "--wait=false"); err != nil {
			return err
		}
		_, err := a.vault("DELETE", "database/config/"+d7.name, nil)
		return err
	})
}

func (a *actor) switchover(source, replica *database) error {
	// Prepare the reverse endpoint and dummy Secret before CNPG demotion.
	if err := a.gateway(replica); err != nil {
		return err
	}
	if err := a.apply(source.region, credentialSecret(source)); err != nil {
		return err
	}
	if err := a.patch(source.region, source.ns, "cluster", source.name, map[string]any{"spec": map[string]any{"externalClusters": []any{selfExternal(source), a.external(source, replica)}, "replica": map[string]string{"primary": replica.name, "source": replica.name}}}); err != nil {
		return err
	}
	var token string
	if err := a.wait("demotion token", 5*time.Minute, func() (bool, error) {
		c, err := a.get(source.region, source.ns, "cluster", source.name)
		token = str(c, "status", "demotionToken")
		return token != "", err
	}); err != nil {
		return err
	}
	sensitive = append(sensitive, token)
	if err := a.patch(replica.region, replica.ns, "cluster", replica.name, map[string]any{"spec": map[string]any{"replica": map[string]string{"primary": replica.name, "promotionToken": token}}}); err != nil {
		return err
	}
	if err := a.wait("promoted writable database", 5*time.Minute, func() (bool, error) {
		v, err := a.sql(replica, "SELECT NOT pg_is_in_recovery();")
		return v == "t", err
	}); err != nil {
		return err
	}
	// The SQL management role followed physical replication. Keep its password
	// consistent while redirecting the old Vault configuration to the new writer.
	replica.management = source.management
	if err := a.configureVault(source.name, replica); err != nil {
		return err
	}
	if err := a.onboard(replica); err != nil {
		return err
	}
	if err := a.configureVault(source.name, replica); err != nil {
		return err
	}
	source.source = replica
	replica.source = nil
	return a.rotation(source, "")
}

func (a *actor) safety(d *database) error {
	old := a.lease(d)
	if _, err := a.k(d.region, nil, "-n", "cnpg-system", "rollout", "restart", "deployment/vault-replica-controller"); err != nil {
		return err
	}
	if _, err := a.k(d.region, nil, "-n", "cnpg-system", "rollout", "status", "deployment/vault-replica-controller", "--timeout=3m"); err != nil {
		return err
	}
	if a.lease(d) != old {
		return fmt.Errorf("restart duplicated a committed rotation")
	}
	// Exposition must decrease without an event and must expose only the
	// identifying namespace/Cluster labels from the monitoring contract.
	expr := fmt.Sprintf(`vault_replica_current_lease_time_to_expiration_seconds{namespace="%s",cluster="%s"}`, d.ns, d.name)
	initial, err := a.restartLeaseBaseline(d.region, expr)
	if err != nil {
		return err
	}
	if err := a.wait("scrape-time expiration", time.Minute, func() (bool, error) { v, err := a.query(d.region, expr); return err == nil && v < initial-5, err }); err != nil {
		return err
	}
	if err := a.invalidPassword(d); err != nil {
		return err
	}
	for _, region := range []string{"us", "eu"} {
		if v, err := a.query(region, `up{job="vault-replica"}`); err != nil || v != 1 {
			return fmt.Errorf("Prometheus controller target unhealthy in %s", region)
		}
		if v, err := a.query(region, "sum(vault_replica_pending_workflows)"); err != nil || v != 0 {
			return fmt.Errorf("pending workflows did not settle in %s", region)
		}
		metrics, err := a.k(region, nil, "-n", "cnpg-system", "get", "--raw", "/api/v1/namespaces/cnpg-system/services/vault-replica-metrics:8080/proxy/metrics")
		if err != nil {
			return err
		}
		for _, value := range sensitive {
			if value != "" && strings.Contains(string(metrics), value) {
				return fmt.Errorf("metrics expose sensitive value")
			}
		}
		if !strings.Contains(string(metrics), "controller_runtime_reconcile_total") || !strings.Contains(string(metrics), "workqueue_") {
			return fmt.Errorf("standard controller metrics missing")
		}
		a.artifactWrite("metrics-"+region+".txt", metrics)
		out, err := a.k(region, nil, "-n", "cnpg-system", "logs", "deployment/vault-replica-controller")
		if err != nil {
			return err
		}
		for _, value := range sensitive {
			if value != "" && strings.Contains(string(out), value) {
				return fmt.Errorf("controller log contains sensitive value")
			}
		}
		a.artifactWrite("checked-controller-"+region+".log", out)
		journal, err := a.journal(region)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(journal)
		for _, value := range sensitive {
			if value != "" && strings.Contains(string(raw), value) {
				return fmt.Errorf("journal contains credential material")
			}
		}
		a.artifactWrite("checked-journal-"+region+".json", raw)
	}
	return nil
}

type inactiveWindow struct{ since time.Time }

func receiverInactive(raw []byte) bool {
	var status struct {
		Active *bool `json:"isWalReceiverActive"`
	}
	return json.Unmarshal(raw, &status) == nil && status.Active != nil && !*status.Active
}

func (w *inactiveWindow) observe(inactive bool, now time.Time) bool {
	if !inactive {
		w.since = time.Time{}
		return false
	}
	if w.since.IsZero() {
		w.since = now
	}
	return now.Sub(w.since) >= config.DefaultPasswordPropagationDelay
}

func (a *actor) invalidPassword(d *database) error {
	old := a.lease(d)
	if err := a.failover(d); err != nil {
		return err
	}
	var deadline time.Time
	if err := a.wait("pending reconnect boundary", 3*time.Minute, func() (bool, error) {
		s, err := a.entry(d)
		if s.Pending != nil && s.Pending.Stage == "reconnect-pending" {
			deadline = s.Pending.StageDeadline
			return true, err
		}
		return false, err
	}); err != nil {
		return err
	}
	// Restart at a persisted boundary; the new process must honor its delay.
	if err := a.wait("pending workflow shipped to Prometheus", time.Minute, func() (bool, error) {
		v, err := a.query(d.region, `sum(vault_replica_pending_workflows{phase="reconnect-pending"})`)
		return v >= 1, err
	}); err != nil {
		return err
	}
	if _, err := a.k(d.region, nil, "-n", "cnpg-system", "rollout", "restart", "deployment/vault-replica-controller"); err != nil {
		return err
	}
	if err := a.patch(d.region, d.ns, "secret", "credential-"+d.name, map[string]any{"stringData": map[string]string{"password": "invalid-test-password"}}); err != nil {
		return err
	}
	sensitive = append(sensitive, "invalid-test-password")
	window := &inactiveWindow{}
	if err := a.wait("invalid password receiver inactive", time.Minute, func() (bool, error) {
		v, err := a.sql(d, "SELECT NOT EXISTS (SELECT FROM pg_stat_wal_receiver WHERE status='streaming');")
		if err != nil {
			return false, err
		}
		p, err := a.primary(d)
		if err != nil {
			return false, err
		}
		raw, err := a.k(d.region, nil, "get", "--raw", "/api/v1/namespaces/"+d.ns+"/pods/https:"+p+":8000/proxy/pg/status")
		if err != nil {
			window.observe(false, time.Now())
			return false, err
		}
		inactive := v == "t" && receiverInactive(raw)
		if window.observe(inactive, time.Now()) {
			return true, nil
		}
		if !inactive {
			// A disconnect before CNPG loads the Secret can reconnect with the
			// old passfile. Retry only this replica's sender, then require a
			// sustained inactive receiver without further disconnects.
			_, err := a.sql(d.source, "SELECT pg_terminate_backend(pid) FROM pg_stat_replication WHERE application_name='"+d.name+"';")
			return false, err
		}
		return false, nil
	}); err != nil {
		return err
	}
	// The normal reconnect deadline is five minutes; do not shorten it. Until
	// that deadline, neither a Ready Pod nor controller restart may commit.
	if err := a.wait("retain leases before verification deadline", time.Until(deadline)+time.Minute, func() (bool, error) {
		s, err := a.entry(d)
		if err != nil {
			return false, err
		}
		if s.CurrentLeaseID != old {
			return false, fmt.Errorf("failed pending credential committed")
		}
		return time.Now().After(deadline), nil
	}); err != nil {
		return err
	}
	if err := a.wait("verification failure metrics", time.Minute, func() (bool, error) {
		failures, err := a.query(d.region, `sum(vault_replica_rotations_total{result="failure"}) or vector(0)`)
		return failures >= 1, err
	}); err != nil {
		return err
	}
	return a.rotation(d, old)
}
