#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=setup.sh
source "${ROOT_DIR}/test/e2e/setup.sh"

MANAGEMENT_USER=vault_replica_admin
MANAGEMENT_PASSWORD=VaultAdmin-E2E-Only-42
DUMMY_USER=dummy-user-value
DUMMY_PASSWORD=dummy-password-value
RESULTS_FILE="${ARTIFACT_DIR}/scenario-results.txt"
CONTROLLER_WATCH=e2e-bootstrap
next_gateway_port=15432
next_health_port=18081

declare -A gateway_ports
declare -A gateway_regions

phase() {
	log "$*"
	printf 'PASS %s\n' "$*" >>"${RESULTS_FILE}"
}

lease_fingerprint() {
	printf '%s' "$1" | sha256sum | cut -c1-12
}

wait_for() {
	local description="$1"
	local timeout_seconds="$2"
	shift 2
	local deadline=$((SECONDS + timeout_seconds))
	until "$@"; do
		if ((SECONDS >= deadline)); then
			echo "timed out waiting for ${description}" >&2
			return 1
		fi
		sleep 5
	done
}

cluster_ready() {
	local context="$1" namespace="$2" cluster="$3"
	kubectl --context "${context}" -n "${namespace}" get cluster "${cluster}" -o json 2>/dev/null |
		jq -e '.status.conditions[]? | select(.type == "Ready" and .status == "True")' >/dev/null
}

cluster_pod_count_is() {
	local count
	count=$(kubectl --context "$1" -n "$2" get pods -l "cnpg.io/cluster=$3" \
		-o json 2>/dev/null | jq '.items | length' 2>/dev/null || true)
	[[ "${count}" == "$4" ]]
}

wait_cluster_ready() {
	wait_for "Cluster $3 Ready" 600 cluster_ready "$1" "$2" "$3"
}

state_json() {
	kubectl --context "$1" -n cnpg-system get secret vault-replica-controller-state \
		-o jsonpath='{.data.state\.json}' | base64 -d
}

state_lease() {
	state_json "$1" | jq -r --arg key "$2/$3" '.clusters[$key].currentLeaseID // ""'
}

state_pending_lease() {
	state_json "$1" | jq -r --arg key "$2/$3" '.clusters[$key].pending.leaseID // ""'
}

state_pending_stage() {
	state_json "$1" | jq -r --arg key "$2/$3" '.clusters[$key].pending.stage // ""'
}

state_committed() {
	local context="$1" namespace="$2" cluster="$3" previous="$4"
	local journal
	journal=$(state_json "${context}")
	jq -e --arg key "${namespace}/${cluster}" --arg previous "${previous}" \
		'.clusters[$key].pending == null and (.clusters[$key].currentLeaseID // "") != "" and .clusters[$key].currentLeaseID != $previous' \
		<<<"${journal}" >/dev/null
}

state_absent() {
	state_json "$1" | jq -e --arg key "$2/$3" '.clusters[$key] == null' >/dev/null
}

cluster_user() {
	kubectl --context "$1" -n "$2" get cluster "$3" -o json |
		jq -r --arg source "$4" '.spec.externalClusters[] | select(.name == $source) | .connectionParameters.user'
}

secret_password() {
	kubectl --context "$1" -n "$2" get secret "$3" -o jsonpath='{.data.password}' | base64 -d
}

primary_pod() {
	kubectl --context "$1" -n "$2" get cluster "$3" -o jsonpath='{.status.currentPrimary}'
}

primary_uid() {
	local pod
	pod=$(primary_pod "$1" "$2" "$3")
	kubectl --context "$1" -n "$2" get pod "${pod}" -o jsonpath='{.metadata.uid}'
}

primary_node() {
	local pod
	pod=$(primary_pod "$1" "$2" "$3")
	kubectl --context "$1" -n "$2" get pod "${pod}" -o jsonpath='{.spec.nodeName}'
}

apply_target_rbac_namespace() {
	local context="$1" namespace="$2"
	for manifest in target-role.yaml target-rolebinding.yaml; do
		sed "s/namespace: reporting/namespace: ${namespace}/g" \
			"${ROOT_DIR}/config/rbac/${manifest}" | kubectl --context "${context}" apply -f - >/dev/null
	done
}

update_cnpg_watch() {
	local context="$1" namespaces="$2"
	kubectl cnpg install generate --version "${CNPG_VERSION_BARE}" \
		--watch-namespace "${namespaces}" --control-plane |
		kubectl --context "${context}" apply --server-side -f - >/dev/null
	kubectl --context "${context}" -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=5m >/dev/null
}

update_controller_watch() {
	local context="$1" namespaces="$2"
	kubectl --context "${context}" -n cnpg-system set env deployment/vault-replica-controller \
		WATCH_NAMESPACE="${namespaces}" >/dev/null
	kubectl --context "${context}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
	wait_for "controller leader acquisition in ${context}" 120 controller_is_leader "${context}"
}

