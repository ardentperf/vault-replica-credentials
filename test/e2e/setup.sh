#!/usr/bin/env bash

# Provision the complete no-database E2E environment. Database scenarios are
# run by run.sh after this script has established the clean infrastructure.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
E2E_DIR="${ROOT_DIR}/test/e2e"
ARTIFACT_DIR="${E2E_ARTIFACT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/vault-replica-e2e.XXXXXX")}"
KEEP_E2E_CLUSTERS="${KEEP_E2E_CLUSTERS:-false}"

CNPG_VERSION="${CNPG_VERSION:-v1.30.0}"
CNPG_VERSION_BARE="${CNPG_VERSION#v}"
CNPG_RELEASE_BRANCH="${CNPG_VERSION_BARE%.*}"
CNPG_WATCH_NAMESPACE="${CNPG_WATCH_NAMESPACE:-e2e-bootstrap}"
KIND_VERSION="${KIND_VERSION:-v0.30.0}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.35.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.35.0}"
CNPG_PLUGIN_VERSION="${CNPG_PLUGIN_VERSION:-v1.30.0}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-ghcr.io/cloudnative-pg/postgresql:18.0}"
GO_IMAGE="${GO_IMAGE:-golang:1.26.3-bookworm}"
VAULT_IMAGE="${VAULT_IMAGE:-hashicorp/vault:1.20.4}"
VAULT_HOST_PORT="${VAULT_HOST_PORT:-18200}"
VAULT_ROOT_TOKEN="${VAULT_ROOT_TOKEN:-e2e-dev-root-token}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials:0.1.0-e2e}"
GATEWAY_IMAGE="${GATEWAY_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials-gateway:0.1.0-e2e}"
PROMETHEUS_IMAGE="${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996}"
PASSWORD_PROPAGATION_DELAY="${PASSWORD_PROPAGATION_DELAY:-5s}"
VERIFICATION_DELAY="${VERIFICATION_DELAY:-2m}"
ORPHAN_SWEEP_INTERVAL="${ORPHAN_SWEEP_INTERVAL:-5m}"

US_CLUSTER="k8s-us"
EU_CLUSTER="k8s-eu"
US_CONTEXT="kind-${US_CLUSTER}"
EU_CONTEXT="kind-${EU_CLUSTER}"
US_KUBECONFIG="${ARTIFACT_DIR}/kubeconfig-us.yaml"
EU_KUBECONFIG="${ARTIFACT_DIR}/kubeconfig-eu.yaml"
export KUBECONFIG="${US_KUBECONFIG}:${EU_KUBECONFIG}"

mkdir -p "${ARTIFACT_DIR}"

created_us=false
created_eu=false
PROMETHEUS_PIDS=()

log() {
	echo "[e2e-setup] $*"
}

context_for() {
	case "$1" in
		us) echo "${US_CONTEXT}" ;;
		eu) echo "${EU_CONTEXT}" ;;
		*) echo "unknown region: $1" >&2; return 1 ;;
	esac
}

cluster_for() {
	case "$1" in
		us) echo "${US_CLUSTER}" ;;
		eu) echo "${EU_CLUSTER}" ;;
		*) echo "unknown region: $1" >&2; return 1 ;;
	esac
}

kubeconfig_for() {
	case "$1" in
		us) echo "${US_KUBECONFIG}" ;;
		eu) echo "${EU_KUBECONFIG}" ;;
		*) echo "unknown region: $1" >&2; return 1 ;;
	esac
}

collect_logs() {
	set +e
	for region in us eu; do
		context=$(context_for "${region}")
		kubectl --context "${context}" -n cnpg-system logs deployment/cnpg-controller-manager \
			--all-containers=true >"${ARTIFACT_DIR}/cnpg-${region}.log" 2>&1
		kubectl --context "${context}" -n cnpg-system logs deployment/vault-replica-controller \
			--all-containers=true >"${ARTIFACT_DIR}/controller-${region}.log" 2>&1
	done
	kubectl --context "${US_CONTEXT}" -n vault logs deployment/vault \
		--all-containers=true >"${ARTIFACT_DIR}/vault.log" 2>&1
	for region in us eu; do
		context=$(context_for "${region}")
		kubectl --context "${context}" -n e2e-monitoring logs deployment/prometheus \
			--all-containers=true >"${ARTIFACT_DIR}/prometheus-${region}.log" 2>&1
		kubectl --context "${context}" -n e2e-first logs -l app.kubernetes.io/name=cnpg-test-gateway \
			--all-containers=true >"${ARTIFACT_DIR}/gateway-${region}.log" 2>&1
	done
}

