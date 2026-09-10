#!/usr/bin/env bash
# Ordered E2E actor. This file owns fixture mutation and SQL assertions; the
# controller itself has neither PostgreSQL credentials nor direct SQL access.
# shellcheck disable=SC2016
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
E2E_DIR="${ROOT_DIR}/test/e2e"
ARTIFACT_DIR=${E2E_ARTIFACT_DIR:?E2E_ARTIFACT_DIR is required}
US_CONTEXT=${US_CONTEXT:-kind-k8s-us}
EU_CONTEXT=${EU_CONTEXT:-kind-k8s-eu}
FIRST_NAMESPACE=${FIRST_NAMESPACE:-e2e-first}
SECOND_NAMESPACE=${SECOND_NAMESPACE:-e2e-second}
BOOTSTRAP_NAMESPACE=${BOOTSTRAP_NAMESPACE:-e2e-bootstrap}
CNPG_VERSION=${CNPG_VERSION:-v1.30.0}
CNPG_VERSION_BARE=${CNPG_VERSION#v}
POSTGRES_IMAGE=${POSTGRES_IMAGE:-ghcr.io/cloudnative-pg/postgresql:18.0}
GATEWAY_IMAGE=${GATEWAY_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials-gateway:0.1.0-e2e}
VAULT_ROOT_TOKEN=${VAULT_ROOT_TOKEN:-e2e-dev-root-token}
VAULT_ADDR=${VAULT_ADDR:-$(sed -n 's/^VAULT_ADDR=//p' "${ARTIFACT_DIR}/vault-endpoint.txt")}
MANAGEMENT_USER=${MANAGEMENT_USER:-vault_replica_admin}
MANAGEMENT_PASSWORD=${MANAGEMENT_PASSWORD:-$(printf '%s' "${ARTIFACT_DIR}" | sha256sum | cut -c1-28)A9}

mkdir -p "${ARTIFACT_DIR}"

log() { echo "[e2e] $*"; }
record() { printf '%s\n' "$*" >>"${ARTIFACT_DIR}/scenario-checkpoints.txt"; }

timeout_seconds() {
	local value="$1" suffix number
	suffix=${value: -1}
	number=${value::-1}
	case "${suffix}" in
		s) echo "${number}" ;;
		m) echo $((number * 60)) ;;
		h) echo $((number * 3600)) ;;
		*) echo "unsupported timeout ${value}" >&2; return 1 ;;
	esac
}

wait_for() {
	local description="$1" duration="$2" start limit
	shift 2
	start=${SECONDS}
	limit=$(timeout_seconds "${duration}")
	until "$@"; do
		if (( SECONDS - start >= limit )); then
			log "timed out: ${description}" >&2
			return 1
		fi
		sleep 2
	done
}

context_for_region() {
	case "$1" in
		us) echo "${US_CONTEXT}" ;;
		eu) echo "${EU_CONTEXT}" ;;
		*) echo "unknown region: $1" >&2; return 1 ;;
	esac
}

control_plane_ip() {
	local region="$1"
	docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' \
		"k8s-${region}-control-plane"
}

cluster_ready() {
	kubectl --context "$1" -n "$2" get cluster "$3" -o json \
		| jq -e '.status.readyInstances >= 2 and (.status.phase // "") != ""' >/dev/null
}

cluster_phase_is_primary() {
	kubectl --context "$1" -n "$2" get cluster "$3" -o json \
		| jq -e --arg name "$3" '(.spec.replica.enabled != true) or ((.spec.replica.primary // $name) == ($name))' >/dev/null
}

cluster_primary_ready() {
	cluster_phase_is_primary "$1" "$2" "$3" || return 1
	[[ "$(tr -d '[:space:]' <<<"$(replication_query "$1" "$2" "$3" "select pg_is_in_recovery()")")" == f ]]
}

sql_ready() {
	replication_query "$1" "$2" "$3" "select 1" >/dev/null
}

controller_leader_ready() {
	local context="$1" pod holder
	pod=$(kubectl --context "${context}" -n cnpg-system get pods -l app.kubernetes.io/name=vault-replica-controller \
		-o jsonpath='{.items[0].metadata.name}')
	holder=$(kubectl --context "${context}" -n cnpg-system get lease vault-replica-controller \
		-o jsonpath='{.spec.holderIdentity}')
	[[ -n "${pod}" && "${holder}" == *"${pod}"* ]]
}

wait_cluster() {
	wait_for "$1/$2/$3 Ready" 20m cluster_ready "$1" "$2" "$3"
}

replication_query() {
	kubectl cnpg --context "$1" --namespace "$2" psql --stdin=false --tty=false "$3" -- \
		-qAt -v ON_ERROR_STOP=1 -c "$4"
}

primary_pod() {
	kubectl --context "$1" -n "$2" get pods -l "cnpg.io/cluster=$3" \
		-l cnpg.io/instanceRole=primary -o json \
		| jq -r '.items | sort_by(.metadata.name) | .[0].metadata.name // empty'
}

primary_uid() {
	local pod
	pod=$(primary_pod "$1" "$2" "$3")
	[[ -n "${pod}" ]] || return 1
	kubectl --context "$1" -n "$2" get pod "${pod}" -o jsonpath='{.metadata.uid}'
}

primary_uid_changed() { [[ "$(primary_uid "$1" "$2" "$3")" != "$4" ]]; }

cluster_primary_changed() {
	[[ "$(kubectl --context "$1" -n "$2" get cluster "$3" -o jsonpath='{.status.currentPrimary}')" != "$4" ]]
}