controller_is_leader() {
	local context="$1" holder pod
	pod=$(kubectl --context "${context}" -n cnpg-system get pods \
		-l app.kubernetes.io/name=vault-replica-controller \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	holder=$(kubectl --context "${context}" -n cnpg-system get lease vault-replica-controller \
		-o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
	[[ -n "${pod}" && "${holder}" == "${pod}_"* ]]
}

restart_controller() {
	local context="$1"
	kubectl --context "${context}" -n cnpg-system rollout restart deployment/vault-replica-controller >/dev/null
	kubectl --context "${context}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
	wait_for "controller leader acquisition in ${context}" 120 controller_is_leader "${context}"
}

onboard_namespace() {
	local namespace="$1"
	CONTROLLER_WATCH="${CONTROLLER_WATCH},${namespace}"
	for region in us eu; do
		local context
		context=$(context_for "${region}")
		kubectl --context "${context}" create namespace "${namespace}" >/dev/null
		apply_target_rbac_namespace "${context}" "${namespace}"
		update_cnpg_watch "${context}" "${CONTROLLER_WATCH}"
		update_controller_watch "${context}" "${CONTROLLER_WATCH}"
	done
}

create_source() {
	local region="$1" namespace="$2" name="$3" mode="${4:-standalone}" partner="${5:-}" partner_host="${6:-}" partner_port="${7:-}"
	local context
	context=$(context_for "${region}")
	if [[ "${mode}" == distributed ]]; then
		kubectl --context "${context}" -n "${namespace}" create secret generic "${name}-reverse-credentials" \
			--from-literal=password="${DUMMY_PASSWORD}" >/dev/null
		cat <<EOF | kubectl --context "${context}" apply -f - >/dev/null
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:${POSTGRES_VERSION}
  storage: {size: 1Gi}
  affinity:
    nodeSelector: {postgres.node.kubernetes.io: ""}
    tolerations: [{key: node-role.kubernetes.io/postgres, operator: Exists}]
  postgresql:
    pg_hba: ["hostssl replication all all scram-sha-256", "hostssl all all all scram-sha-256"]
  replica: {primary: ${name}, source: ${partner}}
  externalClusters:
    - name: ${name}
      connectionParameters: {host: 127.0.0.1, user: unused, dbname: postgres}
      password: {name: ${name}-reverse-credentials, key: password}
    - name: ${partner}
      connectionParameters: {host: ${partner_host}, port: "${partner_port}", user: ${DUMMY_USER}, dbname: postgres, sslmode: require}
      password: {name: ${name}-reverse-credentials, key: password}
EOF
	else
		cat <<EOF | kubectl --context "${context}" apply -f - >/dev/null
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: ${name}, namespace: ${namespace}}
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:${POSTGRES_VERSION}
  storage: {size: 1Gi}
  affinity:
    nodeSelector: {postgres.node.kubernetes.io: ""}
    tolerations: [{key: node-role.kubernetes.io/postgres, operator: Exists}]
  postgresql:
    pg_hba: ["hostssl replication all all scram-sha-256", "hostssl all all all scram-sha-256"]
EOF
	fi
	wait_cluster_ready "${context}" "${namespace}" "${name}"
	local primary
	primary=$(primary_pod "${context}" "${namespace}" "${name}")
	kubectl --context "${context}" -n "${namespace}" exec "${primary}" -- psql -U postgres -v ON_ERROR_STOP=1 \
		-c "CREATE ROLE ${MANAGEMENT_USER} WITH LOGIN CREATEROLE REPLICATION PASSWORD '${MANAGEMENT_PASSWORD}';" >/dev/null
}

deploy_gateway() {
	local region="$1" namespace="$2" source="$3"
	local context port health
	context=$(context_for "${region}")
	port=${next_gateway_port}
	health=${next_health_port}
	next_gateway_port=$((next_gateway_port + 1))
	next_health_port=$((next_health_port + 1))
	gateway_ports["${source}"]=${port}
	gateway_regions["${source}"]=${region}
	sed -e "s|__GATEWAY_NAME__|gateway-${source}|g" \
		-e "s|__NAMESPACE__|${namespace}|g" \
		-e "s|__GATEWAY_IMAGE__|${GATEWAY_IMAGE}|g" \
		-e "s|__GATEWAY_PORT__|${port}|g" \
		-e "s|__HEALTH_PORT__|${health}|g" \
		-e "s|__BACKEND_SERVICE__|${source}-rw.${namespace}.svc|g" \
		"${E2E_DIR}/manifests/gateway.yaml" | kubectl --context "${context}" apply -f - >/dev/null
	kubectl --context "${context}" -n "${namespace}" rollout status "deployment/gateway-${source}" --timeout=5m >/dev/null
}

gateway_host() {
	local region=${gateway_regions[$1]}
	docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$(cluster_for "${region}")-control-plane"
}

vault_call() {
	local method="$1" path="$2" data="${3:-}"
	if [[ -n "${data}" ]]; then
		curl -fsS --connect-timeout 5 --max-time 30 -H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" -H 'Content-Type: application/json' \
			-X "${method}" --data-binary "${data}" "${VAULT_ADDR}/v1/${path}"
	else
		curl -fsS --connect-timeout 5 --max-time 30 -H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" -X "${method}" "${VAULT_ADDR}/v1/${path}"
	fi
}

enable_database_engine() {
	vault_call POST sys/mounts/database '{"type":"database"}' >/dev/null
}

configure_vault_source() {
	local source="$1"
	local host=${2:-$(gateway_host "${source}")}
	local port=${3:-${gateway_ports[$source]}}
	local smoke_test=${4:-true}
	local connection config role disposable lease
	connection="postgresql://{{username}}:{{password}}@${host}:${port}/postgres?sslmode=require"
	config=$(jq -n --arg url "${connection}" --arg username "${MANAGEMENT_USER}" --arg password "${MANAGEMENT_PASSWORD}" --arg role "${source}" \
		'{plugin_name:"postgresql-database-plugin",allowed_roles:$role,connection_url:$url,username:$username,password:$password,verify_connection:false}')
	vault_call PUT "database/config/${source}" "${config}" >/dev/null
	role=$(jq -n --arg db "${source}" \
		--arg creation 'CREATE ROLE "{{name}}" WITH LOGIN PASSWORD '\''{{password}}'\'' VALID UNTIL '\''{{expiration}}'\'' REPLICATION;' \
		--arg revocation 'DROP ROLE IF EXISTS "{{name}}";' \
		'{db_name:$db,creation_statements:[$creation],revocation_statements:[$revocation],default_ttl:"768h",max_ttl:"768h"}')
	vault_call PUT "database/roles/${source}" "${role}" >/dev/null
	if [[ "${smoke_test}" != true ]]; then
		return 0
	fi
	disposable=$(vault_call GET "database/creds/${source}")
	jq -e '.lease_duration == 2764800 and .lease_id != "" and .data.username != "" and .data.password != ""' <<<"${disposable}" >/dev/null
	lease=$(jq -r '.lease_id' <<<"${disposable}")
	vault_call PUT sys/leases/revoke "$(jq -n --arg lease "${lease}" '{lease_id:$lease}')" >/dev/null
}

create_replica() {
	local region="$1" namespace="$2" name="$3" source="$4" mode="${5:-standalone}"
	local context host port
	context=$(context_for "${region}")
	host=$(gateway_host "${source}")
	port=${gateway_ports[$source]}
	kubectl --context "${context}" -n "${namespace}" create secret generic "${name}-credentials" \
		--from-literal=password="${DUMMY_PASSWORD}" >/dev/null
	if [[ "${mode}" == distributed ]]; then
		cat <<EOF | kubectl --context "${context}" apply -f - >/dev/null
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: ${name}, namespace: ${namespace}}
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:${POSTGRES_VERSION}
  storage: {size: 1Gi}
  affinity:
    nodeSelector: {postgres.node.kubernetes.io: ""}
    tolerations: [{key: node-role.kubernetes.io/postgres, operator: Exists}]
  postgresql:
    pg_hba: ["hostssl replication all all scram-sha-256", "hostssl all all all scram-sha-256"]
  bootstrap: {pg_basebackup: {source: ${source}}}
  replica: {primary: ${source}, source: ${source}}
  externalClusters:
    - name: ${source}
      connectionParameters: {host: ${host}, port: "${port}", user: ${DUMMY_USER}, dbname: postgres, sslmode: require}
      password: {name: ${name}-credentials, key: password}
    - name: ${name}
      connectionParameters: {host: 127.0.0.1, user: unused, dbname: postgres}
      password: {name: ${name}-credentials, key: password}
EOF
	else
		cat <<EOF | kubectl --context "${context}" apply -f - >/dev/null
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: ${name}, namespace: ${namespace}}
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:${POSTGRES_VERSION}
  storage: {size: 1Gi}
  affinity:
    nodeSelector: {postgres.node.kubernetes.io: ""}
    tolerations: [{key: node-role.kubernetes.io/postgres, operator: Exists}]
  postgresql:
    pg_hba: ["hostssl replication all all scram-sha-256", "hostssl all all all scram-sha-256"]
  bootstrap: {pg_basebackup: {source: ${source}}}
  replica: {enabled: true, source: ${source}}
  externalClusters:
    - name: ${source}
      connectionParameters: {host: ${host}, port: "${port}", user: ${DUMMY_USER}, dbname: postgres, sslmode: require}
      password: {name: ${name}-credentials, key: password}
EOF
	fi
	wait_for "controller username patch for ${name}" 300 username_changed "${context}" "${namespace}" "${name}" "${source}"
	# A running pg_basebackup Job snapshots its username at creation. Recreate
	# the failed test-owned bootstrap attempt after the controller's ordered
	# password/username mutation; CNPG then owns the replacement Job.
	local bootstrap_job
	bootstrap_job=$(kubectl --context "${context}" -n "${namespace}" get jobs -o name 2>/dev/null | grep -F "job.batch/${name}-1-pgbasebackup" || true)
	if [[ -n "${bootstrap_job}" ]]; then
		kubectl --context "${context}" -n "${namespace}" delete "${bootstrap_job}" >/dev/null
	fi
	wait_cluster_ready "${context}" "${namespace}" "${name}"
	wait_for "committed initial rotation for ${name}" 600 state_committed "${context}" "${namespace}" "${name}" ""
	assert_replication "${source}" "${region}" "${namespace}" "${name}"
}

