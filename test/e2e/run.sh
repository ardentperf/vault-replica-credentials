#!/usr/bin/env bash
set -euo pipefail

# The E2E entrypoint owns one complete clean environment. Scenario helpers are
# deliberately shell functions so every action and diagnostic remains usable
# from a developer checkout without CI-only orchestration.
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
E2E_DIR="${ROOT_DIR}/test/e2e"
ARTIFACT_DIR="${E2E_ARTIFACT_DIR:-${E2E_DIR}/artifacts/$(date -u +%Y%m%dT%H%M%SZ)}"
KEEP_E2E_CLUSTERS="${KEEP_E2E_CLUSTERS:-false}"
US_CONTEXT=kind-k8s-us
EU_CONTEXT=kind-k8s-eu
US_CLUSTER=k8s-us
EU_CLUSTER=k8s-eu
FIRST_NAMESPACE=e2e-first
SECOND_NAMESPACE=e2e-second
POSTGRES_IMAGE="${POSTGRES_IMAGE:-ghcr.io/cloudnative-pg/postgresql:18.1-standard-trixie}"
VAULT_ROOT_TOKEN="${VAULT_ROOT_TOKEN:-e2e-dev-root-token}"
VAULT_HOST_PORT="${VAULT_HOST_PORT:-18200}"
GATEWAY_IMAGE="${GATEWAY_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials-gateway:e2e}"
BOOTSTRAP_USER=e2e_bootstrap
BOOTSTRAP_PASSWORD=e2eBootstrapPassword
MANAGEMENT_USER=vault_replica_admin
MANAGEMENT_PASSWORD=e2eManagementPassword
E2E_PHASES="${E2E_PHASES:-all}"
E2E_SKIP_SETUP="${E2E_SKIP_SETUP:-false}"

mkdir -p "${ARTIFACT_DIR}"

log() { echo "[e2e] $*"; }

cleanup() {
	local status=$?
	if [[ "${status}" -ne 0 ]]; then
		collect_failure_diagnostics
	fi
	redact_artifacts
	if [[ "${KEEP_E2E_CLUSTERS}" != "true" ]]; then
		kind delete cluster --name "${US_CLUSTER}" >/dev/null 2>&1 || true
		kind delete cluster --name "${EU_CLUSTER}" >/dev/null 2>&1 || true
	fi
	exit "${status}"
}
trap cleanup EXIT

context_for() {
	case "$1" in
		us) echo "${US_CONTEXT}" ;;
		eu) echo "${EU_CONTEXT}" ;;
		*) return 1 ;;
	esac
}

phase() {
	local name=$1
	shift
	log "phase ${name}"
	# Do not wrap the scenario in an `if`: Bash disables errexit for a function
	# invoked as a conditional, which would turn a rejected CNPG manifest into a
	# misleading polling timeout. The cleanup trap retains diagnostics on error.
	"$@"
	printf '%s\n' "${name}: passed" >>"${ARTIFACT_DIR}/phases.txt"
}

collect_failure_diagnostics() {
	local region
	local context
	set +e
	for region in us eu; do
		context=$(context_for "${region}")
		kubectl --context "${context}" get nodes >"${ARTIFACT_DIR}/nodes-${region}.txt" 2>&1
		kubectl --context "${context}" get cluster -A -o json | \
			jq '(.items[]?.spec.externalClusters[]?.connectionParameters.user) = "[REDACTED]"' >"${ARTIFACT_DIR}/clusters-${region}.json" 2>&1
		kubectl --context "${context}" get pods -A -o wide >"${ARTIFACT_DIR}/pods-${region}.txt" 2>&1
		kubectl --context "${context}" -n cnpg-system logs deployment/vault-replica-controller --all-containers=true >"${ARTIFACT_DIR}/controller-${region}.log" 2>&1
		kubectl --context "${context}" -n cnpg-system logs deployment/cnpg-controller-manager --all-containers=true >"${ARTIFACT_DIR}/cnpg-${region}.log" 2>&1
	done
	kubectl --context "${US_CONTEXT}" -n vault logs deployment/vault --all-containers=true >"${ARTIFACT_DIR}/vault.log" 2>&1
	set -e
}

redact_artifacts() {
	local file
	while IFS= read -r -d '' file; do
		sed -i \
			-e "s/${VAULT_ROOT_TOKEN}/[REDACTED]/g" \
			-e "s/${BOOTSTRAP_PASSWORD}/[REDACTED]/g" \
			-e "s/${MANAGEMENT_PASSWORD}/[REDACTED]/g" \
			-E -e 's/("user(name)?"[[:space:]]*:[[:space:]]*")[^"]*(")/\1[REDACTED]\3/g' \
			-E -e 's/(user(name)?=)[^[:space:],]*/\1[REDACTED]/g' \
			"${file}" || true
	done < <(find "${ARTIFACT_DIR}" -type f -print0)
}

assert_phase_zero() {
	local context
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		kubectl --context "${context}" get nodes >/dev/null
		kubectl --context "${context}" -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=30s
		kubectl --context "${context}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=30s
		# A normal acceptance run must start with no database Cluster resources.
		# The skip-setup switch is solely a retained-environment diagnostic aid,
		# where already-created scenario resources are expected.
		if [[ "${E2E_SKIP_SETUP}" != true ]] && kubectl --context "${context}" get clusters.postgresql.cnpg.io -A -o name | grep -q .; then
			echo "unexpected database Cluster in ${context}" >&2
			return 1
		fi
		done
}

assert_write_only_secret_rbac() {
	local context
	local verb
	local answer
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		answer=$(kubectl --context "${context}" auth can-i patch secrets/example \
			--as=system:serviceaccount:cnpg-system:vault-replica-controller -n e2e-bootstrap || true)
		if [[ "${answer}" != yes ]]; then
			echo "${context}: expected patch secrets/example=yes, got ${answer}" >&2
			return 1
		fi
		for verb in get list watch create update delete; do
			answer=$(kubectl --context "${context}" auth can-i "${verb}" secrets/example \
				--as=system:serviceaccount:cnpg-system:vault-replica-controller -n e2e-bootstrap || true)
			if [[ "${answer}" != no ]]; then
				echo "${context}: expected ${verb} secrets/example=no, got ${answer}" >&2
				return 1
			fi
		done
		if kubectl --context "${context}" get clusterrolebinding -o json | \
			jq -e '.items[] | select(.subjects[]? | .kind == "ServiceAccount" and .name == "vault-replica-controller")' >/dev/null; then
			echo "controller has an unexpected ClusterRoleBinding in ${context}" >&2
			return 1
		fi
	done
}