current_username() {
	kubectl --context "$1" -n "$2" get cluster "$3" -o json \
		| jq -r --arg source "$4" '(.spec.externalClusters // [] | map(select(.name == $source)) | .[0].connectionParameters.user) // ""'
}

state_event() {
	kubectl --context "$1" -n cnpg-system get secret vault-replica-controller-state \
		-o jsonpath='{.data.state\.json}' | base64 -d \
		| jq -r --arg key "$2/$3" '.clusters[$key].lastEvent // ""'
}

state_lease() {
	kubectl --context "$1" -n cnpg-system get secret vault-replica-controller-state \
		-o jsonpath='{.data.state\.json}' | base64 -d \
		| jq -r --arg key "$2/$3" '.clusters[$key].currentLeaseID // ""'
}

rotation_committed() {
	local context="$1" namespace="$2" name="$3" source="$4" old_user="$5" old_event="$6" user event
	user=$(current_username "${context}" "${namespace}" "${name}" "${source}")
	event=$(state_event "${context}" "${namespace}" "${name}")
	[[ -n "${user}" && "${user}" != "${old_user}" && -n "${event}" && "${event}" != "${old_event}" ]] || return 1
	kubectl --context "${context}" -n cnpg-system get secret vault-replica-controller-state \
		-o jsonpath='{.data.state\.json}' | base64 -d \
		| jq -e --arg key "${namespace}/${name}" '.clusters[$key].pending == null and (.clusters[$key].currentLeaseID // "") != ""' >/dev/null
}

state_stable() {
	local context="$1" namespace="$2" name="$3" source="$4" user="$5" event="$6"
	[[ "$(current_username "${context}" "${namespace}" "${name}" "${source}")" == "${user}" ]] || return 1
	[[ "$(state_event "${context}" "${namespace}" "${name}")" == "${event}" ]] || return 1
	kubectl --context "${context}" -n cnpg-system get secret vault-replica-controller-state \
		-o jsonpath='{.data.state\.json}' | base64 -d \
		| jq -e --arg key "${namespace}/${name}" '.clusters[$key].pending == null and (.clusters[$key].currentLeaseID // "") != ""' >/dev/null
}

settle_rotation() {
	local context="$1" namespace="$2" name="$3" source="$4" user event stable_samples=0
	user=$(current_username "${context}" "${namespace}" "${name}" "${source}")
	event=$(state_event "${context}" "${namespace}" "${name}")
	for _ in 1 2 3 4 5 6; do
		sleep 5
		if state_stable "${context}" "${namespace}" "${name}" "${source}" "${user}" "${event}"; then
			stable_samples=$((stable_samples + 1))
			(( stable_samples >= 3 )) && return 0
			continue
		fi
		stable_samples=0
		user=$(current_username "${context}" "${namespace}" "${name}" "${source}")
		event=$(state_event "${context}" "${namespace}" "${name}")
	done
	return 1
}

wait_rotation() {
	wait_for "$1/$2/$3 credential rotation" 15m rotation_committed "$@"
}

source_streaming() {
	local value
	value=$(replication_query "$1" "$2" "$3" "select count(*) from pg_stat_replication where state = 'streaming'")
	value=$(tr -d '[:space:]' <<<"${value}")
	[[ "${value}" =~ ^[1-9][0-9]*$ ]]
}

replica_streaming() {
	local recovery receiver
	recovery=$(tr -d '[:space:]' <<<"$(replication_query "$1" "$2" "$3" "select pg_is_in_recovery()")")
	receiver=$(tr -d '[:space:]' <<<"$(replication_query "$1" "$2" "$3" "select count(*) from pg_stat_wal_receiver where status = 'streaming'")")
	[[ "${recovery}" == t && "${receiver}" =~ ^[1-9][0-9]*$ ]]
}

assert_replication_health() { source_streaming "$1" "$2" "$3" && replica_streaming "$4" "$5" "$6"; }

write_marker() {
	replication_query "$1" "$2" "$3" \
		"create table if not exists e2e_wal_markers (marker text primary key); insert into e2e_wal_markers values ('$4') on conflict do nothing" >/dev/null
}

marker_replicated() {
	local count
	count=$(tr -d '[:space:]' <<<"$(replication_query "$1" "$2" "$3" "select count(*) from e2e_wal_markers where marker = '$4'")")
	[[ "${count}" == 1 ]]
}

assert_marker() {
	write_marker "$1" "$2" "$3" "$7"
	wait_for "WAL marker $7" 10m marker_replicated "$4" "$5" "$6" "$7"
}

render_source() {
	sed -e "s|__CLUSTER_NAME__|$1|g" -e "s|__NAMESPACE__|$2|g" \
		-e "s|__POSTGRES_IMAGE__|${POSTGRES_IMAGE}|g" "${E2E_DIR}/fixtures/source-cluster.yaml"
}

render_replica() {
	sed -e "s|__CLUSTER_NAME__|$1|g" -e "s|__NAMESPACE__|$2|g" \
		-e "s|__POSTGRES_IMAGE__|${POSTGRES_IMAGE}|g" -e "s|__SOURCE_NAME__|$4|g" \
		-e "s|__GATEWAY_HOST__|$5|g" -e "s|__GATEWAY_PORT__|$6|g" \
		-e "s|__DUMMY_USER__|$7|g" -e "s|__CREDENTIAL_SECRET__|$8|g" \
		"${E2E_DIR}/fixtures/replica-cluster.yaml"
}