username_changed() {
	[[ "$(cluster_user "$1" "$2" "$3" "$4")" != "${DUMMY_USER}" ]]
}

assert_replication() {
	local source="$1" replica_region="$2" namespace="$3" replica="$4"
	local source_region=${gateway_regions[$source]}
	local source_context replica_context source_primary replica_primary replication_user marker
	source_context=$(context_for "${source_region}")
	replica_context=$(context_for "${replica_region}")
	source_primary=$(primary_pod "${source_context}" "${namespace}" "${source}")
	replica_primary=$(primary_pod "${replica_context}" "${namespace}" "${replica}")
	replication_user=$(cluster_user "${replica_context}" "${namespace}" "${replica}" "${source}")
	wait_for "source streaming sender for ${replica}" 300 source_streaming \
		"${source_context}" "${namespace}" "${source_primary}" "${replication_user}"
	wait_for "replica WAL receiver for ${replica}" 300 replica_streaming "${replica_context}" "${namespace}" "${replica_primary}"
	marker="marker-${source}-${replica}-$(date +%s%N)"
	kubectl --context "${source_context}" -n "${namespace}" exec "${source_primary}" -- psql -U postgres -v ON_ERROR_STOP=1 \
		-c 'CREATE TABLE IF NOT EXISTS e2e_markers (id text PRIMARY KEY);' \
		-c "INSERT INTO e2e_markers VALUES ('${marker}');" >/dev/null
	wait_for "WAL marker ${source} to ${replica}" 300 marker_visible "${replica_context}" "${namespace}" "${replica_primary}" "${marker}"
	printf 'WAL %s/%s->%s marker=%s\n' "${namespace}" "${source}" "${replica}" "${marker}" >>"${RESULTS_FILE}"
}

source_streaming() {
	local count
	count=$(source_streaming_count "$@" 2>/dev/null || true)
	[[ "${count}" =~ ^[0-9]+$ ]] && ((count >= 1))
}

source_not_streaming() {
	[[ "$(source_streaming_count "$@" 2>/dev/null || true)" == 0 ]]
}