assert_prometheus() {
	local region
	local context
	local port
	local process
	for region in us eu; do
		context=$(context_for "${region}")
		port=$([[ "${region}" == us ]] && echo 19190 || echo 19191)
		kubectl --context "${context}" -n cnpg-system port-forward service/e2e-prometheus "${port}:9090" \
			>"${ARTIFACT_DIR}/prometheus-${region}.log" 2>&1 &
		process=$!
		for _ in $(seq 1 30); do
			if curl --fail --silent "http://127.0.0.1:${port}/api/v1/targets" >"${ARTIFACT_DIR}/prometheus-${region}.json" && \
				jq -e '.data.activeTargets[] | select(.labels.job == "vault-replica-controller" and .health == "up")' "${ARTIFACT_DIR}/prometheus-${region}.json" >/dev/null; then
				break
			fi
			sleep 2
		done
		kill "${process}" >/dev/null 2>&1 || true
		jq -e '.data.activeTargets[] | select(.labels.job == "vault-replica-controller" and .health == "up")' "${ARTIFACT_DIR}/prometheus-${region}.json" >/dev/null
	done
}

kubectl_for() {
	local region=$1
	shift
	kubectl --context "$(context_for "${region}")" "$@"
}

control_plane_ip() {
	local region=$1
	local cluster=$US_CLUSTER
	if [[ "${region}" == eu ]]; then
		cluster=$EU_CLUSTER
	fi
	docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "${cluster}-control-plane"
}

gateway_port() {
	local number=${1#db}
	printf '%s\n' "$((15400 + 10#${number}))"
}

gateway_health_port() {
	local number=${1#db}
	printf '%s\n' "$((18000 + 10#${number}))"
}

wait_until() {
	local attempts=$1
	local description=$2
	shift 2
	local _
	for _ in $(seq 1 "${attempts}"); do
		if "$@"; then
			return 0
		fi
		sleep 5
	done
	echo "timed out waiting for ${description}" >&2
	return 1
}

primary_for() {
	local region=$1
	local namespace=$2
	local name=$3
	kubectl_for "${region}" -n "${namespace}" get cluster "${name}" -o jsonpath='{.status.currentPrimary}'
}

cluster_ready() {
	local region=$1
	local namespace=$2
	local name=$3
	kubectl_for "${region}" -n "${namespace}" wait --for=condition=Ready "cluster/${name}" --timeout=12m >/dev/null
	wait_until 48 "designated primary for ${namespace}/${name}" primary_exists "${region}" "${namespace}" "${name}"
}

primary_exists() {
	[[ -n "$(primary_for "$@")" ]]
}

sql() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local query=$4
	local primary
	primary=$(primary_for "${region}" "${namespace}" "${cluster}")
	kubectl_for "${region}" -n "${namespace}" exec "${primary}" -- \
		psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres -Atqc "${query}"
}

create_namespace_and_watches() {
	local namespaces=$1
	local region
	local namespace
	for region in us eu; do
		IFS=, read -r -a requested_namespaces <<<"${namespaces}"
		for namespace in "${requested_namespaces[@]}"; do
			kubectl_for "${region}" create namespace "${namespace}" --dry-run=client -o yaml | kubectl_for "${region}" apply -f - >/dev/null
			sed "s/namespace: reporting/namespace: ${namespace}/g" "${ROOT_DIR}/config/rbac/target-role.yaml" | kubectl_for "${region}" apply -f - >/dev/null
			sed "s/namespace: reporting/namespace: ${namespace}/g" "${ROOT_DIR}/config/rbac/target-rolebinding.yaml" | kubectl_for "${region}" apply -f - >/dev/null
		done
		# The controller's cache scope is process-start configuration. Restarting
		# is intentional and keeps removed namespaces out of the cache.
		kubectl_for "${region}" -n cnpg-system set env deployment/vault-replica-controller "WATCH_NAMESPACE=${namespaces}" >/dev/null
		kubectl_for "${region}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
		# CNPG documents this ConfigMap plus a rollout restart as its supported
		# namespace-watch reconfiguration path.
		kubectl_for "${region}" -n cnpg-system create configmap cnpg-controller-manager-config \
			--from-literal=WATCH_NAMESPACE="${namespaces}" \
			--from-literal=POSTGRES_IMAGE_NAME="${POSTGRES_IMAGE}" \
			--dry-run=client -o yaml | kubectl_for "${region}" apply -f - >/dev/null
		kubectl_for "${region}" -n cnpg-system rollout restart deployment/cnpg-controller-manager >/dev/null
		kubectl_for "${region}" -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=5m >/dev/null
		kubectl_for "${region}" -n cnpg-system get configmap cnpg-controller-manager-config \
			-o jsonpath='{.data.WATCH_NAMESPACE}' | grep -Fxq "${namespaces}"
	done
}

controller_only_watches() {
	local namespaces=$1
	local region
	for region in us eu; do
		kubectl_for "${region}" -n cnpg-system set env deployment/vault-replica-controller "WATCH_NAMESPACE=${namespaces}" >/dev/null
		kubectl_for "${region}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
		kubectl_for "${region}" -n cnpg-system get deployment vault-replica-controller -o json | \
			jq -e --arg value "${namespaces}" '.spec.template.spec.containers[] | select(.name == "manager").env[] | select(.name == "WATCH_NAMESPACE" and .value == $value)' >/dev/null
	done
}

controller_pods_absent() {
	local region=$1
	[[ "$(kubectl_for "${region}" -n cnpg-system get pods -l app.kubernetes.io/name=vault-replica-controller -o json | jq '.items | length')" == 0 ]]
}

pause_controller() {
	local region=$1
	kubectl_for "${region}" -n cnpg-system scale deployment/vault-replica-controller --replicas=0 >/dev/null
	wait_until 36 "controller stop in ${region}" controller_pods_absent "${region}"
}

resume_controller() {
	local region=$1
	kubectl_for "${region}" -n cnpg-system scale deployment/vault-replica-controller --replicas=1 >/dev/null
	kubectl_for "${region}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
}

render_gateway() {
	local name=$1
	local namespace=$2
	local port=$3
	local backend=$4
	sed \
		-e "s|__GATEWAY_NAME__|${name}-gateway|g" \
		-e "s|__NAMESPACE__|${namespace}|g" \
		-e "s|__GATEWAY_IMAGE__|${GATEWAY_IMAGE}|g" \
		-e "s|__GATEWAY_PORT__|${port}|g" \
		-e "s|__HEALTH_PORT__|$(gateway_health_port "${name}")|g" \
		-e "s|__BACKEND_SERVICE__|${backend}|g" \
		"${E2E_DIR}/manifests/gateway.yaml"
}

create_gateway() {
	local region=$1
	local namespace=$2
	local name=$3
	local port
	port=$(gateway_port "${name}")
	render_gateway "${name}" "${namespace}" "${port}" "${name}-rw.${namespace}.svc" | kubectl_for "${region}" apply -f - >/dev/null
	kubectl_for "${region}" -n "${namespace}" rollout status "deployment/${name}-gateway" --timeout=5m >/dev/null
}

create_password_secret() {
	local region=$1
	local namespace=$2
	local name=$3
	kubectl_for "${region}" -n "${namespace}" create secret generic "${name}" \
		--from-literal=password="${BOOTSTRAP_PASSWORD}" --dry-run=client -o yaml | kubectl_for "${region}" apply -f - >/dev/null
}

ensure_distributed_self_external() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local secret=$4
	local patch
	if kubectl_for "${region}" -n "${namespace}" get cluster "${cluster}" -o json | \
		jq -e --arg cluster "${cluster}" '.spec.externalClusters[]? | select(.name == $cluster)' >/dev/null; then
		return 0
	fi
	create_password_secret "${region}" "${namespace}" "${secret}"
	patch=$(jq -cn --arg cluster "${cluster}" --arg namespace "${namespace}" --arg secret "${secret}" --arg user "${BOOTSTRAP_USER}" \
		'[{op:"add", path:"/spec/externalClusters/-", value:{name:$cluster, connectionParameters:{host:($cluster + "-rw." + $namespace + ".svc"), port:"5432", user:$user, dbname:"postgres", sslmode:"disable"}, password:{name:$secret, key:"password"}}}]')
	kubectl_for "${region}" -n "${namespace}" patch cluster "${cluster}" --type=json -p "${patch}" >/dev/null
}