cleanup() {
	exit_code=$?
	if [[ "${exit_code}" -ne 0 ]]; then
		log "setup failed; collecting diagnostics in ${ARTIFACT_DIR}"
		collect_logs
	fi
	if [[ "${KEEP_E2E_CLUSTERS}" == "true" ]]; then
		log "keeping run-owned Kind clusters; artifacts are in ${ARTIFACT_DIR}"
	else
		for pid in "${PROMETHEUS_PIDS[@]}"; do kill "${pid}" >/dev/null 2>&1 || true; done
		if [[ "${created_us}" == "true" ]]; then
			kind delete cluster --name "${US_CLUSTER}" >/dev/null 2>&1 || true
		fi
		if [[ "${created_eu}" == "true" ]]; then
			kind delete cluster --name "${EU_CLUSTER}" >/dev/null 2>&1 || true
		fi
	fi
	exit "${exit_code}"
}
trap cleanup EXIT

require_commands() {
	local command_name
	for command_name in docker kind kubectl curl jq; do
		command -v "${command_name}" >/dev/null || {
			log "missing required command: ${command_name}"
			return 1
		}
	done
	if ! command -v go >/dev/null 2>&1; then
		docker run --rm "${GO_IMAGE}" go version >/dev/null || {
			log "Go is missing and the configured Go build image is unavailable"
			return 1
		}
	fi
	kubectl cnpg --help >/dev/null 2>&1 || {
		log "kubectl cnpg plugin is required"
		return 1
	}
}

record_versions() {
	{
		docker version --format '{{.Server.Version}}'
		kind version
		kubectl version --client=true --output=yaml
		kubectl cnpg version
		if command -v go >/dev/null 2>&1; then go version; else docker run --rm "${GO_IMAGE}" go version; fi
		jq --version
		echo "GATEWAY_IMAGE=${GATEWAY_IMAGE}"
		echo "PROMETHEUS_IMAGE=${PROMETHEUS_IMAGE}"
		echo "CNPG_VERSION=${CNPG_VERSION}"
		echo "CNPG_RELEASE_BRANCH=${CNPG_RELEASE_BRANCH}"
		echo "KIND_VERSION=${KIND_VERSION}"
		echo "KIND_NODE_IMAGE=${KIND_NODE_IMAGE}"
		echo "KUBECTL_VERSION=${KUBECTL_VERSION}"
		echo "CNPG_PLUGIN_VERSION=${CNPG_PLUGIN_VERSION}"
		echo "POSTGRES_IMAGE=${POSTGRES_IMAGE}"
		echo "GO_IMAGE=${GO_IMAGE}"
		echo "PASSWORD_PROPAGATION_DELAY=${PASSWORD_PROPAGATION_DELAY}"
		echo "VERIFICATION_DELAY=${VERIFICATION_DELAY}"
		echo "ORPHAN_SWEEP_INTERVAL=${ORPHAN_SWEEP_INTERVAL}"
	} >"${ARTIFACT_DIR}/versions.txt" 2>&1 || true
}

refuse_existing_clusters() {
	local existing
	existing=$(kind get clusters 2>/dev/null || true)
	if grep -qx "${US_CLUSTER}" <<<"${existing}" || grep -qx "${EU_CLUSTER}" <<<"${existing}"; then
		log "refusing to reuse existing ${US_CLUSTER} or ${EU_CLUSTER} Kind cluster"
		return 1
	fi
}

create_cluster() {
	local region="$1"
	local cluster
	local kubeconfig
	cluster=$(cluster_for "${region}")
	kubeconfig=$(kubeconfig_for "${region}")
	log "creating ${cluster}"
	kind create cluster --name "${cluster}" --config "${E2E_DIR}/kind-cluster.yaml" \
		--image "${KIND_NODE_IMAGE}" \
		--kubeconfig "${kubeconfig}"
	if [[ "${region}" == "us" ]]; then
		created_us=true
	else
		created_eu=true
	fi
}

label_nodes() {
	local region="$1"
	local context
	local zone_index=1
	local node
	context=$(context_for "${region}")

	kubectl --context "${context}" label node -l postgres.node.kubernetes.io \
		node-role.kubernetes.io/postgres="" --overwrite
	kubectl --context "${context}" label nodes --all --overwrite \
		topology.kubernetes.io/region="${region}" \
		topology.kubernetes.io/zone="${region}-1"

	while IFS= read -r node; do
		kubectl --context "${context}" label "${node}" --overwrite \
			topology.kubernetes.io/zone="${region}-${zone_index}"
		zone_index=$((zone_index + 1))
	done < <(kubectl --context "${context}" get nodes -l postgres.node.kubernetes.io \
		-o name | sort)
}