source_streaming_count() {
	printf '%s\n' \
		"SELECT count(*) FROM pg_stat_replication WHERE state='streaming' AND usename = :'expected_user';" |
		kubectl --context "$1" -n "$2" exec -i "$3" -- psql -U postgres \
			-v "expected_user=$4" -At
}

replica_streaming() {
	local result
	result=$(kubectl --context "$1" -n "$2" exec "$3" -- psql -U postgres -Atc \
		"SELECT pg_is_in_recovery(), EXISTS (SELECT 1 FROM pg_stat_wal_receiver WHERE status='streaming');" 2>/dev/null || true)
	[[ "${result}" == "t|t" ]]
}

replica_not_streaming() {
	local result
	result=$(kubectl --context "$1" -n "$2" exec "$3" -- psql -U postgres -Atc \
		"SELECT pg_is_in_recovery(), EXISTS (SELECT 1 FROM pg_stat_wal_receiver WHERE status='streaming');" 2>/dev/null || true)
	[[ "${result}" == "t|f" ]]
}

marker_visible() {
	[[ "$(kubectl --context "$1" -n "$2" exec "$3" -- psql -U postgres -Atc "SELECT count(*) FROM e2e_markers WHERE id='$4';" 2>/dev/null || true)" == 1 ]]
}

lease_gone() {
	local lease="$1"
	[[ -z "${lease}" ]] && return 0
	local response
	response=$(vault_call PUT sys/leases/lookup "$(jq -n --arg lease "${lease}" '{lease_id:$lease}')" 2>/dev/null || true)
	[[ -z "${response}" ]] || jq -e '.errors != null' <<<"${response}" >/dev/null
}

lease_exists() {
	local lease="$1"
	[[ -n "${lease}" ]] || return 1
	vault_call PUT sys/leases/lookup "$(jq -n --arg lease "${lease}" '{lease_id:$lease}')" >/dev/null 2>&1
}

vault_role_lease_count() {
	local source="$1" response
	response=$(vault_call LIST "sys/leases/lookup/database/creds/${source}" 2>/dev/null || true)
	jq -r '.data.keys | length // 0' <<<"${response}" 2>/dev/null || echo 0
}

pending_reconnect() {
	[[ "$(state_pending_stage "$1" "$2" "$3")" == reconnect-pending ]]
}

pending_or_current_lease_is() {
	local pending current
	pending=$(state_pending_lease "$1" "$2" "$3")
	current=$(state_lease "$1" "$2" "$3")
	[[ "${pending}" == "$4" || "${current}" == "$4" ]]
}

wait_rotation() {
	local context="$1" namespace="$2" cluster="$3" previous_lease="$4"
	wait_for "rotation for ${cluster}" 600 state_committed "${context}" "${namespace}" "${cluster}" "${previous_lease}"
	if [[ -n "${previous_lease}" ]]; then
		wait_for "old lease revocation for ${cluster}" 180 lease_gone "${previous_lease}"
	fi
	printf 'LEASE %s/%s fingerprint=%s\n' "${namespace}" "${cluster}" \
		"$(lease_fingerprint "$(state_lease "${context}" "${namespace}" "${cluster}")")" >>"${RESULTS_FILE}"
}

failover_cluster() {
	local context="$1" namespace="$2" cluster="$3"
	local current candidate
	current=$(primary_pod "${context}" "${namespace}" "${cluster}")
	candidate=$(kubectl --context "${context}" -n "${namespace}" get pods -l "cnpg.io/cluster=${cluster},cnpg.io/podRole=instance" -o json |
		jq -r --arg current "${current}" '.items[] | select(.metadata.name != $current) | .metadata.name' | head -1)
	test -n "${candidate}"
	kubectl cnpg --context "${context}" --namespace "${namespace}" promote "${cluster}" "${candidate}" >/dev/null
	wait_for "new designated primary for ${cluster}" 300 primary_changed "${context}" "${namespace}" "${cluster}" "${current}"
}

primary_changed() {
	local current
	current=$(primary_pod "$1" "$2" "$3" 2>/dev/null || true)
	[[ -n "${current}" && "${current}" != "$4" ]]
}

assert_rbac() {
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		local service_account="vault-replica-controller"
		local actor="system:serviceaccount:cnpg-system:${service_account}"
		[[ "$(kubectl --context "${context}" auth can-i patch secrets -n e2e-bootstrap --as "${actor}")" == yes ]]
		for verb in get list watch update create delete; do
			[[ "$(kubectl --context "${context}" auth can-i "${verb}" secrets -n e2e-bootstrap --as "${actor}")" == no ]]
		done
		for verb in get list watch patch; do
			[[ "$(kubectl --context "${context}" auth can-i "${verb}" clusters.postgresql.cnpg.io -n e2e-bootstrap --as "${actor}")" == yes ]]
		done
		for verb in create update delete; do
			[[ "$(kubectl --context "${context}" auth can-i "${verb}" clusters.postgresql.cnpg.io -n e2e-bootstrap --as "${actor}")" == no ]]
		done
		for verb in get list watch; do
			[[ "$(kubectl --context "${context}" auth can-i "${verb}" pods -n e2e-bootstrap --as "${actor}")" == yes ]]
			[[ "$(kubectl --context "${context}" auth can-i "${verb}" nodes --as "${actor}")" == no ]]
		done
		[[ "$(kubectl --context "${context}" auth can-i get pods/proxy -n e2e-bootstrap --as "${actor}")" == yes ]]
		for namespace in e2e-bootstrap cnpg-system; do
			for verb in create patch; do
				[[ "$(kubectl --context "${context}" auth can-i "${verb}" events -n "${namespace}" --as "${actor}")" == yes ]]
			done
			for verb in get list watch; do
				[[ "$(kubectl --context "${context}" auth can-i "${verb}" events -n "${namespace}" --as "${actor}")" == no ]]
			done
		done
		if kubectl --context "${context}" get clusterrolebindings -o json |
			jq -e --arg subject "${service_account}" '.items[].subjects[]? | select(.kind == "ServiceAccount" and .name == $subject and .namespace == "cnpg-system")' >/dev/null; then
			echo "controller has a forbidden ClusterRoleBinding" >&2
			return 1
		fi
	done
}