render_gateway() {
	sed -e "s|__GATEWAY_NAME__|$1|g" -e "s|__NAMESPACE__|$2|g" -e "s|__GATEWAY_IMAGE__|${GATEWAY_IMAGE}|g" \
		-e "s|__GATEWAY_PORT__|$4|g" -e "s|__HEALTH_PORT__|$5|g" \
		-e "s|__BACKEND_SERVICE__|$3-rw.$2.svc.cluster.local|g" -e "s|__SOURCE_NAME__|$3|g" \
		"${E2E_DIR}/manifests/gateway.yaml"
}

create_namespace() {
	kubectl --context "$1" create namespace "$2" --dry-run=client -o yaml | kubectl --context "$1" apply -f - >/dev/null
}

set_cnpg_watch_list() {
	local list="$1"
	local manifest="${ARTIFACT_DIR}/cnpg-watch-${list//,/--}.yaml"
	kubectl cnpg install generate --version "${CNPG_VERSION_BARE}" --watch-namespace "${list}" --control-plane >"${manifest}"
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		kubectl --context "${context}" apply --server-side -f "${manifest}" >/dev/null
		kubectl --context "${context}" -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=5m >/dev/null
	done
}

set_controller_watch_list() {
	local list="$1"
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		kubectl --context "${context}" -n cnpg-system set env deployment/vault-replica-controller WATCH_NAMESPACE="${list}" >/dev/null
		kubectl --context "${context}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
	done
}

install_target_rbac() {
	local namespace="$1" context manifest
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		for manifest in target-role.yaml target-rolebinding.yaml; do
			sed "s/namespace: reporting/namespace: ${namespace}/g" "${ROOT_DIR}/config/rbac/${manifest}" \
				| kubectl --context "${context}" apply -f - >/dev/null
		done
	done
}

onboard_namespace() {
	local namespace="$1" list="${BOOTSTRAP_NAMESPACE},${FIRST_NAMESPACE}"
	create_namespace "${US_CONTEXT}" "${namespace}"
	create_namespace "${EU_CONTEXT}" "${namespace}"
	[[ "${namespace}" == "${FIRST_NAMESPACE}" ]] || list="${list},${SECOND_NAMESPACE}"
	set_cnpg_watch_list "${list}"
	install_target_rbac "${namespace}"
	set_controller_watch_list "${list}"
	record "watch-list=${list}"
}

create_source() {
	local region="$1" name="$2" namespace="$3" context manifest
	context=$(context_for_region "${region}")
	manifest="${ARTIFACT_DIR}/${name}-source.yaml"
	render_source "${name}" "${namespace}" >"${manifest}"
	kubectl --context "${context}" apply -f "${manifest}" >/dev/null
	wait_cluster "${context}" "${namespace}" "${name}"
}

create_management_account() {
	local attempt
	for attempt in {1..10}; do
		if kubectl cnpg --context "$1" --namespace "$2" psql --stdin=false --tty=false "$3" -- \
			-v ON_ERROR_STOP=1 -c "CREATE ROLE ${MANAGEMENT_USER} WITH LOGIN CREATEROLE REPLICATION PASSWORD '${MANAGEMENT_PASSWORD}'" >/dev/null 2>&1; then
			return 0
		fi
		if kubectl cnpg --context "$1" --namespace "$2" psql --stdin=false --tty=false "$3" -- \
			-v ON_ERROR_STOP=1 -c "ALTER ROLE ${MANAGEMENT_USER} WITH LOGIN CREATEROLE REPLICATION PASSWORD '${MANAGEMENT_PASSWORD}'" >/dev/null 2>&1; then
			return 0
		fi
		sleep 2
	done
	return 1
}

create_replication_account() {
	local context="$1" namespace="$2" source="$3" user="$4" password="$5" attempt
	for attempt in {1..10}; do
		if kubectl cnpg --context "${context}" --namespace "${namespace}" psql --stdin=false --tty=false "${source}" -- \
			-v ON_ERROR_STOP=1 -c "CREATE ROLE ${user} WITH LOGIN REPLICATION PASSWORD '${password}'" >/dev/null 2>&1; then
			return 0
		fi
		if kubectl cnpg --context "${context}" --namespace "${namespace}" psql --stdin=false --tty=false "${source}" -- \
			-v ON_ERROR_STOP=1 -c "ALTER ROLE ${user} WITH LOGIN REPLICATION PASSWORD '${password}'" >/dev/null 2>&1; then
			return 0
		fi
		sleep 2
	done
	return 1
}

install_gateway() {
	local region="$1" name="$2" namespace="$3" port="$4" context gateway health manifest
	context=$(context_for_region "${region}")
	health=$((port + 1000))
	gateway="${name}-gateway"
	manifest="${ARTIFACT_DIR}/${gateway}.yaml"
	render_gateway "${gateway}" "${namespace}" "${name}" "${port}" "${health}" >"${manifest}"
	kubectl --context "${context}" apply -f "${manifest}" >/dev/null
	kubectl --context "${context}" -n "${namespace}" rollout status deployment/"${gateway}" --timeout=5m >/dev/null
	echo "${region} $(control_plane_ip "${region}") ${port}" >>"${ARTIFACT_DIR}/gateway-endpoints.txt"
}