render_source() {
	local namespace=$1
	local name=$2
	local remote_name=$3
	local remote_host=$4
	local remote_port=$5
	local remote_secret=$6
	sed \
		-e "s|__NAMESPACE__|${namespace}|g" \
		-e "s|__CLUSTER_NAME__|${name}|g" \
		-e "s|__POSTGRES_IMAGE__|${POSTGRES_IMAGE}|g" \
		-e "s|__REMOTE_CLUSTER_NAME__|${remote_name}|g" \
		-e "s|__REMOTE_HOST__|${remote_host}|g" \
		-e "s|__REMOTE_PORT__|${remote_port}|g" \
		-e "s|__BOOTSTRAP_USER__|${BOOTSTRAP_USER}|g" \
		-e "s|__REMOTE_PASSWORD_SECRET__|${remote_secret}|g" \
		"${E2E_DIR}/manifests/source-cluster.yaml"
}

render_replica() {
	local namespace=$1
	local name=$2
	local primary=$3
	local source=$4
	local host=$5
	local port=$6
	local password_secret=$7
	sed \
		-e "s|__NAMESPACE__|${namespace}|g" \
		-e "s|__CLUSTER_NAME__|${name}|g" \
		-e "s|__POSTGRES_IMAGE__|${POSTGRES_IMAGE}|g" \
		-e "s|__PRIMARY_NAME__|${primary}|g" \
		-e "s|__SOURCE_NAME__|${source}|g" \
		-e "s|__SOURCE_HOST__|${host}|g" \
		-e "s|__SOURCE_PORT__|${port}|g" \
		-e "s|__BOOTSTRAP_USER__|${BOOTSTRAP_USER}|g" \
		-e "s|__PASSWORD_SECRET__|${password_secret}|g" \
		"${E2E_DIR}/manifests/replica-cluster.yaml"
}

create_source() {
	local region=$1
	local namespace=$2
	local name=$3
	local remote_name=$4
	local remote_region=$5
	local remote_secret=$6
	local remote_host
	remote_host=$(control_plane_ip "${remote_region}")
	render_source "${namespace}" "${name}" "${remote_name}" "${remote_host}" "$(gateway_port "${remote_name}")" "${remote_secret}" | kubectl_for "${region}" apply -f - >/dev/null
	cluster_ready "${region}" "${namespace}" "${name}"
	sql "${region}" "${namespace}" "${name}" 'SHOW server_version_num' | grep -q '^18'
	create_gateway "${region}" "${namespace}" "${name}"
	# The two static fixture users are created by the test actor, outside CNPG
	# declarative role management. A role creating REPLICATION roles must itself
	# have REPLICATION (or SUPERUSER), so the narrowly scoped management role
	# has both CREATEROLE and REPLICATION but never SUPERUSER.
	sql "${region}" "${namespace}" "${name}" "DO \$\$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${BOOTSTRAP_USER}') THEN CREATE ROLE ${BOOTSTRAP_USER} LOGIN REPLICATION PASSWORD '${BOOTSTRAP_PASSWORD}'; END IF; IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${MANAGEMENT_USER}') THEN CREATE ROLE ${MANAGEMENT_USER} LOGIN PASSWORD '${MANAGEMENT_PASSWORD}'; END IF; END \$\$; ALTER ROLE ${MANAGEMENT_USER} CREATEROLE REPLICATION;" >/dev/null
}

vault_exec() {
	kubectl_for us -n vault exec deployment/vault -- env \
		"VAULT_ADDR=http://127.0.0.1:${VAULT_HOST_PORT}" "VAULT_TOKEN=${VAULT_ROOT_TOKEN}" vault "$@"
}

ensure_vault_database_engine() {
	if ! vault_exec secrets list -format=json | jq -e 'has("database/")' >/dev/null; then
		vault_exec secrets enable database >/dev/null
	fi
}