prom_query() {
	local region="$1" query="$2" local_port pid response
	if [[ "${region}" == us ]]; then local_port=19090; else local_port=19091; fi
	kubectl --context "$(context_for "${region}")" -n cnpg-system port-forward service/prometheus "${local_port}:9090" \
		>"${ARTIFACT_DIR}/prometheus-query-${region}.log" 2>&1 &
	pid=$!
	for _ in $(seq 1 20); do
		response=$(curl -fsS --connect-timeout 2 --max-time 5 --get --data-urlencode "query=${query}" "http://127.0.0.1:${local_port}/api/v1/query" 2>/dev/null || true)
		if jq -e '.status == "success"' <<<"${response}" >/dev/null 2>&1; then
			kill "${pid}" >/dev/null 2>&1 || true
			wait "${pid}" 2>/dev/null || true
			jq -r '.data.result[0].value[1] // "0"' <<<"${response}"
			return 0
		fi
		sleep 1
	done
	kill "${pid}" >/dev/null 2>&1 || true
	wait "${pid}" 2>/dev/null || true
	return 1
}

assert_metrics() {
	local region="$1" namespace="$2" cluster="$3"
	local lease_value first second
	wait_for "successful rotation metric" 60 metric_positive "${region}" 'sum(vault_replica_rotations_total{result="success"})'
	wait_for "Vault issue metric" 60 metric_positive "${region}" 'sum(vault_replica_vault_operations_total{operation="issue",result="success"})'
	lease_value=$(prom_query "${region}" "vault_replica_current_lease_time_to_expiration_seconds{namespace=\"${namespace}\",cluster=\"${cluster}\"}")
	awk 'BEGIN {exit !('"${lease_value}"' > 0)}'
	first=${lease_value%.*}
	sleep 6
	second=$(prom_query "${region}" "vault_replica_current_lease_time_to_expiration_seconds{namespace=\"${namespace}\",cluster=\"${cluster}\"}")
	second=${second%.*}
	((second < first))
}

metric_positive() {
	local value
	value=$(prom_query "$1" "$2")
	awk 'BEGIN {exit !('"${value}"' > 0)}'
}

metric_equals() {
	local region="$1" query="$2" expected="$3" value
	if wait_for "exact Prometheus value ${expected} for ${query}" 60 \
		metric_value_equals "${region}" "${query}" "${expected}"; then
		return 0
	fi
	value=$(prom_query "${region}" "${query}" || echo unavailable)
	echo "Prometheus value mismatch: query=${query} actual=${value} expected=${expected}" >&2
	return 1
}

metric_value_equals() {
	local value
	value=$(prom_query "$1" "$2") || return 1
	awk -v actual="${value}" -v expected="$3" 'BEGIN {exit !(actual == expected)}'
}

assert_metric_delta() {
	local region="$1" query="$2" before="$3" expected_delta="$4" after
	if wait_for "exact Prometheus delta ${expected_delta} for ${query}" 60 \
		metric_delta_equals "${region}" "${query}" "${before}" "${expected_delta}"; then
		return 0
	fi
	after=$(prom_query "${region}" "${query}" || echo unavailable)
	echo "Prometheus delta mismatch: query=${query} before=${before} after=${after} expected=${expected_delta}" >&2
	return 1
}

metric_delta_equals() {
	local after
	after=$(prom_query "$1" "$2") || return 1
	awk -v before="$3" -v after="${after}" -v delta="$4" \
		'BEGIN {exit !((after - before) == delta)}'
}

wal_receiver_inactive() {
	local pod response
	pod=$(primary_pod "$1" "$2" "$3" 2>/dev/null || true)
	[[ -n "${pod}" ]] || return 1
	response=$(kubectl --context "$1" get --raw \
		"/api/v1/namespaces/$2/pods/https:${pod}:8000/proxy/pg/status" 2>/dev/null || true)
	jq -e '[.. | objects | select(has("isWalReceiverActive")) | .isWalReceiverActive] | any(. == false)' \
		<<<"${response}" >/dev/null 2>&1
}

snapshot_redacted() {
	local context="$1" region="$2"
	state_json "${context}" | jq '
		walk(if type == "object" then
			(if has("currentLeaseID") then .currentLeaseID = "[REDACTED_LEASE]" else . end) |
			(if has("leaseID") then .leaseID = "[REDACTED_LEASE]" else . end) |
			(if has("username") then .username = "[REDACTED_USERNAME]" else . end)
		else . end)' >"${ARTIFACT_DIR}/state-${region}.json"
	kubectl --context "${context}" get clusters.postgresql.cnpg.io -A -o json |
		jq 'del(.items[]?.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]) |
		walk(if type == "object" then
			(if has("user") then .user = "[REDACTED_USERNAME]" else . end) |
			(if has("promotionToken") then .promotionToken = "[REDACTED_TOKEN]" else . end) |
			(if has("demotionToken") then .demotionToken = "[REDACTED_TOKEN]" else . end)
		else . end)' \
		>"${ARTIFACT_DIR}/clusters-${region}.json"
}

scan_sensitive_artifacts() {
	if grep -RInE --exclude='scenario-results.txt' --exclude='versions.txt' \
		"${VAULT_ROOT_TOKEN}|${MANAGEMENT_PASSWORD}|${DUMMY_PASSWORD}|${DUMMY_USER}|v-token-" "${ARTIFACT_DIR}"; then
		echo "sensitive value found in E2E artifacts" >&2
		return 1
	fi
}