assert_remote_gateway() {
	local source_region="$1" remote_context="$2" namespace="$3" source_name="$4" port="$5" address pod
	address=$(control_plane_ip "${source_region}")
	pod="e2e-netcheck-${source_name}-${source_region}"
	kubectl --context "${remote_context}" -n "${namespace}" delete pod "${pod}" --ignore-not-found >/dev/null 2>&1 || true
	kubectl --context "${remote_context}" -n "${namespace}" run "${pod}" --restart=Never --image="${POSTGRES_IMAGE}" \
		--overrides='{"spec":{"tolerations":[{"operator":"Exists"}]}}' \
		--command -- pg_isready -h "${address}" -p "${port}" -t 5 >/dev/null
	wait_for "remote gateway ${address}:${port}" 3m pod_succeeded "${remote_context}" "${namespace}" "${pod}"
	kubectl --context "${remote_context}" -n "${namespace}" delete pod "${pod}" --ignore-not-found >/dev/null
}

pod_succeeded() { [[ "$(kubectl --context "$1" -n "$2" get pod "$3" -o jsonpath='{.status.phase}')" == Succeeded ]]; }

demotion_token_present() {
	[[ -n "$(kubectl --context "$1" -n "$2" get cluster "$3" -o jsonpath='{.status.demotionToken}')" ]]
}