onboard_source() {
	local region=$1
	local name=$2
	configure_vault_source "${region}" "${name}"
	# A disposable issuance proves the Vault/database configuration before it is
	# handed to the controller; only the lease ID crosses this shell boundary.
	local lease
	lease=$(vault_exec read -format=json "database/creds/${name}" | jq -r '.lease_id')
	[[ -n "${lease}" && "${lease}" != null ]]
	vault_exec lease revoke "${lease}" >/dev/null
}

configure_vault_source() {
	local region=$1
	local name=$2
	configure_vault_database_endpoint "${name}" "${region}" "${name}"
}

# Vault stores a lease's database connection by its original database name.
# During a cross-region promotion the same replicated role exists on the new
# writable primary, so the fixture must move that existing database connection
# before the controller can revoke leases issued by the former source.
configure_vault_database_endpoint() {
	local database_name=$1
	local target_region=$2
	local target_cluster=$3
	local host
	host=$(control_plane_ip "${target_region}")
	ensure_vault_database_engine
	vault_exec write "database/config/${database_name}" \
		plugin_name=postgresql-database-plugin allowed_roles="${database_name}" \
		"connection_url=postgresql://{{username}}:{{password}}@${host}:$(gateway_port "${target_cluster}")/postgres?sslmode=disable" \
		username="${MANAGEMENT_USER}" password="${MANAGEMENT_PASSWORD}" password_authentication=scram-sha-256 >/dev/null
	vault_exec write "database/roles/${database_name}" db_name="${database_name}" \
		"creation_statements=CREATE ROLE \"{{name}}\" WITH LOGIN REPLICATION PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';" \
		"revocation_statements=DROP ROLE IF EXISTS \"{{name}}\";" default_ttl=768h max_ttl=768h >/dev/null
}

assert_remote_gateway() {
	local region=$1
	local namespace=$2
	local host=$3
	local port=$4
	kubectl_for "${region}" -n "${namespace}" run "gateway-check-${region}-${port}" \
		--image=busybox:1.37.0 --restart=Never --rm --attach \
		--overrides='{"apiVersion":"v1","spec":{"tolerations":[{"operator":"Exists"}]}}' --command -- \
		sh -ec "nc -z -w 5 '${host}' '${port}'" >/dev/null
}

secret_password() {
	local region=$1
	local namespace=$2
	local name=$3
	kubectl_for "${region}" -n "${namespace}" get secret "${name}" -o jsonpath='{.data.password}' | base64 --decode
}

cluster_user() {
	local region=$1
	local namespace=$2
	local name=$3
	local source=$4
	kubectl_for "${region}" -n "${namespace}" get cluster "${name}" -o json | jq -r --arg source "${source}" '.spec.externalClusters[] | select(.name == $source) | .connectionParameters.user'
}

state_lease() {
	local region=$1
	local namespace=$2
	local name=$3
	kubectl_for "${region}" -n cnpg-system get secret vault-replica-controller-state -o json | \
		jq -r --arg key "${namespace}/${name}" '.data["state.json"] | @base64d | fromjson | .clusters[$key].currentLeaseID // empty'
}

state_entry_absent() {
	local region=$1
	local namespace=$2
	local name=$3
	[[ -z "$(state_lease "${region}" "${namespace}" "${name}")" ]]
}

state_lease_exists() {
	[[ -n "$(state_lease "$@")" ]]
}

state_lease_changed() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local previous=$4
	local current
	current=$(state_lease "${region}" "${namespace}" "${cluster}")
	[[ -n "${current}" && "${current}" != "${previous}" ]]
}

lease_is_revoked() {
	local lease=$1
	! vault_exec lease lookup "${lease}" >/dev/null 2>&1
}

wal_receiver_active() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local pod
	pod=$(primary_for "${region}" "${namespace}" "${cluster}")
	kubectl_for "${region}" get --raw "/api/v1/namespaces/${namespace}/pods/https:${pod}:8000/proxy/pg/status" | jq -e '.isWalReceiverActive == true' >/dev/null
}

replication_is_healthy() {
	local source_region=$1
	local source_namespace=$2
	local source=$3
	local replica_region=$4
	local replica=$5
	sql "${source_region}" "${source_namespace}" "${source}" "SELECT count(*) FROM pg_stat_replication WHERE state = 'streaming'" | grep -Eq '^[1-9][0-9]*$'
	sql "${replica_region}" "${source_namespace}" "${replica}" 'SELECT pg_is_in_recovery()' | grep -qx t
	sql "${replica_region}" "${source_namespace}" "${replica}" "SELECT count(*) FROM pg_stat_wal_receiver WHERE status = 'streaming'" | grep -Eq '^[1-9][0-9]*$'
}

assert_dynamic_login() {
	local source_region=$1
	local namespace=$2
	local source=$3
	local replica_region=$4
	local secret=$5
	local replica=$6
	local source_name=$7
	local user
	local password
	user=$(cluster_user "${replica_region}" "${namespace}" "${replica}" "${source_name}")
	password=$(secret_password "${replica_region}" "${namespace}" "${secret}")
	[[ "${user}" != "${BOOTSTRAP_USER}" && "${password}" != "${BOOTSTRAP_PASSWORD}" ]]
	local source_pod
	source_pod=$(primary_for "${source_region}" "${namespace}" "${source}")
	kubectl_for "${source_region}" -n "${namespace}" exec "${source_pod}" -- env PGPASSWORD="${password}" \
		psql -X -v ON_ERROR_STOP=1 -h "${source}-rw" -U "${user}" -d postgres -Atqc 'SELECT 1' | grep -qx 1
}

assert_marker() {
	local source_region=$1
	local namespace=$2
	local source=$3
	local replica_region=$4
	local replica=$5
	local marker=$6
	sql "${source_region}" "${namespace}" "${source}" 'CREATE TABLE IF NOT EXISTS e2e_rotation_markers (marker text PRIMARY KEY)' >/dev/null
	sql "${source_region}" "${namespace}" "${source}" "INSERT INTO e2e_rotation_markers(marker) VALUES ('${marker}')" >/dev/null
	wait_until 72 "WAL marker ${marker}" marker_visible "${replica_region}" "${namespace}" "${replica}" "${marker}"
}