run_ordered_suite() {
	: >"${RESULTS_FILE}"
	setup_environment
	assert_rbac
	phase "0 full environment, bounded watches, RBAC, and Prometheus targets"

	enable_database_engine
	onboard_namespace e2e-first
	local eu_ip
	eu_ip=$(gateway_host_for_region eu)

	create_source us e2e-first db01 distributed db02 "${eu_ip}" 15433
	deploy_gateway us e2e-first db01
	configure_vault_source db01
	create_replica eu e2e-first db02 db01 distributed
	# Reserve the reverse gateway port encoded in db01's distributed topology.
	deploy_gateway_at eu e2e-first db02 15433 18082

	create_source eu e2e-first db03
	deploy_gateway eu e2e-first db03
	configure_vault_source db03
	create_replica us e2e-first db04 db03 standalone
	assert_metrics eu e2e-first db02
	assert_metrics us e2e-first db04
	metric_equals eu 'sum(vault_replica_rotations_total{result="success"})' 1
	metric_equals us 'sum(vault_replica_rotations_total{result="success"})' 1
	state_absent "${US_CONTEXT}" e2e-first db01
	state_absent "${EU_CONTEXT}" e2e-first db03
	phase "1 two directional source/replica flows and initial dynamic issuance"

	# Startup informer observations must be deduplicated against lastEvent.
	local previous context lease_count
	context=${EU_CONTEXT}
	previous=$(state_lease "${context}" e2e-first db02)
	lease_count=$(vault_role_lease_count db01)
	restart_controller "${context}"
	sleep 20
	[[ "$(state_lease "${context}" e2e-first db02)" == "${previous}" ]]
	[[ "$(vault_role_lease_count db01)" == "${lease_count}" ]]

	failover_cluster "${context}" e2e-first db02
	wait_for "persisted reconnect stage for db02" 90 pending_reconnect "${context}" e2e-first db02
	local pending
	pending=$(state_pending_lease "${context}" e2e-first db02)
	[[ -n "${pending}" && "$(vault_role_lease_count db01)" == 2 ]]
	restart_controller "${context}"
	pending_or_current_lease_is "${context}" e2e-first db02 "${pending}"
	[[ "$(vault_role_lease_count db01)" == 2 ]]
	wait_rotation "${context}" e2e-first db02 "${previous}"
	[[ "$(state_lease "${context}" e2e-first db02)" == "${pending}" ]]
	[[ "$(vault_role_lease_count db01)" == 1 ]]
	assert_replication db01 eu e2e-first db02
	phase "2.1 explicit replica failover, persisted-stage restart, and duplicate suppression"

	previous=$(state_lease "${context}" e2e-first db02)
	local old_uid primary rotation_before issue_before revoke_before
	rotation_before=$(prom_query eu 'sum(vault_replica_rotations_total{result="success"})')
	issue_before=$(prom_query eu 'sum(vault_replica_vault_operations_total{operation="issue",result="success"})')
	revoke_before=$(prom_query eu 'sum(vault_replica_vault_operations_total{operation="revoke",result="success"})')
	primary=$(primary_pod "${context}" e2e-first db02)
	old_uid=$(primary_uid "${context}" e2e-first db02)
	kubectl --context "${context}" -n e2e-first delete pod "${primary}" >/dev/null
	wait_for "replacement Pod UID" 300 pod_uid_changed "${context}" e2e-first db02 "${old_uid}"
	wait_rotation "${context}" e2e-first db02 "${previous}"
	assert_metric_delta eu 'sum(vault_replica_rotations_total{result="success"})' "${rotation_before}" 1
	assert_metric_delta eu 'sum(vault_replica_vault_operations_total{operation="issue",result="success"})' "${issue_before}" 1
	assert_metric_delta eu 'sum(vault_replica_vault_operations_total{operation="revoke",result="success"})' "${revoke_before}" 1
	assert_replication db01 eu e2e-first db02
	phase "2.2 designated-primary Pod deletion and replacement"

	previous=$(state_lease "${context}" e2e-first db02)
	rotation_before=$(prom_query eu 'sum(vault_replica_rotations_total{result="success"})')
	local drained_node
	drained_node=$(primary_node "${context}" e2e-first db02)
	kubectl --context "${context}" cordon "${drained_node}" >/dev/null
	kubectl --context "${context}" drain "${drained_node}" --ignore-daemonsets --delete-emptydir-data --force --timeout=5m >/dev/null
	wait_for "primary leaves drained node" 300 primary_not_on_node "${context}" e2e-first db02 "${drained_node}"
	wait_rotation "${context}" e2e-first db02 "${previous}"
	assert_metric_delta eu 'sum(vault_replica_rotations_total{result="success"})' "${rotation_before}" 1
	kubectl --context "${context}" uncordon "${drained_node}" >/dev/null
	assert_replication db01 eu e2e-first db02
	phase "2.3 primary worker cordon and drain"

	# Ready is not authentication proof. Converge to one instance so no standby
	# can retain an already-authenticated receiver session, stop this regional
	# controller, inject a bad password, and force the sole Pod to reload it.
	previous=$(state_lease "${context}" e2e-first db02)
	kubectl --context "${context}" -n e2e-first patch cluster db02 --type=merge \
		-p '{"spec":{"instances":1}}' >/dev/null
	wait_for "db02 single-instance convergence" 300 cluster_pod_count_is "${context}" e2e-first db02 1
	wait_cluster_ready "${context}" e2e-first db02
	old_uid=$(primary_uid "${context}" e2e-first db02)
	kubectl --context "${context}" -n cnpg-system scale deployment/vault-replica-controller --replicas=0 >/dev/null
	kubectl --context "${context}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
	kubectl --context "${context}" -n e2e-first patch secret db02-credentials --type=merge \
		-p "$(jq -n --arg value "${DUMMY_PASSWORD}" '{data:{password:($value|@base64)}}')" >/dev/null
	primary=$(primary_pod "${context}" e2e-first db02)
	kubectl --context "${context}" -n e2e-first delete pod "${primary}" >/dev/null
	wait_for "invalid-password replacement Pod" 300 pod_uid_changed "${context}" e2e-first db02 "${old_uid}"
	wait_for "inactive WAL receiver with invalid password" 300 wal_receiver_inactive "${context}" e2e-first db02
	wait_cluster_ready "${context}" e2e-first db02
	local source_primary failed_replica_primary failed_replication_user
	source_primary=$(primary_pod "${US_CONTEXT}" e2e-first db01)
	failed_replica_primary=$(primary_pod "${context}" e2e-first db02)
	failed_replication_user=$(cluster_user "${context}" e2e-first db02 db01)
	wait_for "source sender for the rejected credential to stop" 120 source_not_streaming \
		"${US_CONTEXT}" e2e-first "${source_primary}" "${failed_replication_user}"
	wait_for "replica SQL receiver to report non-streaming" 120 replica_not_streaming \
		"${context}" e2e-first "${failed_replica_primary}"
	lease_exists "${previous}"
	[[ "$(state_lease "${context}" e2e-first db02)" == "${previous}" ]]
	kubectl --context "${context}" -n cnpg-system scale deployment/vault-replica-controller --replicas=1 >/dev/null
	kubectl --context "${context}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
	wait_for "controller leader acquisition in ${context}" 120 controller_is_leader "${context}"
	wait_rotation "${context}" e2e-first db02 "${previous}"
	assert_replication db01 eu e2e-first db02
	kubectl --context "${context}" -n e2e-first patch cluster db02 --type=merge \
		-p '{"spec":{"instances":2}}' >/dev/null
	wait_for "db02 two-instance restoration" 600 cluster_pod_count_is "${context}" e2e-first db02 2
	wait_cluster_ready "${context}" e2e-first db02
	assert_replication db01 eu e2e-first db02
	phase "2.4 invalid-password rejection and normal rotation recovery"

	local promoted_prior_lease
	promoted_prior_lease=$(state_lease "${EU_CONTEXT}" e2e-first db02)
	kubectl --context "${US_CONTEXT}" -n e2e-first patch cluster db01 --type=merge \
		-p '{"spec":{"replica":{"primary":"db02","source":"db02"}}}' >/dev/null
	wait_for "db01 demotion token" 600 demotion_token_present "${US_CONTEXT}" e2e-first db01
	local token
	token=$(kubectl --context "${US_CONTEXT}" -n e2e-first get cluster db01 -o jsonpath='{.status.demotionToken}')
	kubectl --context "${EU_CONTEXT}" -n e2e-first patch cluster db02 --type=merge \
		-p "$(jq -n --arg token "${token}" '{spec:{replica:{primary:"db02",source:"db01",promotionToken:$token}}}')" >/dev/null
	wait_for "db02 healthy after promotion" 900 cluster_promoted_healthy "${EU_CONTEXT}" e2e-first db02
	sleep 15
	cluster_promoted_healthy "${EU_CONTEXT}" e2e-first db02
	# The old db01-issued role exists on the promoted database through WAL.
	# Point Vault's db01 connection at the new writer so cleanup SQL for db02's
	# former replication lease can succeed after the old source became read-only.
	configure_vault_source db01 "${eu_ip}" 15433 false
	restart_controller "${EU_CONTEXT}"
	wait_for "db02 promotion credential cleanup" 300 state_absent "${EU_CONTEXT}" e2e-first db02
	wait_for "db02 promoted lease revocation" 180 lease_gone "${promoted_prior_lease}"
	configure_vault_source db02 "${eu_ip}" 15433 true
	# The former primary was already declared a replica while the new source
	# was read-only. A startup observation promptly retries after its Vault role
	# becomes usable without relying on an unrelated CNPG status update.
	restart_controller "${US_CONTEXT}"
	wait_cluster_ready "${US_CONTEXT}" e2e-first db01
	wait_for "db01 reverse rotation" 900 state_committed "${US_CONTEXT}" e2e-first db01 ""
	assert_replication db02 us e2e-first db01
	phase "3 cross-region distributed switchover and reverse replication"

	previous=$(state_lease "${US_CONTEXT}" e2e-first db04)
	local promoted_user promoted_password
	promoted_user=$(cluster_user "${US_CONTEXT}" e2e-first db04 db03)
	promoted_password=$(secret_password "${US_CONTEXT}" e2e-first db04-credentials)
	kubectl --context "${US_CONTEXT}" -n e2e-first patch cluster db04 --type=merge -p '{"spec":{"replica":{"enabled":false}}}' >/dev/null
	wait_for "db04 standalone cleanup" 300 state_absent "${US_CONTEXT}" e2e-first db04
	wait_for "db04 prior lease revoked" 180 lease_gone "${previous}"
	[[ "$(cluster_user "${US_CONTEXT}" e2e-first db04 db03)" == "${promoted_user}" ]]
	[[ "$(secret_password "${US_CONTEXT}" e2e-first db04-credentials)" == "${promoted_password}" ]]
	wait_for "db04 writable after promotion" 600 cluster_promoted_healthy "${US_CONTEXT}" e2e-first db04
	deploy_gateway us e2e-first db04
	configure_vault_source db04
	create_replica us e2e-first db05 db03 standalone
	create_replica eu e2e-first db06 db04 standalone
	phase "4 standalone promotion cleanup and db05/db06 rebuilds"

	onboard_namespace e2e-second
	create_source us e2e-second db07
	deploy_gateway us e2e-second db07
	configure_vault_source db07
	create_replica eu e2e-second db08 db07 standalone
	phase "5 second namespace onboarding and replication"

	local first_state_before
	first_state_before=$(state_json "${EU_CONTEXT}" | jq -c '.clusters | with_entries(select(.key | startswith("e2e-first/")))')
	for region in us eu; do
		update_controller_watch "$(context_for "${region}")" 'e2e-bootstrap,e2e-second'
	done
	previous=$(state_lease "${EU_CONTEXT}" e2e-first db06)
	failover_cluster "${EU_CONTEXT}" e2e-first db06
	sleep 20
	[[ "$(state_lease "${EU_CONTEXT}" e2e-first db06)" == "${previous}" ]]
	[[ "$(state_json "${EU_CONTEXT}" | jq -c '.clusters | with_entries(select(.key | startswith("e2e-first/")))')" == "${first_state_before}" ]]
	assert_replication db04 eu e2e-first db06
	previous=$(state_lease "${EU_CONTEXT}" e2e-first db06)
	for region in us eu; do
		update_controller_watch "$(context_for "${region}")" "${CONTROLLER_WATCH}"
	done
	# Re-adding the namespace exposes the failover that occurred while it was
	# unwatched. Let the startup observation reconcile that missed topology
	# before triggering a distinct, deterministic watched failover.
	wait_rotation "${EU_CONTEXT}" e2e-first db06 "${previous}"
	assert_replication db04 eu e2e-first db06
	previous=$(state_lease "${EU_CONTEXT}" e2e-first db06)
	failover_cluster "${EU_CONTEXT}" e2e-first db06
	wait_rotation "${EU_CONTEXT}" e2e-first db06 "${previous}"
	assert_replication db04 eu e2e-first db06
	phase "6 controller-only namespace removal, no rotation, and restored rotation"

	previous=$(state_lease "${EU_CONTEXT}" e2e-second db08)
	kubectl --context "${EU_CONTEXT}" -n e2e-second delete cluster db08 --wait=false >/dev/null
	wait_for "two-sweep db08 cleanup" 900 state_absent "${EU_CONTEXT}" e2e-second db08
	wait_for "db08 lease revocation" 180 lease_gone "${previous}"
	[[ "$(kubectl --context "${EU_CONTEXT}" -n e2e-second get secret db08-credentials -o name)" == secret/db08-credentials ]]
	metric_equals eu 'count(vault_replica_current_lease_time_to_expiration_seconds{namespace="e2e-second",cluster="db08"})' 0
	metric_equals eu 'sum(vault_replica_pending_workflows)' 0
	kubectl --context "${US_CONTEXT}" -n e2e-second delete cluster db07 --wait=true >/dev/null
	phase "7 database deletion, two-sweep lease cleanup, and retained target Secret"

	for region in us eu; do
		snapshot_redacted "$(context_for "${region}")" "${region}"
	done
	collect_logs
	if ! scan_sensitive_artifacts; then
		return 1
	fi
	phase "redaction, metrics, and final diagnostics"
}