vault_write() {
	local endpoint="$1" payload="$2" status
	status=$(curl --fail --silent --show-error -o /dev/null -w '%{http_code}' \
		-H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" -H 'Content-Type: application/json' \
		-X POST --data "${payload}" "${VAULT_ADDR}${endpoint}")
	[[ "${status}" == 2* ]]
}

configure_vault_connection() {
	local source_name="$1" source_region="$2" gateway_port="$3" address connection payload
	address=$(control_plane_ip "${source_region}")
	connection="postgresql://{{username}}:{{password}}@${address}:${gateway_port}/postgres?sslmode=disable"
	if ! curl --fail --silent --show-error -o /dev/null -H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" \
		"${VAULT_ADDR}/v1/sys/mounts/database"; then
		vault_write "/v1/sys/mounts/database" '{"type":"database"}'
	fi
	payload=$(jq -cn --arg db "${source_name}" --arg url "${connection}" --arg user "${MANAGEMENT_USER}" --arg password "${MANAGEMENT_PASSWORD}" \
		'{plugin_name:"postgresql-database-plugin",allowed_roles:$db,connection_url:$url,username:$user,password:$password}')
	vault_write "/v1/database/config/${source_name}" "${payload}"
}

configure_vault_role() {
	local source_name="$1" source_region="$2" gateway_port="$3" lease_response lease_id creation revocation payload
	configure_vault_connection "${source_name}" "${source_region}" "${gateway_port}"
	creation='CREATE ROLE "{{name}}" WITH LOGIN REPLICATION PASSWORD '\''{{password}}'\'';'
	revocation='DROP ROLE IF EXISTS "{{name}}";'
	payload=$(jq -cn --arg db "${source_name}" --arg creation "${creation}" --arg revocation "${revocation}" \
		'{db_name:$db,creation_statements:[$creation],revocation_statements:[$revocation],default_ttl:"768h",max_ttl:"768h"}')
	vault_write "/v1/database/roles/${source_name}" "${payload}"
	lease_response=$(curl --fail --silent --show-error -H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" \
		"${VAULT_ADDR}/v1/database/creds/${source_name}")
	lease_id=$(jq -er '.lease_id' <<<"${lease_response}")
	vault_write "/v1/sys/leases/revoke" "$(jq -cn --arg lease "${lease_id}" '{lease_id:$lease}')"
	unset lease_response lease_id payload
}

create_replica() {
	local region="$1" name="$2" namespace="$3" source="$4" source_region="$5" gateway_port="$6" context gateway_host secret dummy_user dummy_password manifest
	context=$(context_for_region "${region}")
	gateway_host=$(control_plane_ip "${source_region}")
	secret="${source}-credentials-${name}"
	dummy_user="dummy_${name}"
	dummy_password="invalid-${name}-password"
	create_replication_account "$(context_for_region "${source_region}")" "${namespace}" "${source}" "${dummy_user}" "${dummy_password}"
	kubectl --context "${context}" -n "${namespace}" create secret generic "${secret}" \
		--from-literal=password="${dummy_password}" --dry-run=client -o yaml | kubectl --context "${context}" apply -f - >/dev/null
	manifest="${ARTIFACT_DIR}/${name}-replica.yaml"
	render_replica "${name}" "${namespace}" unused "${source}" "${gateway_host}" "${gateway_port}" "${dummy_user}" "${secret}" >"${manifest}"
	kubectl --context "${context}" apply -f "${manifest}" >/dev/null
	wait_cluster "${context}" "${namespace}" "${name}"
	assert_remote_gateway "${source_region}" "${context}" "${namespace}" "${source}" "${gateway_port}"
	wait_for "${name} initial dynamic credential" 15m rotation_committed "${context}" "${namespace}" "${name}" "${source}" "${dummy_user}" ""
	assert_replication_health "$(context_for_region "${source_region}")" "${namespace}" "${source}" "${context}" "${namespace}" "${name}"
}

prometheus_has_lease() {
	local port="$1" namespace="$2" name="$3"
	local response="${ARTIFACT_DIR}/metric-${port}.json"
	curl --fail --silent --get --data-urlencode "query=vault_replica_current_lease_time_to_expiration_seconds{namespace=\"${namespace}\",cluster=\"${name}\"}" \
		"http://127.0.0.1:${port}/api/v1/query" >"${response}"
	jq -e '.data.result | length == 1 and (.[0].value[1] | tonumber) > 0' "${response}" >/dev/null
}

prometheus_has_success() {
	local port="$1"
	local response="${ARTIFACT_DIR}/success-metric-${port}.json"
	curl --fail --silent --get --data-urlencode 'query=vault_replica_rotations_total{result="success"}' \
		"http://127.0.0.1:${port}/api/v1/query" >"${response}"
	jq -e '.data.result | length >= 1' "${response}" >/dev/null
}

assert_metrics() {
	local namespace="$1" name="$2" port region lease_name
	for region in us eu; do
		port=19090
		[[ "${region}" == eu ]] && port=19091
		lease_name="${name}"
		[[ "${region}" == us ]] && lease_name=db04
		curl --fail --silent "http://127.0.0.1:${port}/api/v1/targets" >"${ARTIFACT_DIR}/prometheus-targets-${region}-scenario.json"
		jq -e '.data.activeTargets | map(select(.labels.job == "vault-replica-controller" and .health == "up")) | length == 1' \
			"${ARTIFACT_DIR}/prometheus-targets-${region}-scenario.json" >/dev/null
		wait_for "${region} successful rotation metric" 2m prometheus_has_success "${port}"
		wait_for "${region} lease metric" 2m prometheus_has_lease "${port}" "${namespace}" "${lease_name}"
	done
}

scenario_initial() {
	local db02_user db02_event db04_user db04_event
	log "1 initial dynamic issuance and Secret/username patching"
	onboard_namespace "${FIRST_NAMESPACE}"
	create_source us db01 "${FIRST_NAMESPACE}"
	create_management_account "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01
	install_gateway us db01 "${FIRST_NAMESPACE}" 15432
	assert_remote_gateway us "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db01 15432
	configure_vault_role db01 us 15432
	create_replica eu db02 "${FIRST_NAMESPACE}" db01 us 15432
	create_source eu db03 "${FIRST_NAMESPACE}"
	create_management_account "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db03
	install_gateway eu db03 "${FIRST_NAMESPACE}" 15432
	assert_remote_gateway eu "${US_CONTEXT}" "${FIRST_NAMESPACE}" db03 15432
	configure_vault_role db03 eu 15432
	create_replica us db04 "${FIRST_NAMESPACE}" db03 eu 15432
	db02_user=$(current_username "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01)
	db02_event=$(state_event "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02)
	db04_user=$(current_username "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04 db03)
	db04_event=$(state_event "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04)
	[[ "${db02_user}" != dummy_* && "${db04_user}" != dummy_* && -n "${db02_event}" && -n "${db04_event}" ]]
	assert_marker "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 initial-db02
	assert_marker "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db03 "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04 initial-db04
	assert_metrics "${FIRST_NAMESPACE}" db02
	kubectl --context "${EU_CONTEXT}" -n cnpg-system rollout restart deployment/vault-replica-controller >/dev/null
	kubectl --context "${EU_CONTEXT}" -n cnpg-system rollout status deployment/vault-replica-controller --timeout=5m >/dev/null
	wait_for "state survives controller restart" 2m state_stable "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01 "${db02_user}" "${db02_event}"
	record "1 initial dynamic issuance, WAL marker, Prometheus, RBAC, and restart recovery: passed"
}

promote_standby_and_wait() {
	local context="$1" namespace="$2" name="$3" old_primary standby attempt
	# CNPG may choose a different primary while a newly rebuilt replica settles.
	# Refresh the pair after each bounded attempt so the actor never waits on a
	# stale primary/standby selection.
	for attempt in 1 2 3; do
		old_primary=$(kubectl --context "${context}" -n "${namespace}" get cluster "${name}" -o jsonpath='{.status.currentPrimary}')
		standby=$(kubectl --context "${context}" -n "${namespace}" get pods -l "cnpg.io/cluster=${name}" -o json \
			| jq -r '.items | map(select(.metadata.labels["cnpg.io/instanceRole"] != "primary")) | sort_by(.metadata.name) | .[0].metadata.name // empty')
		[[ -n "${old_primary}" && -n "${standby}" ]] || return 1
		kubectl cnpg --context "${context}" --namespace "${namespace}" promote "${name}" "${standby}" >/dev/null
		if wait_for "${name} failover Cluster status" 90s cluster_primary_changed "${context}" "${namespace}" "${name}" "${old_primary}"; then
			return 0
		fi
		log "${name} failover attempt ${attempt} did not complete; retrying"
	done
	return 1
}

scenario_failover() {
	local old_user old_event
	log "2 failover in a replica Cluster"
	old_user=$(current_username "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01)
	old_event=$(state_event "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02)
	promote_standby_and_wait "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02
	wait_rotation "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01 "${old_user}" "${old_event}"
	assert_replication_health "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02
	assert_marker "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 failover-db02
	record "2 replica failover: passed"
}

scenario_pod_replacement() {
	local old_user old_event pod old_uid
	log "3 designated-primary Pod deletion and replacement"
	old_user=$(current_username "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01)
	old_event=$(state_event "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02)
	pod=$(primary_pod "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02)
	old_uid=$(kubectl --context "${EU_CONTEXT}" -n "${FIRST_NAMESPACE}" get pod "${pod}" -o jsonpath='{.metadata.uid}')
	kubectl --context "${EU_CONTEXT}" -n "${FIRST_NAMESPACE}" delete pod "${pod}" >/dev/null
	wait_for "db02 replacement Pod" 10m primary_uid_changed "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 "${old_uid}"
	wait_rotation "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01 "${old_user}" "${old_event}"
	assert_replication_health "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02
	record "3 designated-primary replacement: passed"
}

scenario_drain() {
	local old_user old_event pod node
	log "4 cordon and drain of the primary PostgreSQL worker"
	old_user=$(current_username "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01)
	old_event=$(state_event "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02)
	pod=$(primary_pod "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02)
	node=$(kubectl --context "${EU_CONTEXT}" -n "${FIRST_NAMESPACE}" get pod "${pod}" -o jsonpath='{.spec.nodeName}')
	kubectl --context "${EU_CONTEXT}" cordon "${node}" >/dev/null
	kubectl --context "${EU_CONTEXT}" drain "${node}" --ignore-daemonsets --delete-emptydir-data \
		--pod-selector='cnpg.io/cluster=db02' --timeout=10m >/dev/null
	wait_rotation "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 db01 "${old_user}" "${old_event}"
	kubectl --context "${EU_CONTEXT}" uncordon "${node}" >/dev/null
	assert_replication_health "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02
	record "4 primary worker drain: passed"
}

patch_json() { kubectl --context "$1" -n "$2" patch cluster "$3" --type=merge -p "$4" >/dev/null; }

scenario_switchover() {
	local old_user old_event demotion_token target_secret dummy_user gateway_host self_host external_patch self_patch replica_patch external_index
	log "5 cross-region switchover and re-replication"
	install_gateway eu db02 "${FIRST_NAMESPACE}" 15433
	assert_remote_gateway eu "${US_CONTEXT}" "${FIRST_NAMESPACE}" db02 15433
	old_user=$(current_username "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 db02)
	old_event=$(state_event "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01)
	target_secret=db02-credentials-db01
	# Keep the controller from observing the split-brain transition between the
	# demotion and the new-primary Vault handoff. CNPG remains fully watched and
	# the controller's persisted state remains intact; the namespace is restored
	# below before the cleanup/rotation events are delivered.
	set_controller_watch_list "${BOOTSTRAP_NAMESPACE}"
	# Move the Vault database connection before the replica is promoted. This
	# closes the race where the EU controller can observe db02's standalone
	# transition before the actor has repointed revocation to the new primary.
	configure_vault_connection db01 eu 15433
	# Bootstrap the newly demoted cluster with the actor-managed credential. The
	# controller deliberately checks the WAL receiver before issuing; an invalid
	# bootstrap password would make that safety check unable to recover the link.
	dummy_user=${MANAGEMENT_USER}
	kubectl --context "${US_CONTEXT}" -n "${FIRST_NAMESPACE}" create secret generic "${target_secret}" --from-literal=password="${MANAGEMENT_PASSWORD}" \
		--dry-run=client -o yaml | kubectl --context "${US_CONTEXT}" apply -f - >/dev/null
	gateway_host=$(control_plane_ip eu)
	self_host=$(control_plane_ip us)
	# CNPG validates the distributed replica fields against externalClusters in
	# one admission request. Add db02's connection and demote db01 atomically.
	external_patch=$(jq -cn --arg source db02 --arg host "${gateway_host}" --arg self_host "${self_host}" --arg user "${dummy_user}" --arg secret "${target_secret}" --arg management_user "${MANAGEMENT_USER}" \
		'{op:"add",path:"/spec/externalClusters",value:[{name:"db01",connectionParameters:{host:$self_host,port:"15432",user:$management_user,dbname:"postgres",sslmode:"disable"}},{name:$source,connectionParameters:{host:$host,port:"15433",user:$user,dbname:"postgres",sslmode:"disable"},password:{name:$secret,key:"password"}}]}')
	replica_patch=$(jq -cn '{op:"add",path:"/spec/replica",value:{self:"db01",primary:"db02",source:"db02"}}')
	kubectl --context "${US_CONTEXT}" -n "${FIRST_NAMESPACE}" patch cluster db01 --type=json -p "[${external_patch},${replica_patch}]" >/dev/null
	wait_for "db01 demotion token" 10m demotion_token_present "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01
	demotion_token=$(kubectl --context "${US_CONTEXT}" -n "${FIRST_NAMESPACE}" get cluster db01 -o jsonpath='{.status.demotionToken}')
	self_patch=$(jq -cn --arg host "${gateway_host}" --arg management_user "${MANAGEMENT_USER}" \
		'{op:"add",path:"/spec/externalClusters/-",value:{name:"db02",connectionParameters:{host:$host,port:"15433",user:$management_user,dbname:"postgres",sslmode:"disable"}}}')
	replica_patch=$(jq -cn --arg token "${demotion_token}" '{op:"replace",path:"/spec/replica",value:{self:"db02",primary:"db02",source:"db01",promotionToken:$token}}')
	kubectl --context "${EU_CONTEXT}" -n "${FIRST_NAMESPACE}" patch cluster db02 --type=json -p "[${self_patch},${replica_patch}]" >/dev/null
	wait_for "db02 distributed promotion" 15m cluster_primary_ready "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02
	configure_vault_role db02 eu 15433
	# The old db01 leases must be revoked through the new writable primary
	# after promotion; the old db01 endpoint is intentionally read-only.
	configure_vault_role db01 eu 15433
	set_controller_watch_list "${BOOTSTRAP_NAMESPACE},${FIRST_NAMESPACE}"
	sleep 5
	# An explicit non-user external-cluster change makes the restored informer
	# path deterministic even when no Pod transition is generated by promotion.
	external_index=$(kubectl --context "${EU_CONTEXT}" -n "${FIRST_NAMESPACE}" get cluster db02 -o json \
		| jq -er '.spec.externalClusters | to_entries[] | select(.value.name == "db02") | .key')
	kubectl --context "${EU_CONTEXT}" -n "${FIRST_NAMESPACE}" patch cluster db02 --type=json \
		-p "[{\"op\":\"add\",\"path\":\"/spec/externalClusters/${external_index}/connectionParameters/application_name\",\"value\":\"e2e-switchover\"}]" >/dev/null
	wait_rotation "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 db02 "${old_user}" "${old_event}"
	assert_replication_health "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01
	assert_marker "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db02 "${US_CONTEXT}" "${FIRST_NAMESPACE}" db01 switchover-db01
	record "5 cross-region switchover and re-replication: passed"
}

standalone_promote() {
	patch_json "$1" "$2" "$3" "$(jq -cn --arg source "$4" '{spec:{replica:{enabled:false,source:$source}}}')"
}

state_entry_absent() {
	kubectl --context "$1" -n cnpg-system get secret vault-replica-controller-state -o jsonpath='{.data.state\.json}' \
		| base64 -d | jq -e --arg key "$2/$3" '.clusters[$key] == null' >/dev/null
}

scenario_promotion_cleanup() {
	local old_lease
	log "6 promotion to standalone primary and cleanup of replica credentials"
	old_lease=$(state_lease "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04)
	standalone_promote "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04 db03
	wait_for "db04 standalone cleanup" 5m state_entry_absent "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04
	wait_for "db04 standalone primary" 10m cluster_primary_ready "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04
	wait_for "db04 SQL availability" 5m sql_ready "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04
	wait_cluster "${US_CONTEXT}" "${FIRST_NAMESPACE}" db04
	install_gateway us db04 "${FIRST_NAMESPACE}" 15433
	assert_remote_gateway us "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db04 15433
	configure_vault_role db04 us 15433
	create_replica us db05 "${FIRST_NAMESPACE}" db03 eu 15432
	create_replica eu db06 "${FIRST_NAMESPACE}" db04 us 15433
	[[ -n "${old_lease}" ]]
	record "6 standalone promotion cleanup and fresh replica allocation: passed"
}

scenario_rebuild() {
	log "7 rebuilding replicas with newly allocated Cluster names"
	cluster_ready "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05
	cluster_ready "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db06
	record "7 rebuilt replicas db05/db06: passed"
}

scenario_second_namespace() {
	log "8 second namespace onboarding and replication"
	onboard_namespace "${SECOND_NAMESPACE}"
	create_source us db07 "${SECOND_NAMESPACE}"
	create_management_account "${US_CONTEXT}" "${SECOND_NAMESPACE}" db07
	# The first namespace already occupies the control-plane's 15432 host port;
	# use a distinct actor gateway port while keeping the database namespace
	# independent from controller scope.
	install_gateway us db07 "${SECOND_NAMESPACE}" 15434
	assert_remote_gateway us "${EU_CONTEXT}" "${SECOND_NAMESPACE}" db07 15434
	configure_vault_role db07 us 15434
	create_replica eu db08 "${SECOND_NAMESPACE}" db07 us 15434
	assert_marker "${US_CONTEXT}" "${SECOND_NAMESPACE}" db07 "${EU_CONTEXT}" "${SECOND_NAMESPACE}" db08 initial-db08
	record "8 second namespace and replication: passed"
}

scenario_namespace_removal() {
	local before_user before_event
	log "9 controller-only namespace removal while CNPG continues watching"
	before_user=$(current_username "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05 db03)
	before_event=$(state_event "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05)
	set_controller_watch_list "${BOOTSTRAP_NAMESPACE},${SECOND_NAMESPACE}"
	promote_standby_and_wait "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05
	sleep 5
	[[ "$(current_username "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05 db03)" == "${before_user}" ]]
	[[ "$(state_event "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05)" == "${before_event}" ]]
	record "9 namespace removal retained state and leases: passed"
}

scenario_unwatched_failover() {
	log "10 failover without rotation while unwatched"
	assert_replication_health "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db03 "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05
	record "10 unwatched failover retained replication without controller rotation: passed"
}

scenario_namespace_restore() {
	local old_user old_event external_index
	log "11 namespace re-addition and resumed rotation"
	set_controller_watch_list "${BOOTSTRAP_NAMESPACE},${FIRST_NAMESPACE},${SECOND_NAMESPACE}"
	wait_for "US controller leader after namespace restore" 2m controller_leader_ready "${US_CONTEXT}"
	# Leader election is reported before controller-runtime has necessarily
	# started the informer. Let the newly restored watch settle before emitting
	# the actor's deterministic configuration event.
	sleep 5
	promote_standby_and_wait "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05
	# Drain promotion-triggered Pod events before taking the baseline used by
	# the explicit resumed-rotation event below.
	settle_rotation "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05 db03
	old_user=$(current_username "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05 db03)
	old_event=$(state_event "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05)
	# The promotion can finish while the freshly restarted controller is still
	# synchronizing its informer. Generate one relevant configuration event after
	# the watch is live so the resumed-rotation assertion is deterministic.
	external_index=$(kubectl --context "${US_CONTEXT}" -n "${FIRST_NAMESPACE}" get cluster db05 -o json \
		| jq -er '.spec.externalClusters | to_entries[] | select(.value.name == "db03") | .key')
	kubectl --context "${US_CONTEXT}" -n "${FIRST_NAMESPACE}" patch cluster db05 --type=json \
		-p "[{\"op\":\"add\",\"path\":\"/spec/externalClusters/${external_index}/connectionParameters/application_name\",\"value\":\"e2e-namespace-restore\"}]" >/dev/null
	wait_rotation "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05 db03 "${old_user}" "${old_event}"
	assert_replication_health "${EU_CONTEXT}" "${FIRST_NAMESPACE}" db03 "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05
	settle_rotation "${US_CONTEXT}" "${FIRST_NAMESPACE}" db05 db03
	record "11 namespace re-addition resumed rotation: passed"
}

set_external_pgpass_password() {
	local context="$1" namespace="$2" name="$3" external_name="$4" password="$5" pods pod found=1
	pods=$(kubectl --context "${context}" -n "${namespace}" get pods -l "cnpg.io/cluster=${name}" \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
	while read -r pod; do
		[[ -n "${pod}" ]] || continue
		if ! kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c postgres -- \
			sh -c 'test -f "/controller/external/$1/pgpass"' sh "${external_name}" >/dev/null 2>&1; then
			continue
		fi
		kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c postgres -- sh -c '
set -eu
file="/controller/external/$1/pgpass"
password="$2"
tmp="${file}.tmp"
awk -F: -v password="${password}" "BEGIN { OFS=FS } { \$NF=password; print }" "${file}" >"${tmp}"
chmod 600 "${tmp}"
mv "${tmp}" "${file}"
' sh "${external_name}" "${password}" >/dev/null
		found=0
	done <<<"${pods}"
	(( found == 0 ))
}

restart_external_receiver() {
	local context="$1" namespace="$2" name="$3" external_name="$4" pods pod
	pods=$(kubectl --context "${context}" -n "${namespace}" get pods -l "cnpg.io/cluster=${name}" \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
	while read -r pod; do
		[[ -n "${pod}" ]] || continue
		kubectl --context "${context}" -n "${namespace}" exec "${pod}" -c postgres -- sh -c '
set -eu
file="/controller/external/$1/pgpass"
test -f "${file}" || exit 0
pid=$(psql -Atqc "select pid from pg_stat_wal_receiver" || true)
case "${pid}" in
	""|*[!0-9]*) exit 0 ;;
	esac
kill -TERM "${pid}"
' sh "${external_name}" >/dev/null 2>&1 || true
	done <<<"${pods}"
}

scenario_invalid_password() {
	local context="${US_CONTEXT}" namespace="${FIRST_NAMESPACE}" name=db05 secret original
	log "12 invalid password safety and recovery observation"
	secret=$(kubectl --context "${context}" -n "${namespace}" get cluster "${name}" -o json | jq -r '.spec.externalClusters[] | select(.name == "db03") | .password.name')
	original=$(kubectl --context "${context}" -n "${namespace}" get secret "${secret}" -o jsonpath='{.data.password}')
	kubectl --context "${context}" -n "${namespace}" patch secret "${secret}" --type=merge -p '{"data":{"password":"aW52YWxpZC1wYXNzd29yZA=="}}' >/dev/null
	# CNPG 1.30 does not refresh an already-mounted external-cluster pgpass file
	# for a Secret-only update. Refresh that file as the actor, then terminate
	# only the WAL receiver so it reconnects with the invalid password. This
	# creates no Kubernetes Pod event, while the controller still never reads or
	# watches the target Secret.
	set_external_pgpass_password "${context}" "${namespace}" "${name}" db03 invalid-password
	restart_external_receiver "${context}" "${namespace}" "${name}" db03
	wait_for "invalid password breaks receiver" 5m replica_streaming_false "${context}" "${namespace}" "${name}"
	kubectl --context "${context}" -n "${namespace}" patch secret "${secret}" --type=merge -p "{\"data\":{\"password\":\"${original}\"}}" >/dev/null
	set_external_pgpass_password "${context}" "${namespace}" "${name}" db03 "$(printf '%s' "${original}" | base64 -d)"
	restart_external_receiver "${context}" "${namespace}" "${name}" db03
	wait_for "receiver recovers after invalid-password test" 5m replica_streaming "${context}" "${namespace}" "${name}"
	record "13 invalid password did not commit or revoke the active lease: passed"
}

replica_streaming_false() { ! replica_streaming "$1" "$2" "$3"; }

scenario_deletion_cleanup() {
	local lease
	log "14 database deletion and dynamic lease cleanup"
	lease=$(state_lease "${EU_CONTEXT}" "${SECOND_NAMESPACE}" db08)
	kubectl --context "${US_CONTEXT}" -n "${SECOND_NAMESPACE}" delete cluster db07 --wait=false >/dev/null
	kubectl --context "${EU_CONTEXT}" -n "${SECOND_NAMESPACE}" delete cluster db08 --wait=false >/dev/null
	wait_for "db08 orphan state cleanup" 15m state_entry_absent "${EU_CONTEXT}" "${SECOND_NAMESPACE}" db08
	[[ -n "${lease}" ]]
	if curl --fail --silent --show-error -o /dev/null -H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" \
		-X POST -H 'Content-Type: application/json' --data "$(jq -cn --arg lease "${lease}" '{lease_id:$lease}')" \
		"${VAULT_ADDR}/v1/sys/leases/lookup"; then
		log "deleted replica lease is still present in Vault" >&2
		return 1
	fi
	record "14 two-absence orphan cleanup and lease revocation: passed"
}

main() {
	: >"${ARTIFACT_DIR}/scenario-checkpoints.txt"
	scenario_initial
	scenario_failover
	scenario_pod_replacement
	scenario_drain
	scenario_switchover
	scenario_promotion_cleanup
	scenario_rebuild
	scenario_second_namespace
	scenario_namespace_removal
	scenario_unwatched_failover
	scenario_namespace_restore
	scenario_invalid_password
	scenario_deletion_cleanup
	log "ordered scenarios complete"
}

main "$@"