marker_visible() {
	local region=$1
	local namespace=$2
	local replica=$3
	local marker=$4
	sql "${region}" "${namespace}" "${replica}" "SELECT marker FROM e2e_rotation_markers WHERE marker = '${marker}'" | grep -qx "${marker}"
}

assert_rotation() {
	local source_region=$1
	local namespace=$2
	local source=$3
	local replica_region=$4
	local replica=$5
	local secret=$6
	local prior_lease=${7:-}
	wait_until 96 "committed replacement lease for ${namespace}/${replica}" state_lease_changed "${replica_region}" "${namespace}" "${replica}" "${prior_lease}"
	local current_lease
	current_lease=$(state_lease "${replica_region}" "${namespace}" "${replica}")
	[[ -n "${current_lease}" && "${current_lease}" != "${prior_lease}" ]]
	assert_dynamic_login "${source_region}" "${namespace}" "${source}" "${replica_region}" "${secret}" "${replica}" "${source}"
	wait_until 72 "WAL receiver ${namespace}/${replica}" wal_receiver_active "${replica_region}" "${namespace}" "${replica}"
	wait_until 72 "streaming replication ${namespace}/${replica}" replication_is_healthy "${source_region}" "${namespace}" "${source}" "${replica_region}" "${replica}"
	assert_marker "${source_region}" "${namespace}" "${source}" "${replica_region}" "${replica}" "marker-${replica}-$(date -u +%s)"
	if [[ -n "${prior_lease}" ]]; then
		wait_until 48 "revocation of prior lease" lease_is_revoked "${prior_lease}"
	fi
}

create_pair() {
	local source_region=$1
	local source=$2
	local replica_region=$3
	local replica=$4
	local namespace=$5
	local source_remote_secret="${source}-to-${replica}-credentials"
	local replica_secret="${replica}-${source}-credentials"
	# The source's reverse external-cluster entry is required by CNPG's
	# distributed topology. Its gateway can safely exist before the remote
	# Service; it resolves the stable -rw DNS name only on a connection.
	create_password_secret "${source_region}" "${namespace}" "${source_remote_secret}"
	create_gateway "${replica_region}" "${namespace}" "${replica}"
	create_source "${source_region}" "${namespace}" "${source}" "${replica}" "${replica_region}" "${source_remote_secret}"
	onboard_source "${source_region}" "${source}"
	create_password_secret "${replica_region}" "${namespace}" "${replica_secret}"
	assert_remote_gateway "${replica_region}" "${namespace}" "$(control_plane_ip "${source_region}")" "$(gateway_port "${source}")"
	render_replica "${namespace}" "${replica}" "${source}" "${source}" \
		"$(control_plane_ip "${source_region}")" "$(gateway_port "${source}")" "${replica_secret}" | kubectl_for "${replica_region}" apply -f - >/dev/null
	cluster_ready "${replica_region}" "${namespace}" "${replica}"
	assert_rotation "${source_region}" "${namespace}" "${source}" "${replica_region}" "${replica}" "${replica_secret}"
}

phase_one_initial_pairs() {
	create_namespace_and_watches "e2e-bootstrap,${FIRST_NAMESPACE}"
	create_pair us db01 eu db02 "${FIRST_NAMESPACE}"
	create_pair eu db03 us db04 "${FIRST_NAMESPACE}"
}

prometheus_query() {
	local region=$1
	local query=$2
	local port
	if [[ "${region}" == us ]]; then port=19290; else port=19291; fi
	kubectl_for "${region}" -n cnpg-system port-forward service/e2e-prometheus "${port}:9090" >"${ARTIFACT_DIR}/prometheus-query-${region}.log" 2>&1 &
	local process=$!
	trap 'kill "${process}" >/dev/null 2>&1 || true' RETURN
	for _ in $(seq 1 30); do
		if curl --fail --silent --get --data-urlencode "query=${query}" "http://127.0.0.1:${port}/api/v1/query"; then
			kill "${process}" >/dev/null 2>&1 || true
			trap - RETURN
			return 0
		fi
		sleep 2
	done
	kill "${process}" >/dev/null 2>&1 || true
	trap - RETURN
	return 1
}

assert_rotation_metrics() {
	local region=$1
	local namespace=$2
	local replica=$3
	local result
	# Prometheus exposes the controller's next scrape asynchronously. A completed
	# credential rotation is the event under test, so wait for that scrape rather
	# than failing the scenario when a just-committed metric is one interval late.
	wait_until 24 "current lease metric for ${namespace}/${replica}" lease_metric_present "${region}" "${namespace}" "${replica}"
	wait_until 24 "successful rotation metric" successful_rotation_metric_present "${region}"
	# Metric exposition must not carry controller-unsafe identities. The known
	# fixture values catch accidental labels while the regex catches raw leases.
	result=$(prometheus_query "${region}" '{__name__=~"vault_replica_.*"}')
	if jq -r '.data.result[]? | @json' <<<"${result}" | rg -q "${BOOTSTRAP_PASSWORD}|${BOOTSTRAP_USER}|${MANAGEMENT_PASSWORD}|${MANAGEMENT_USER}|${VAULT_ROOT_TOKEN}|database/creds"; then
		echo "sensitive value appeared in Prometheus series" >&2
		return 1
	fi
}

lease_metric_present() {
	local region=$1
	local namespace=$2
	local replica=$3
	prometheus_query "${region}" "vault_replica_current_lease_time_to_expiration_seconds{namespace=\"${namespace}\",cluster=\"${replica}\"}" | \
		jq -e '(.data.result | length) == 1 and (.data.result[0].value[1] | tonumber > 0)' >/dev/null
}

successful_rotation_metric_present() {
	local region=$1
	prometheus_query "${region}" 'vault_replica_rotations_total{result="success"}' | \
		jq -e '.data.result | length > 0' >/dev/null
}

primary_changed() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local previous=$4
	local current
	current=$(primary_for "${region}" "${namespace}" "${cluster}")
	[[ -n "${current}" && "${current}" != "${previous}" ]]
}

standby_for() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local primary=$4
	kubectl_for "${region}" -n "${namespace}" get pods -l "cnpg.io/cluster=${cluster}" -o json | \
		jq -r --arg primary "${primary}" '.items[] | select(.metadata.name != $primary) | .metadata.name' | head -n 1
}