assert_node_topology() {
	local region="$1"
	local context
	local control_plane_count
	local postgres_count
	context=$(context_for "${region}")
	control_plane_count=$(kubectl --context "${context}" get nodes \
		-l node-role.kubernetes.io/control-plane --no-headers | wc -l)
	postgres_count=$(kubectl --context "${context}" get nodes \
		-l postgres.node.kubernetes.io --no-headers | wc -l)
	if [[ "${control_plane_count}" -ne 1 || "${postgres_count}" -ne 3 ]]; then
		log "unexpected ${region} topology: control-plane=${control_plane_count}, postgres=${postgres_count}"
		return 1
	fi
}

install_cnpg() {
	local region="$1"
	local context
	local manifest
	context=$(context_for "${region}")
	manifest="${ARTIFACT_DIR}/cnpg-${region}.yaml"
	log "installing CloudNativePG ${CNPG_VERSION} in ${context}"
	kubectl cnpg install generate --version "${CNPG_VERSION_BARE}" \
		--watch-namespace "${CNPG_WATCH_NAMESPACE}" --control-plane >"${manifest}"
	kubectl --context "${context}" apply --server-side -f "${manifest}"
	kubectl --context "${context}" -n cnpg-system rollout status \
		deployment/cnpg-controller-manager --timeout=5m
	kubectl --context "${context}" get crd clusters.postgresql.cnpg.io >/dev/null
}

render_vault_manifest() {
	sed \
		-e "s|__VAULT_IMAGE__|${VAULT_IMAGE}|g" \
		-e "s|__VAULT_ROOT_TOKEN__|${VAULT_ROOT_TOKEN}|g" \
		-e "s|__VAULT_HOST_PORT__|${VAULT_HOST_PORT}|g" \
		"${E2E_DIR}/manifests/vault.yaml"
}

install_vault() {
	local control_plane_ip
	log "installing one ephemeral HTTP Vault in ${US_CONTEXT}"
	kubectl --context "${US_CONTEXT}" create namespace vault
	render_vault_manifest | kubectl --context "${US_CONTEXT}" apply -f -
	kubectl --context "${US_CONTEXT}" -n vault rollout status deployment/vault --timeout=5m
	control_plane_ip=$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' \
		"${US_CLUSTER}-control-plane")
	if [[ -z "${control_plane_ip}" ]]; then
		log "could not determine the US control-plane Docker-network IP"
		return 1
	fi
	VAULT_ADDR="http://${control_plane_ip}:${VAULT_HOST_PORT}"
	export VAULT_ADDR
	curl --fail --silent --show-error "${VAULT_ADDR}/v1/sys/health" >/dev/null
	{
		echo "VAULT_ADDR=${VAULT_ADDR}"
		echo "VAULT_HOST_PORT=${VAULT_HOST_PORT}"
	} >"${ARTIFACT_DIR}/vault-endpoint.txt"
}

build_images() {
	log "building and loading local operator and gateway images"
	docker build --tag "${OPERATOR_IMAGE}" "${ROOT_DIR}"
	docker build --file "${ROOT_DIR}/Dockerfile.gateway" --tag "${GATEWAY_IMAGE}" "${ROOT_DIR}"
	for cluster in "${US_CLUSTER}" "${EU_CLUSTER}"; do
		kind load docker-image --name "${cluster}" "${OPERATOR_IMAGE}"
		kind load docker-image --name "${cluster}" "${GATEWAY_IMAGE}"
	done
}

apply_target_rbac() {
	local context="$1"
	for manifest in target-role.yaml target-rolebinding.yaml; do
		sed "s/namespace: reporting/namespace: ${CNPG_WATCH_NAMESPACE}/g" \
			"${ROOT_DIR}/config/rbac/${manifest}" | kubectl --context "${context}" apply -f -
	done
}