gateway_host_for_region() {
	docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "$(cluster_for "$1")-control-plane"
}

deploy_gateway_at() {
	local region="$1" namespace="$2" source="$3" port="$4" health="$5"
	local context
	context=$(context_for "${region}")
	gateway_ports["${source}"]=${port}
	gateway_regions["${source}"]=${region}
	if ((next_gateway_port <= port)); then next_gateway_port=$((port + 1)); fi
	if ((next_health_port <= health)); then next_health_port=$((health + 1)); fi
	sed -e "s|__GATEWAY_NAME__|gateway-${source}|g" -e "s|__NAMESPACE__|${namespace}|g" \
		-e "s|__GATEWAY_IMAGE__|${GATEWAY_IMAGE}|g" -e "s|__GATEWAY_PORT__|${port}|g" \
		-e "s|__HEALTH_PORT__|${health}|g" -e "s|__BACKEND_SERVICE__|${source}-rw.${namespace}.svc|g" \
		"${E2E_DIR}/manifests/gateway.yaml" | kubectl --context "${context}" apply -f - >/dev/null
	kubectl --context "${context}" -n "${namespace}" rollout status "deployment/gateway-${source}" --timeout=5m >/dev/null
}

pod_uid_changed() {
	local current
	current=$(primary_uid "$1" "$2" "$3" 2>/dev/null || true)
	[[ -n "${current}" && "${current}" != "$4" ]]
}