promote_replica_primary() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local prior
	local standby
	prior=$(primary_for "${region}" "${namespace}" "${cluster}")
	standby=$(standby_for "${region}" "${namespace}" "${cluster}" "${prior}")
	[[ -n "${standby}" ]]
	kubectl cnpg --context "$(context_for "${region}")" --namespace "${namespace}" promote "${cluster}" "${standby}" >/dev/null
	wait_until 96 "CNPG failover for ${namespace}/${cluster}" primary_changed "${region}" "${namespace}" "${cluster}" "${prior}"
}

delete_primary_pod() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local prior
	prior=$(primary_for "${region}" "${namespace}" "${cluster}")
	kubectl_for "${region}" -n "${namespace}" delete pod "${prior}" --wait=false >/dev/null
	wait_until 120 "replacement primary for ${namespace}/${cluster}" primary_changed "${region}" "${namespace}" "${cluster}" "${prior}"
}

drain_primary_node() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local primary
	local node
	primary=$(primary_for "${region}" "${namespace}" "${cluster}")
	node=$(kubectl_for "${region}" -n "${namespace}" get pod "${primary}" -o jsonpath='{.spec.nodeName}')
	[[ -n "${node}" ]]
	kubectl_for "${region}" cordon "${node}" >/dev/null
	# CNPG's disruption budget correctly denies a normal eviction of this
	# two-instance test cluster. The scenario intentionally needs a hard node
	# drain to exercise recovery, so use drain's delete path while retaining the
	# cordon/drain operation and bounded recovery assertion.
	kubectl_for "${region}" drain "${node}" --ignore-daemonsets --delete-emptydir-data --force --disable-eviction --timeout=5m >/dev/null
	wait_until 120 "drained-node replacement for ${namespace}/${cluster}" primary_not_on_node "${region}" "${namespace}" "${cluster}" "${node}"
	kubectl_for "${region}" uncordon "${node}" >/dev/null
}

primary_not_on_node() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local old_node=$4
	local primary
	primary=$(primary_for "${region}" "${namespace}" "${cluster}")
	[[ -n "${primary}" ]]
	[[ "$(kubectl_for "${region}" -n "${namespace}" get pod "${primary}" -o jsonpath='{.spec.nodeName}')" != "${old_node}" ]]
}

phase_two_failover_replacement_and_drain() {
	local prior
	prior=$(state_lease eu "${FIRST_NAMESPACE}" db02)
	promote_replica_primary eu "${FIRST_NAMESPACE}" db02
	assert_rotation us "${FIRST_NAMESPACE}" db01 eu db02 db02-db01-credentials "${prior}"

	prior=$(state_lease eu "${FIRST_NAMESPACE}" db02)
	delete_primary_pod eu "${FIRST_NAMESPACE}" db02
	assert_rotation us "${FIRST_NAMESPACE}" db01 eu db02 db02-db01-credentials "${prior}"

	prior=$(state_lease eu "${FIRST_NAMESPACE}" db02)
	drain_primary_node eu "${FIRST_NAMESPACE}" db02
	assert_rotation us "${FIRST_NAMESPACE}" db01 eu db02 db02-db01-credentials "${prior}"
	assert_rotation_metrics eu "${FIRST_NAMESPACE}" db02
}

demotion_token_exists() {
	local region=$1
	local namespace=$2
	local name=$3
	[[ -n "$(kubectl_for "${region}" -n "${namespace}" get cluster "${name}" -o jsonpath='{.status.demotionToken}')" ]]
}

phase_three_cross_region_switchover() {
	local old_db02_lease
	old_db02_lease=$(state_lease eu "${FIRST_NAMESPACE}" db02)
	# db02 already contains the replicated static management account. Configure
	# its Vault role before promotion, but do not issue against a read-only node.
	configure_vault_source eu db02
	# The fixture's gateways name individual source clusters, unlike a production
	# HA endpoint. Quiesce both controllers while CNPG changes the topology so no
	# lease operation can race the endpoint handoff to the promoted writable
	# source. The resumed controllers still perform every cleanup and rotation.
	pause_controller us
	pause_controller eu
	# Distributed Topology requires every member, including a newly promoted
	# primary, to declare its own external-cluster identity. db02 began as a
	# replica, so add this fixture-only self entry before setting it primary.
	ensure_distributed_self_external eu "${FIRST_NAMESPACE}" db02 db02-to-db01-credentials
	kubectl_for us -n "${FIRST_NAMESPACE}" patch cluster db01 --type=merge \
		-p '{"spec":{"replica":{"primary":"db02","source":"db02"}}}' >/dev/null
	wait_until 96 "demotion token for db01" demotion_token_exists us "${FIRST_NAMESPACE}" db01
	local token
	token=$(kubectl_for us -n "${FIRST_NAMESPACE}" get cluster db01 -o jsonpath='{.status.demotionToken}')
	kubectl_for eu -n "${FIRST_NAMESPACE}" patch cluster db02 --type=merge \
		-p "{\"spec\":{\"replica\":{\"primary\":\"db02\",\"source\":\"db01\",\"promotionToken\":\"${token}\"}}}" >/dev/null
	cluster_ready eu "${FIRST_NAMESPACE}" db02
	# CNPG must first re-establish db01's existing static replication
	# connection to db02. Starting the dynamic rotation before that recovery is
	# visible would consume its bounded reconnect deadline on topology work
	# rather than on the credential change under test.
	cluster_ready us "${FIRST_NAMESPACE}" db01
	wait_until 96 "static WAL receiver after cross-region switchover" wal_receiver_active us "${FIRST_NAMESPACE}" db01
	# `old_db02_lease` was issued by the database role named db01 while db01
	# was the source. Its role data is replicated to db02, so point Vault's
	# db01 connection at the promoted writable db02 before awaiting cleanup.
	configure_vault_database_endpoint db01 eu db02
	resume_controller eu
	wait_until 72 "old db02 lease cleanup" lease_is_revoked "${old_db02_lease}"
	resume_controller us
	assert_rotation eu "${FIRST_NAMESPACE}" db02 us db01 db01-to-db02-credentials
}

