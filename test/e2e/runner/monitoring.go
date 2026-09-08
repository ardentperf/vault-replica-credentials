package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func probeOverrides() string {
	return `{"spec":{"nodeSelector":{"node-role.kubernetes.io/control-plane":""},"tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]}}`
}

func (a *actor) infrastructure() error {
	for _, region := range []string{"us", "eu"} {
		service := obj("Service", "cnpg-system", "vault-replica-metrics", map[string]any{"selector": map[string]string{"app.kubernetes.io/name": "vault-replica-controller"}, "ports": []any{map[string]any{"port": 8080, "targetPort": "metrics"}}})
		if err := a.apply(region, service); err != nil {
			return err
		}
		cfg := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "e2e-prometheus", "namespace": "cnpg-system"}, "data": map[string]string{"prometheus.yml": "global:\n  scrape_interval: 5s\nscrape_configs:\n  - job_name: vault-replica\n    static_configs:\n      - targets: ['vault-replica-metrics:8080']\n"}}
		if err := a.apply(region, cfg); err != nil {
			return err
		}
		raw, _ := json.Marshal(cfg)
		a.artifactWrite("prometheus-"+region+".json", raw)
		labels := map[string]string{"app": "e2e-prometheus"}
		promService := obj("Service", "cnpg-system", "e2e-prometheus", map[string]any{"selector": labels, "ports": []any{map[string]any{"port": 9090, "targetPort": 9090}}})
		if err := a.apply(region, promService); err != nil {
			return err
		}
		dep := obj("Deployment", "cnpg-system", "e2e-prometheus", map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{"nodeSelector": map[string]string{"node-role.kubernetes.io/control-plane": ""}, "tolerations": []any{map[string]string{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists"}}, "containers": []any{map[string]any{"name": "prometheus", "image": os.Getenv("PROMETHEUS_IMAGE"), "args": []string{"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.retention.time=2h"}, "volumeMounts": []any{map[string]string{"name": "config", "mountPath": "/etc/prometheus"}}, "readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/-/ready", "port": 9090}}}}, "volumes": []any{map[string]any{"name": "config", "configMap": map[string]string{"name": "e2e-prometheus"}}}}}})
		if err := a.apply(region, dep); err != nil {
			return err
		}
		if _, err := a.k(region, nil, "-n", "cnpg-system", "rollout", "status", "deployment/e2e-prometheus", "--timeout=5m"); err != nil {
			return err
		}
		cmd := exec.CommandContext(a.ctx, "kubectl", "--context", "kind-k8s-"+region, "-n", "cnpg-system", "port-forward", "service/e2e-prometheus", fmt.Sprintf("%d:9090", a.ports[region]))
		if err := cmd.Start(); err != nil {
			return err
		}
		a.forwards = append(a.forwards, cmd)
		if err := a.wait("Prometheus target "+region, 2*time.Minute, func() (bool, error) {
			v, err := a.prom(region, "/api/v1/targets")
			if err != nil {
				return false, err
			}
			targets, _ := nested(v, "data", "activeTargets").([]any)
			if len(targets) != 1 {
				return false, nil
			}
			target := targets[0].(map[string]any)
			raw, _ := json.Marshal(v)
			a.artifactWrite("targets-"+region+".json", raw)
			return target["health"] == "up", nil
		}); err != nil {
			return err
		}
		if err := a.rbac(region, "e2e-bootstrap"); err != nil {
			return err
		}
		// Cross-Kind host-network reachability is checked from actual Pods.
		if _, err := a.k(region, nil, "-n", "e2e-bootstrap", "run", "vault-reachability", "--image="+os.Getenv("VAULT_IMAGE"), "--overrides="+probeOverrides(), "--restart=Never", "--attach=true", "--rm=true", "--command", "--", "wget", "-qO-", a.address+"/v1/sys/health"); err != nil {
			return err
		}
	}
	return nil
}
func (a *actor) prom(region, path string) (map[string]any, error) {
	resp, err := a.http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", a.ports[region], path))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var v map[string]any
	err = json.Unmarshal(raw, &v)
	return v, err
}
func (a *actor) query(region, expr string) (float64, error) {
	v, err := a.prom(region, "/api/v1/query?query="+url.QueryEscape(expr))
	if err != nil {
		return 0, err
	}
	raw, _ := json.Marshal(map[string]any{"query": expr, "response": v})
	a.artifactWrite(fmt.Sprintf("query-%s-%x.json", region, sha256.Sum256([]byte(expr))), raw)
	results, _ := nested(v, "data", "result").([]any)
	if len(results) != 1 {
		return 0, fmt.Errorf("Prometheus expected one result for %s", expr)
	}
	value := results[0].(map[string]any)["value"].([]any)
	return strconv.ParseFloat(value[1].(string), 64)
}

func (a *actor) absentLeaseMetric(d *database) error {
	expr := fmt.Sprintf(`count(vault_replica_current_lease_time_to_expiration_seconds{namespace="%s",cluster="%s"}) or vector(0)`, d.ns, d.name)
	return a.wait("removed lease metric "+d.name, time.Minute, func() (bool, error) {
		v, err := a.query(d.region, expr)
		return v == 0, err
	})
}

func (a *actor) restartLeaseBaseline(region, expr string) (float64, error) {
	var value float64
	err := a.wait("leader-restored lease metric", time.Minute, func() (bool, error) {
		var err error
		value, err = a.query(region, expr)
		return err == nil && value > 0, err
	})
	return value, err
}
func (a *actor) rbac(region, ns string) error {
	for _, verb := range []string{"patch", "get", "list", "watch", "update", "create", "delete"} {
		out, err := a.k(region, nil, "auth", "can-i", verb, "secrets/credential", "--as=system:serviceaccount:cnpg-system:vault-replica-controller", "-n", ns)
		if verb == "patch" {
			if err != nil || string(out) != "yes\n" {
				return fmt.Errorf("Secret patch RBAC missing in %s/%s", region, ns)
			}
		} else if err == nil {
			return fmt.Errorf("forbidden Secret %s allowed in %s/%s", verb, region, ns)
		}
	}
	for _, resource := range []string{"nodes", "events"} {
		if _, err := a.k(region, nil, "auth", "can-i", "watch", resource, "--as=system:serviceaccount:cnpg-system:vault-replica-controller", "-n", ns); err == nil {
			return fmt.Errorf("forbidden %s watch allowed", resource)
		}
	}
	return nil
}

func (a *actor) counters(region string) ([3]float64, error) {
	var values [3]float64
	for i, expr := range []string{`sum(vault_replica_rotations_total{result="success"}) or vector(0)`, `sum(vault_replica_vault_operations_total{operation="issue",result="success"}) or vector(0)`, `sum(vault_replica_vault_operations_total{operation="revoke",result="success"}) or vector(0)`} {
		v, err := a.query(region, expr)
		if err != nil {
			return values, err
		}
		values[i] = v
	}
	return values, nil
}
func (a *actor) counterDelta(region string, before [3]float64, rotation, issue, revoke float64) error {
	want := [3]float64{rotation, issue, revoke}
	return a.wait("Prometheus exact operation deltas", time.Minute, func() (bool, error) {
		after, err := a.counters(region)
		if err != nil {
			return false, err
		}
		for i := range after {
			if after[i]-before[i] != want[i] {
				return false, fmt.Errorf("metric %d delta %g, expected %g", i, after[i]-before[i], want[i])
			}
		}
		pending, err := a.query(region, "sum(vault_replica_pending_workflows)")
		return pending == 0, err
	})
}
