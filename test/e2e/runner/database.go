package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ardentperf/vault-replica-credentials/internal/state"
)

type database struct {
	name, region, ns, host, port, management string
	source                                   *database
}

func (a *actor) allocate(region, ns string) *database {
	a.counter++
	return &database{name: fmt.Sprintf("db%02d", a.counter), region: region, ns: ns, port: strconv.Itoa(15432 + a.counter), management: random()}
}
func cnpgConfig(namespaces string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"namespace": "cnpg-system", "name": "cnpg-controller-manager-config"}, "data": map[string]any{"WATCH_NAMESPACE": namespaces}}
}
func (a *actor) watch(namespaces string, cnpg bool) error {
	for _, region := range []string{"us", "eu"} {
		for _, ns := range strings.Split(namespaces, ",") {
			if _, err := a.k(region, nil, "get", "namespace", ns); err != nil {
				if _, err = a.k(region, nil, "create", "namespace", ns); err != nil {
					return err
				}
			}
			for _, file := range []string{"target-role.yaml", "target-rolebinding.yaml"} {
				raw, err := os.ReadFile("config/rbac/" + file)
				if err != nil {
					return err
				}
				raw = []byte(strings.ReplaceAll(string(raw), "namespace: reporting", "namespace: "+ns))
				if _, err = a.k(region, raw, "apply", "-f", "-"); err != nil {
					return err
				}
			}
			if err := a.rbac(region, ns); err != nil {
				return err
			}
		}
		if cnpg {
			raw, err := a.command(nil, "kubectl", "cnpg", "install", "generate", "--version", strings.TrimPrefix(os.Getenv("CNPG_VERSION"), "v"), "--watch-namespace", namespaces, "--control-plane")
			if err != nil {
				return err
			}
			if _, err = a.k(region, raw, "apply", "--server-side", "-f", "-"); err != nil {
				return err
			}
			if err := a.apply(region, cnpgConfig(namespaces)); err != nil {
				return err
			}
			if _, err = a.k(region, nil, "-n", "cnpg-system", "rollout", "restart", "deployment/cnpg-controller-manager"); err != nil {
				return err
			}
			if _, err = a.k(region, nil, "-n", "cnpg-system", "rollout", "status", "deployment/cnpg-controller-manager", "--timeout=5m"); err != nil {
				return err
			}
		}
		if _, err := a.k(region, nil, "-n", "cnpg-system", "set", "env", "deployment/vault-replica-controller", "WATCH_NAMESPACE="+namespaces); err != nil {
			return err
		}
		if _, err := a.k(region, nil, "-n", "cnpg-system", "rollout", "status", "deployment/vault-replica-controller", "--timeout=5m"); err != nil {
			return err
		}
		config, err := a.get(region, "cnpg-system", "configmap", "cnpg-controller-manager-config")
		if err != nil {
			return err
		}
		cnpgNamespaces := str(config, "data", "WATCH_NAMESPACE")
		if cnpg && cnpgNamespaces != namespaces || !cnpg && !strings.Contains(cnpgNamespaces, "e2e-first") {
			return fmt.Errorf("CNPG watch configuration is incorrect in %s", region)
		}
	}
	a.artifactWrite(fmt.Sprintf("watch-%d.txt", time.Now().UnixNano()), []byte(fmt.Sprintf("namespaces=%s cnpg=%t", namespaces, cnpg)))
	return nil
}