make_distributed_source() {
	local source_region=$1
	local namespace=$2
	local source=$3
	local replica_region=$4
	local replica=$5
	local reverse_secret="${source}-to-${replica}-credentials"
	local host
	local bootstrap_source
	local bootstrap_entries
	local external_clusters
	local patch
	host=$(control_plane_ip "${replica_region}")
	create_password_secret "${source_region}" "${namespace}" "${reverse_secret}"
	create_gateway "${replica_region}" "${namespace}" "${replica}"
	# A promoted standalone can still retain its original pg_basebackup source.
	# CNPG validates that reference on every later update, so retain its external
	# entry while replacing the distributed-topology entries for the new replica.
	bootstrap_source=$(kubectl_for "${source_region}" -n "${namespace}" get cluster "${source}" -o json | jq -r '.spec.bootstrap.pg_basebackup.source // empty')
	bootstrap_entries='[]'
	if [[ -n "${bootstrap_source}" ]]; then
		bootstrap_entries=$(kubectl_for "${source_region}" -n "${namespace}" get cluster "${source}" -o json | \
			jq -c --arg bootstrap_source "${bootstrap_source}" '[.spec.externalClusters[]? | select(.name == $bootstrap_source)]')
		if [[ "${bootstrap_entries}" == '[]' ]]; then
			echo "${namespace}/${source} is missing its pg_basebackup external cluster ${bootstrap_source}" >&2
			return 1
		fi
	fi
	external_clusters=$(jq -cn \
		--arg source "${source}" --arg replica "${replica}" --arg namespace "${namespace}" \
		--arg secret "${reverse_secret}" --arg host "${host}" --arg port "$(gateway_port "${replica}")" \
		--arg user "${BOOTSTRAP_USER}" --argjson bootstrap_entries "${bootstrap_entries}" \
		'[{name:$source, connectionParameters:{host:($source + "-rw." + $namespace + ".svc"), port:"5432", user:$user, dbname:"postgres", sslmode:"disable"}, password:{name:$secret, key:"password"}}, {name:$replica, connectionParameters:{host:$host, port:$port, user:$user, dbname:"postgres", sslmode:"disable"}, password:{name:$secret, key:"password"}}] + [$bootstrap_entries[] | select(.name != $source and .name != $replica)]')
	patch=$(jq -cn --arg source "${source}" --arg replica "${replica}" --argjson external_clusters "${external_clusters}" \
		'{spec:{replica:{primary:$source, source:$replica}, externalClusters:$external_clusters}}')
	kubectl_for "${source_region}" -n "${namespace}" patch cluster "${source}" --type=merge \
		-p "${patch}" >/dev/null
	cluster_ready "${source_region}" "${namespace}" "${source}"
}

create_replica_for_existing_source() {
	local source_region=$1
	local namespace=$2
	local source=$3
	local replica_region=$4
	local replica=$5
	local secret="${replica}-${source}-credentials"
	create_password_secret "${replica_region}" "${namespace}" "${secret}"
	assert_remote_gateway "${replica_region}" "${namespace}" "$(control_plane_ip "${source_region}")" "$(gateway_port "${source}")"
	render_replica "${namespace}" "${replica}" "${source}" "${source}" \
		"$(control_plane_ip "${source_region}")" "$(gateway_port "${source}")" "${secret}" | kubectl_for "${replica_region}" apply -f - >/dev/null
	cluster_ready "${replica_region}" "${namespace}" "${replica}"
	assert_rotation "${source_region}" "${namespace}" "${source}" "${replica_region}" "${replica}" "${secret}"
}

phase_four_promote_and_rebuild() {
	local old_db04_lease
	old_db04_lease=$(state_lease us "${FIRST_NAMESPACE}" db04)
	# A standalone promotion must make the old dynamic replication credential
	# unnecessary; the controller cleans it without touching the target Secret.
	kubectl_for us -n "${FIRST_NAMESPACE}" patch cluster db04 --type=json \
		-p '[{"op":"remove","path":"/spec/replica"}]' >/dev/null
	wait_until 120 "standalone db04" standalone_primary us "${FIRST_NAMESPACE}" db04
	wait_until 72 "db04 lease cleanup" state_entry_absent us "${FIRST_NAMESPACE}" db04
	wait_until 72 "db04 old lease revocation" lease_is_revoked "${old_db04_lease}"

	make_distributed_source eu "${FIRST_NAMESPACE}" db03 us db05
	create_replica_for_existing_source eu "${FIRST_NAMESPACE}" db03 us db05

	make_distributed_source us "${FIRST_NAMESPACE}" db04 eu db06
	onboard_source us db04
	create_replica_for_existing_source us "${FIRST_NAMESPACE}" db04 eu db06
}

standalone_primary() {
	local region=$1
	local namespace=$2
	local name=$3
	[[ "$(sql "${region}" "${namespace}" "${name}" 'SELECT pg_is_in_recovery()')" == f ]]
}

phase_five_second_namespace() {
	create_namespace_and_watches "e2e-bootstrap,${FIRST_NAMESPACE},${SECOND_NAMESPACE}"
	create_pair us db07 eu db08 "${SECOND_NAMESPACE}"
	assert_rotation_metrics eu "${SECOND_NAMESPACE}" db08
}

wal_receiver_inactive() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local pod
	pod=$(primary_for "${region}" "${namespace}" "${cluster}")
	kubectl_for "${region}" get --raw "/api/v1/namespaces/${namespace}/pods/https:${pod}:8000/proxy/pg/status" | jq -e '.isWalReceiverActive == false' >/dev/null
}

state_lease_is() {
	local region=$1
	local namespace=$2
	local cluster=$3
	local want=$4
	[[ "$(state_lease "${region}" "${namespace}" "${cluster}")" == "${want}" ]]
}