install_controller() {
	local region="$1"
	local context
	context=$(context_for "${region}")
	log "installing Vault replica controller in ${context}"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/serviceaccount.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/system-role.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/system-rolebinding.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/installation/state-secret.yaml"
	kubectl --context "${context}" -n cnpg-system create secret generic vault-replica-controller-vault-token \
		--from-literal=token="${VAULT_ROOT_TOKEN}" --dry-run=client -o yaml | kubectl --context "${context}" apply -f -
	apply_target_rbac "${context}"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/manager/deployment.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/manager/service.yaml"
	kubectl --context "${context}" -n cnpg-system set image deployment/vault-replica-controller \
		manager="${OPERATOR_IMAGE}"
	kubectl --context "${context}" -n cnpg-system set env deployment/vault-replica-controller \
		WATCH_NAMESPACE="${CNPG_WATCH_NAMESPACE}" \
		VAULT_ADDR="${VAULT_ADDR}" \
		VAULT_ALLOW_INSECURE_HTTP=true \
		PASSWORD_PROPAGATION_DELAY="${PASSWORD_PROPAGATION_DELAY}" \
		VERIFICATION_DELAY="${VERIFICATION_DELAY}" \
		ORPHAN_SWEEP_INTERVAL="${ORPHAN_SWEEP_INTERVAL}"
	kubectl --context "${context}" -n cnpg-system rollout status \
		deployment/vault-replica-controller --timeout=5m
}

install_prometheus() {
	local region="$1" context port
	context=$(context_for "${region}")
	port=19090
	[[ "${region}" == "eu" ]] && port=19091
	sed "s|prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996|${PROMETHEUS_IMAGE}|g" "${E2E_DIR}/manifests/prometheus.yaml" \
		| kubectl --context "${context}" apply -f -
	kubectl --context "${context}" -n e2e-monitoring rollout status deployment/prometheus --timeout=5m
	kubectl --context "${context}" -n e2e-monitoring port-forward --address 127.0.0.1 \
		"svc/prometheus" "${port}:9090" >"${ARTIFACT_DIR}/prometheus-port-forward-${region}.log" 2>&1 &
	PROMETHEUS_PIDS+=("$!")
	echo "$!" >>"${ARTIFACT_DIR}/prometheus.pids"
	for _ in {1..30}; do
		if curl --fail --silent "http://127.0.0.1:${port}/-/ready" >/dev/null 2>&1; then break; fi
		sleep 1
	done
	for _ in {1..60}; do
		if curl --fail --silent "http://127.0.0.1:${port}/api/v1/targets" >"${ARTIFACT_DIR}/prometheus-targets-${region}.json"; then
			if jq -e '.data.activeTargets | map(select(.labels.job == "vault-replica-controller" and .health == "up")) | length == 1' \
				"${ARTIFACT_DIR}/prometheus-targets-${region}.json" >/dev/null; then
				return 0
			fi
		fi
		sleep 1
	done
	log "Prometheus did not report a healthy controller target for ${region}"
	return 1
}

assert_controller_rbac() {
	local context="$1" identity="system:serviceaccount:cnpg-system:vault-replica-controller"
	for verb in get list watch update create delete; do
		if kubectl --context "${context}" auth can-i "${verb}" secrets -n e2e-bootstrap --as="${identity}" | grep -q '^yes$'; then
			log "unsafe target Secret permission: ${verb} in ${context}"
			return 1
		fi
	done
	kubectl --context "${context}" auth can-i patch secrets -n e2e-bootstrap --as="${identity}" | grep -q '^yes$'
	if kubectl --context "${context}" get clusterrolebinding -o json | jq -e --arg identity "${identity}" '.. | objects | select(.kind? == "ServiceAccount") | select((.namespace + ":" + .name) == $identity)' >/dev/null; then
		log "controller has an unsafe ClusterRoleBinding in ${context}"
		return 1
	fi
}

assert_empty_database_inventory() {
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		if kubectl --context "${context}" get clusters.postgresql.cnpg.io -A \
			-o name | grep -q .; then
			log "unexpected CNPG Cluster resource exists in ${context}"
			return 1
		fi
	done
}

main() {
	require_commands
	refuse_existing_clusters
	record_versions
	create_cluster us
	create_cluster eu
	label_nodes us
	label_nodes eu
	assert_node_topology us
	assert_node_topology eu
	kubectl --context "${US_CONTEXT}" create namespace "${CNPG_WATCH_NAMESPACE}"
	kubectl --context "${EU_CONTEXT}" create namespace "${CNPG_WATCH_NAMESPACE}"
	install_cnpg us
	install_cnpg eu
	install_vault
	build_images
	install_controller us
	install_controller eu
	install_prometheus us
	install_prometheus eu
	assert_empty_database_inventory
	assert_controller_rbac "${US_CONTEXT}"
	assert_controller_rbac "${EU_CONTEXT}"
	log "complete: full no-database environment is running"
	log "artifacts: ${ARTIFACT_DIR}"
	log "set KEEP_E2E_CLUSTERS=true to retain the two Kind clusters after exit"
}

main "$@"