func (a *actor) clusterManifest(d *database, source *database) map[string]any {
	peer := "future-peer"
	if source != nil {
		peer = source.name
	}
	spec := map[string]any{"instances": 2, "imageName": os.Getenv("POSTGRES_IMAGE"), "storage": map[string]any{"size": "1Gi"}, "resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "128Mi"}, "limits": map[string]string{"memory": "384Mi"}}, "affinity": map[string]any{"nodeSelector": map[string]string{"postgres.node.kubernetes.io": ""}, "tolerations": []any{map[string]string{"key": "node-role.kubernetes.io/postgres", "operator": "Exists", "effect": "NoSchedule"}}}, "postgresql": map[string]any{"parameters": map[string]string{"shared_buffers": "32MB", "max_connections": "30", "wal_keep_size": "512MB"}, "pg_hba": []string{"host all all 0.0.0.0/0 scram-sha-256", "host replication all 0.0.0.0/0 scram-sha-256"}}, "replica": map[string]any{"primary": d.name, "source": peer, "self": d.name}, "externalClusters": []any{map[string]any{"name": peer, "connectionParameters": map[string]string{"host": "unused", "user": "dummy-user"}, "password": map[string]string{"name": "credential-" + d.name, "key": "password"}}}}
	if source != nil {
		spec["replica"] = map[string]any{"primary": source.name, "source": source.name, "self": d.name}
		spec["bootstrap"] = map[string]any{"pg_basebackup": map[string]string{"source": source.name}}
		spec["externalClusters"] = []any{a.external(d, source)}
	}
	cluster := obj("Cluster", d.ns, d.name, spec)
	spec["externalClusters"] = append([]any{selfExternal(d)}, spec["externalClusters"].([]any)...)
	cluster["apiVersion"] = "postgresql.cnpg.io/v1"
	return cluster
}
func (a *actor) createDB(d *database, source *database) error {
	d.source = source
	if err := a.apply(d.region, a.clusterManifest(d, source)); err != nil {
		return err
	}
	if source != nil {
		if err := a.apply(d.region, credentialSecret(d)); err != nil {
			return err
		}
	}
	return nil
}
func credentialSecret(d *database) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"namespace": d.ns, "name": "credential-" + d.name, "labels": map[string]string{"cnpg.io/reload": "true"}}, "stringData": map[string]string{"password": "dummy-password"}}
}
func (a *actor) external(d, source *database) map[string]any {
	return map[string]any{"name": source.name, "connectionParameters": map[string]string{"host": source.host, "port": source.port, "dbname": "postgres", "sslmode": "disable", "user": "dummy-user", "application_name": d.name}, "password": map[string]string{"name": "credential-" + d.name, "key": "password"}}
}
func selfExternal(d *database) map[string]any {
	return map[string]any{"name": d.name, "connectionParameters": map[string]string{"host": d.name + "-rw." + d.ns + ".svc", "dbname": "postgres"}}
}
func (a *actor) primary(d *database) (string, error) {
	c, err := a.get(d.region, d.ns, "cluster", d.name)
	if err != nil {
		return "", err
	}
	p := str(c, "status", "currentPrimary")
	if p == "" {
		return "", fmt.Errorf("primary unavailable for %s", d.name)
	}
	return p, nil
}
func (a *actor) sql(d *database, sql string) (string, error) {
	p, err := a.primary(d)
	if err != nil {
		return "", err
	}
	out, err := a.k(d.region, []byte(sql), "-n", d.ns, "exec", "-i", p, "-c", "postgres", "--", "psql", "-X", "-qAt", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres")
	return strings.TrimSpace(string(out)), err
}
func (a *actor) ready(d *database) error {
	_, err := a.k(d.region, nil, "-n", d.ns, "wait", "cluster/"+d.name, "--for=condition=Ready", "--timeout=8m")
	return err
}
func gatewayHealthPort(d *database) string {
	port, _ := strconv.Atoi(d.port)
	return strconv.Itoa(port + 2000)
}
func (a *actor) gateway(d *database) error {
	out, err := a.command(nil, "docker", "inspect", "-f", `{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}`, "k8s-"+d.region+"-control-plane")
	if err != nil {
		return err
	}
	d.host = strings.TrimSpace(string(out))
	raw, err := os.ReadFile("test/e2e/manifests/gateway.yaml")
	if err != nil {
		return err
	}
	replacer := strings.NewReplacer("__GATEWAY_NAME__", "gateway-"+d.name, "__NAMESPACE__", d.ns, "__GATEWAY_IMAGE__", os.Getenv("GATEWAY_IMAGE"), "__GATEWAY_PORT__", d.port, "__HEALTH_PORT__", gatewayHealthPort(d), "__BACKEND_SERVICE__", d.name+"-rw."+d.ns+".svc")
	manifest := []byte(replacer.Replace(string(raw)))
	a.artifactWrite("gateway-"+d.name+".yaml", manifest)
	if _, err := a.k(d.region, manifest, "apply", "-f", "-"); err != nil {
		return err
	}
	_, err = a.k(d.region, nil, "-n", d.ns, "rollout", "status", "deployment/gateway-"+d.name, "--timeout=3m")
	return err
}
func (a *actor) onboard(d *database) error {
	if err := a.ready(d); err != nil {
		return err
	}
	if err := a.gateway(d); err != nil {
		return err
	}
	sensitive = append(sensitive, d.management)
	// PostgreSQL requires superuser authority to assign REPLICATION. Narrow
	// SECURITY DEFINER routines grant this operation without making Vault's
	// SQL-provisioned CREATEROLE account a superuser.
	sql := fmt.Sprintf(`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='vault_replica_admin') THEN CREATE ROLE vault_replica_admin LOGIN CREATEROLE; END IF; END $$;
ALTER ROLE vault_replica_admin PASSWORD '%s';
CREATE OR REPLACE FUNCTION public.vault_create_role(n text,p text,e text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN IF n !~ '^v-(token|root)-' THEN RAISE EXCEPTION 'invalid dynamic name'; END IF; EXECUTE format('CREATE ROLE %%I LOGIN REPLICATION PASSWORD %%L VALID UNTIL %%L',n,p,e); END $$;
CREATE OR REPLACE FUNCTION public.vault_drop_role(n text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$ BEGIN IF n !~ '^v-(token|root)-' THEN RAISE EXCEPTION 'invalid dynamic name'; END IF; PERFORM pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename=n; EXECUTE format('DROP ROLE IF EXISTS %%I',n); END $$;
REVOKE ALL ON FUNCTION public.vault_create_role(text,text,text),public.vault_drop_role(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.vault_create_role(text,text,text),public.vault_drop_role(text) TO vault_replica_admin;
CREATE TABLE IF NOT EXISTS public.e2e_markers (id text PRIMARY KEY);
`, d.management)
	if _, err := a.sql(d, sql); err != nil {
		return err
	}
	if err := a.configureVault(d.name, d); err != nil {
		return err
	}
	_, err := a.vault("POST", "database/roles/"+d.name, map[string]any{"db_name": d.name, "default_ttl": "768h", "max_ttl": "768h", "creation_statements": []string{`SELECT public.vault_create_role('{{name}}','{{password}}','{{expiration}}');`}, "revocation_statements": []string{`SELECT public.vault_drop_role('{{name}}');`}})
	if err != nil {
		return err
	}
	probe, err := a.vault("GET", "database/creds/"+d.name, nil)
	if err != nil {
		return err
	}
	sensitive = append(sensitive, str(probe, "data", "username"), str(probe, "data", "password"))
	_, err = a.vault("PUT", "sys/leases/revoke", map[string]string{"lease_id": str(probe, "lease_id")})
	return err
}
func (a *actor) configureVault(name string, d *database) error {
	// Configuration is idempotent (unlike credential issuance). CNPG can
	// restart immediately after promotion to apply archive_mode, so retry the
	// gateway/connection-verification race within a bounded onboarding phase.
	return a.wait("Vault source configuration "+name, 3*time.Minute, func() (bool, error) {
		_, err := a.vault("POST", "database/config/"+name, map[string]any{"plugin_name": "postgresql-database-plugin", "allowed_roles": []string{name}, "connection_url": fmt.Sprintf("postgresql://{{username}}:{{password}}@%s:%s/postgres?sslmode=disable", d.host, d.port), "username": "vault_replica_admin", "password": d.management})
		return err == nil, err
	})
}
func (a *actor) journal(region string) (state.Store, error) {
	secret, err := a.get(region, "cnpg-system", "secret", "vault-replica-controller-state")
	if err != nil {
		return state.Store{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(str(secret, "data", "state.json"))
	if err != nil {
		return state.Store{}, err
	}
	return state.Decode(raw, 262144)
}
func (a *actor) entry(d *database) (state.ClusterState, error) {
	s, err := a.journal(d.region)
	return s.Clusters[d.ns+"/"+d.name], err
}

func primaryReady(pod map[string]any) bool {
	if nested(pod, "metadata", "deletionTimestamp") != nil {
		return false
	}
	conditions, _ := nested(pod, "status", "conditions").([]any)
	for _, condition := range conditions {
		c, _ := condition.(map[string]any)
		if c["type"] == "Ready" {
			return c["status"] == "True"
		}
	}
	return false
}

func selectedUsername(cluster map[string]any, source string) string {
	entries, _ := nested(cluster, "spec", "externalClusters").([]any)
	for _, entry := range entries {
		m, _ := entry.(map[string]any)
		if m["name"] == source {
			return str(m, "connectionParameters", "user")
		}
	}
	return ""
}

func (a *actor) rotation(d *database, old string) error {
	if err := a.rotationPrimary(d, old); err != nil {
		return err
	}
	return a.ready(d)
}

func (a *actor) rotationPrimary(d *database, old string) error {
	var entry state.ClusterState
	if err := a.wait("rotation "+d.name, 12*time.Minute, func() (bool, error) {
		var err error
		entry, err = a.entry(d)
		return err == nil && entry.CurrentLeaseID != "" && entry.CurrentLeaseID != old && entry.Pending == nil, err
	}); err != nil {
		return err
	}
	// A drained standby's local PVC can remain pinned to the cordoned node.
	// Verify the designated primary and SQL stream before uncordoning; the
	// drain scenario checks full Cluster readiness after the node returns.
	if err := a.wait("replication primary Ready "+d.name, 3*time.Minute, func() (bool, error) {
		p, err := a.primary(d)
		if err != nil {
			return false, err
		}
		pod, err := a.get(d.region, d.ns, "pod", p)
		return primaryReady(pod), err
	}); err != nil {
		return err
	}
	cluster, err := a.get(d.region, d.ns, "cluster", d.name)
	if err != nil {
		return err
	}
	username := selectedUsername(cluster, d.source.name)
	if username == "" || username == "dummy-user" {
		return fmt.Errorf("dynamic username missing for %s", d.name)
	}
	sensitive = append(sensitive, username)
	secret, err := a.get(d.region, d.ns, "secret", "credential-"+d.name)
	if err != nil {
		return err
	}
	pw, err := base64.StdEncoding.DecodeString(str(secret, "data", "password"))
	if err != nil || string(pw) == "dummy-password" {
		return fmt.Errorf("dynamic password missing for %s", d.name)
	}
	sensitive = append(sensitive, string(pw), base64.StdEncoding.EncodeToString(pw))
	if err := a.streaming(d, username); err != nil {
		return err
	}
	if old != "" {
		if _, err := a.vault("PUT", "sys/leases/lookup", map[string]string{"lease_id": old}); err == nil {
			return fmt.Errorf("old lease still exists for %s", d.name)
		}
	}
	if _, err := a.vault("PUT", "sys/leases/lookup", map[string]string{"lease_id": entry.CurrentLeaseID}); err != nil {
		return err
	}
	metric := fmt.Sprintf(`vault_replica_current_lease_time_to_expiration_seconds{namespace="%s",cluster="%s"}`, d.ns, d.name)
	if err := a.wait("lease metric "+d.name, time.Minute, func() (bool, error) {
		v, err := a.query(d.region, metric)
		return err == nil && v > 0 && entry.CurrentExpiresAt != nil && abs(v-time.Until(*entry.CurrentExpiresAt).Seconds()) < 20, err
	}); err != nil {
		return err
	}
	a.artifactWrite("rotation-"+d.name+"-"+strconv.FormatInt(time.Now().Unix(), 10)+".json", []byte(fmt.Sprintf(`{"cluster":%q,"namespace":%q,"wal":"streaming","pending":false}`, d.name, d.ns)))
	return nil
}
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
func (a *actor) streaming(d *database, username string) error {
	marker := random()
	if _, err := a.sql(d.source, "INSERT INTO public.e2e_markers VALUES ('"+marker+"');"); err != nil {
		return err
	}
	return a.wait("SQL/WAL marker "+d.name, 3*time.Minute, func() (bool, error) {
		source, err := a.sql(d.source, "SELECT count(*) FROM pg_stat_replication WHERE application_name='"+d.name+"' AND usename='"+username+"' AND state='streaming';")
		if err != nil {
			return false, err
		}
		replica, err := a.sql(d, "SELECT pg_is_in_recovery() AND EXISTS (SELECT FROM pg_stat_wal_receiver WHERE status='streaming') AND EXISTS (SELECT FROM public.e2e_markers WHERE id='"+marker+"');")
		if err != nil {
			return false, err
		}
		return source == "1" && replica == "t", nil
	})
}
func (a *actor) lease(d *database) string {
	s, err := a.entry(d)
	if err != nil {
		return ""
	}
	return s.CurrentLeaseID
}
func (a *actor) failover(d *database) error {
	p, err := a.primary(d)
	if err != nil {
		return err
	}
	pods, err := a.get(d.region, d.ns, "pods", "-l=cnpg.io/cluster="+d.name)
	if err != nil {
		return err
	}
	var candidate string
	for _, item := range pods["items"].([]any) {
		m := item.(map[string]any)
		n := str(m, "metadata", "name")
		if n != p && str(m, "status", "phase") == "Running" {
			candidate = n
		}
	}
	if candidate == "" {
		return fmt.Errorf("standby unavailable for %s", d.name)
	}
	if _, err := a.command(nil, "kubectl", "cnpg", "--context", "kind-k8s-"+d.region, "-n", d.ns, "promote", d.name, candidate); err != nil {
		return err
	}
	return a.wait("primary transition "+d.name, 5*time.Minute, func() (bool, error) { newPrimary, err := a.primary(d); return newPrimary == candidate, err })
}