phase_six_namespace_removal_and_restore() {
	local old_lease
	local old_user
	old_lease=$(state_lease us "${FIRST_NAMESPACE}" db05)
	old_user=$(cluster_user us "${FIRST_NAMESPACE}" db05 db03)
	[[ -n "${old_lease}" ]]
	# CNPG deliberately continues watching the first namespace. Only this
	# controller is removed from scope, which must not be interpreted as delete.
	controller_only_watches "e2e-bootstrap,${SECOND_NAMESPACE}"
	kubectl_for us -n cnpg-system get configmap cnpg-controller-manager-config -o jsonpath='{.data.WATCH_NAMESPACE}' | grep -Fqx "e2e-bootstrap,${FIRST_NAMESPACE},${SECOND_NAMESPACE}"
	kubectl_for eu -n cnpg-system get configmap cnpg-controller-manager-config -o jsonpath='{.data.WATCH_NAMESPACE}' | grep -Fqx "e2e-bootstrap,${FIRST_NAMESPACE},${SECOND_NAMESPACE}"
	# A deliberately invalid value proves the controller does not use Ready as
	# an authentication signal. An established streaming connection keeps its
	# original password, so force the phase's planned failover after updating
	# the Secret. The new receiver must authenticate with the invalid value;
	# Secret events are not watched, so it remains broken while out of scope.
	kubectl_for us -n "${FIRST_NAMESPACE}" patch secret db05-db03-credentials --type=merge \
		-p '{"data":{"password":"aW52YWxpZC1lMmUtcGFzc3dvcmQ="}}' >/dev/null
	delete_primary_pod us "${FIRST_NAMESPACE}" db05
	wait_until 48 "failed WAL receiver after invalid password" wal_receiver_inactive us "${FIRST_NAMESPACE}" db05
	for _ in $(seq 1 12); do
		state_lease_is us "${FIRST_NAMESPACE}" db05 "${old_lease}"
		[[ "$(cluster_user us "${FIRST_NAMESPACE}" db05 db03)" == "${old_user}" ]]
		sleep 5
	done

	controller_only_watches "e2e-bootstrap,${FIRST_NAMESPACE},${SECOND_NAMESPACE}"
	# Cache startup observes the previously missed primary episode; a second
	# CNPG promotion supplies the supported, explicit event after re-addition.
	promote_replica_primary us "${FIRST_NAMESPACE}" db05
	assert_rotation eu "${FIRST_NAMESPACE}" db03 us db05 db05-db03-credentials "${old_lease}"
}

target_secret_exists() {
	local region=$1
	local namespace=$2
	local name=$3
	kubectl_for "${region}" -n "${namespace}" get secret "${name}" >/dev/null
}

phase_seven_deprovision_cleanup() {
	local old_lease
	old_lease=$(state_lease eu "${SECOND_NAMESPACE}" db08)
	[[ -n "${old_lease}" ]]
	# Revoke a replica's database credential while its source is still serving.
	# Deleting both Clusters at once tears down db07's gateway before Vault's
	# database plugin can execute the db08 lease revocation SQL.
	kubectl_for eu -n "${SECOND_NAMESPACE}" delete cluster db08 --wait=true --timeout=8m
	# Preserve the design's five-minute sweep and two-confirmation requirement:
	# this deliberately permits more than ten minutes rather than shortening a
	# production safety interval only for tests.
	wait_until 150 "two-sweep orphan cleanup for db08" state_entry_absent eu "${SECOND_NAMESPACE}" db08
	wait_until 48 "db08 lease revocation" lease_is_revoked "${old_lease}"
	kubectl_for us -n "${SECOND_NAMESPACE}" delete cluster db07 --wait=true --timeout=8m
	target_secret_exists eu "${SECOND_NAMESPACE}" db08-db07-credentials
	assert_rotation_metrics us "${FIRST_NAMESPACE}" db01
	wait_until 48 "removed lease metric for db08" lease_metric_absent eu "${SECOND_NAMESPACE}" db08
}

lease_metric_absent() {
	local region=$1
	local namespace=$2
	local cluster=$3
	prometheus_query "${region}" "vault_replica_current_lease_time_to_expiration_seconds{namespace=\"${namespace}\",cluster=\"${cluster}\"}" | jq -e '.data.result | length == 0' >/dev/null
}

assert_no_sensitive_artifacts() {
	local forbidden
	for forbidden in "${VAULT_ROOT_TOKEN}" "${BOOTSTRAP_PASSWORD}" "${MANAGEMENT_PASSWORD}"; do
		if rg -F --glob '!kubeconfig-*.yaml' "${forbidden}" "${ARTIFACT_DIR}" >/dev/null; then
			echo "unredacted fixture secret found in E2E artifacts" >&2
			return 1
		fi
	done
}

main() {
	# setup.sh has its own cleanup trap; retain its clusters while this parent
	# runs the live assertions, then this entrypoint deletes only the same names.
	if [[ "${E2E_SKIP_SETUP}" != true ]]; then
		KEEP_E2E_CLUSTERS=true E2E_ARTIFACT_DIR="${ARTIFACT_DIR}" bash "${E2E_DIR}/setup.sh"
	fi
	# setup.sh deliberately scopes its temporary kubeconfig to its own process.
	# The scenario runner owns the same two files for all subsequent kubectl
	# operations so it never falls back to a developer's ambient kubeconfig.
	export KUBECONFIG="${ARTIFACT_DIR}/kubeconfig-us.yaml:${ARTIFACT_DIR}/kubeconfig-eu.yaml"
	phase phase-0-environment assert_phase_zero
	phase phase-0-rbac assert_write_only_secret_rbac
	phase phase-0-monitoring assert_prometheus
	if [[ "${E2E_PHASES}" == phase-0 ]]; then
		log "requested phase-0 acceptance passed"
		return 0
	fi
	phase phase-1-initial-pairs phase_one_initial_pairs
	if [[ "${E2E_PHASES}" == phase-1 ]]; then
		log "requested phase-1 acceptance passed"
		return 0
	fi
	if [[ "${E2E_PHASES}" != all ]]; then
		echo "E2E_PHASES must be all, phase-0, or phase-1, got ${E2E_PHASES}" >&2
		return 2
	fi
	phase phase-2-failover-replacement-drain phase_two_failover_replacement_and_drain
	phase phase-3-cross-region-switchover phase_three_cross_region_switchover
	phase phase-4-promotion-and-rebuild phase_four_promote_and_rebuild
	phase phase-5-second-namespace phase_five_second_namespace
	phase phase-6-namespace-removal-and-restore phase_six_namespace_removal_and_restore
	phase phase-7-deprovision-cleanup phase_seven_deprovision_cleanup
	phase phase-8-redaction assert_no_sensitive_artifacts
	log "complete ordered E2E acceptance passed"
}

main "$@"