primary_not_on_node() {
	local node
	node=$(primary_node "$1" "$2" "$3" 2>/dev/null || true)
	[[ -n "${node}" && "${node}" != "$4" ]]
}

demotion_token_present() {
	[[ -n "$(kubectl --context "$1" -n "$2" get cluster "$3" -o jsonpath='{.status.demotionToken}' 2>/dev/null || true)" ]]
}

cluster_not_recovery() {
	local primary result
	primary=$(primary_pod "$1" "$2" "$3" 2>/dev/null || true)
	[[ -n "${primary}" ]] || return 1
	result=$(kubectl --context "$1" -n "$2" exec "${primary}" -- psql -U postgres -Atc 'SELECT pg_is_in_recovery();' 2>/dev/null || true)
	[[ "${result}" == f ]]
}

cluster_promoted_healthy() {
	local cluster_json primary result
	cluster_json=$(kubectl --context "$1" -n "$2" get cluster "$3" -o json 2>/dev/null || true)
	jq -e '
		.status.phase == "Cluster in healthy state" and
		([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length == 1) and
		.status.currentPrimary != "" and .status.currentPrimary == .status.targetPrimary' \
		<<<"${cluster_json}" >/dev/null 2>&1 || return 1
	primary=$(jq -r '.status.currentPrimary' <<<"${cluster_json}")
	result=$(kubectl --context "$1" -n "$2" exec "${primary}" -- psql -U postgres -Atc \
		'SELECT pg_is_in_recovery();' 2>/dev/null || true)
	[[ "${result}" == f ]]
}

run_ordered_suite
