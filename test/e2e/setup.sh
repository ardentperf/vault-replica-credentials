#!/usr/bin/env bash

# Provision the complete no-database E2E environment. The ordered database
# scenarios stay in run.sh so infrastructure setup cannot accidentally create
# or mutate CNPG Cluster resources.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
E2E_DIR="${ROOT_DIR}/test/e2e"
# shellcheck source=versions.env
source "${E2E_DIR}/versions.env"
ARTIFACT_DIR="${E2E_ARTIFACT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/vault-replica-e2e.XXXXXX")}"
KEEP_E2E_CLUSTERS="${KEEP_E2E_CLUSTERS:-false}"

CNPG_VERSION_BARE="${CNPG_VERSION#v}"
CNPG_RELEASE_BRANCH="${CNPG_VERSION_BARE%.*}"
CNPG_WATCH_NAMESPACE="${CNPG_WATCH_NAMESPACE:-e2e-bootstrap}"
VAULT_IMAGE="${VAULT_IMAGE:-hashicorp/vault:${VAULT_VERSION}}"
PROMETHEUS_IMAGE="${PROMETHEUS_IMAGE:-prom/prometheus:${PROMETHEUS_VERSION}}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:${CURL_VERSION}}"
VAULT_HOST_PORT="${VAULT_HOST_PORT:-18200}"
VAULT_ROOT_TOKEN="${VAULT_ROOT_TOKEN:-e2e-dev-root-token}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials:e2e}"
GATEWAY_IMAGE="${GATEWAY_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials-gateway:e2e}"

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
			--all-containers=true 2>&1 | redact >"${ARTIFACT_DIR}/cnpg-${region}.log"
		kubectl --context "${context}" -n cnpg-system logs deployment/vault-replica-controller \
			--all-containers=true 2>&1 | redact >"${ARTIFACT_DIR}/controller-${region}.log"
		kubectl --context "${context}" get clusters.postgresql.cnpg.io,pods,jobs,deployments -A \
			-o wide 2>&1 | redact >"${ARTIFACT_DIR}/resources-${region}.log"
		kubectl --context "${context}" get events -A --sort-by=.lastTimestamp \
			2>&1 | redact >"${ARTIFACT_DIR}/events-${region}.log"
		kubectl --context "${context}" -n cnpg-system get secret vault-replica-controller-state \
			-o jsonpath='{.data.state\.json}' 2>/dev/null | base64 -d | jq '
			walk(if type == "object" then
				(if has("currentLeaseID") then .currentLeaseID = "[REDACTED_LEASE]" else . end) |
				(if has("leaseID") then .leaseID = "[REDACTED_LEASE]" else . end) |
				(if has("username") then .username = "[REDACTED_USERNAME]" else . end)
			else . end)' >"${ARTIFACT_DIR}/state-${region}.json" 2>/dev/null
		for deployment in $(kubectl --context "${context}" get deployments -A \
			-o jsonpath='{range .items[?(@.metadata.labels.app=="cnpg-test-gateway")]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
			namespace=${deployment%%/*}
			name=${deployment#*/}
			kubectl --context "${context}" -n "${namespace}" logs "deployment/${name}" \
				--all-containers=true 2>&1 | redact >>"${ARTIFACT_DIR}/gateways-${region}.log"
		done
	done
	kubectl --context "${US_CONTEXT}" -n vault logs deployment/vault \
		--all-containers=true 2>&1 | redact >"${ARTIFACT_DIR}/vault.log"
}

redact() {
	sed -E \
		-e "s|${VAULT_ROOT_TOKEN}|[REDACTED_TOKEN]|g" \
		-e 's/(password|token)(["=: ]+)[^", ]+/\1\2[REDACTED]/Ig' \
		-e 's/v-token-[A-Za-z0-9_-]+/[REDACTED_USERNAME]/g' \
		-e 's#database/creds/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+#[REDACTED_LEASE]#g' \
		-e 's/(Unseal Key:)[[:space:]]+.*/\1 [REDACTED]/g' \
		-e 's/dummy-(user|password)-value/[REDACTED_DUMMY]/g'
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
		if [[ "${created_us}" == "true" && -n "${VAULT_ADDR:-}" ]]; then
			# Best-effort cleanup runs while source databases are still reachable.
			# The dev Vault and its test-only lease namespace are run-owned.
			curl --fail --silent --show-error --connect-timeout 5 --max-time 30 \
				-H "X-Vault-Token: ${VAULT_ROOT_TOKEN}" \
				-X PUT "${VAULT_ADDR}/v1/sys/leases/revoke-prefix/database/creds/" \
				>/dev/null 2>&1 || true
			kubectl --context "${US_CONTEXT}" delete namespace vault --ignore-not-found \
				--wait=true --timeout=2m >/dev/null 2>&1 || true
		fi
		if [[ "${created_us}" == "true" ]]; then
			kind delete cluster --name "${US_CLUSTER}" >/dev/null 2>&1 || true
		fi
		if [[ "${created_eu}" == "true" ]]; then
			kind delete cluster --name "${EU_CLUSTER}" >/dev/null 2>&1 || true
		fi
		# Never retain or upload ephemeral kube-admin credentials after the
		# run-owned clusters have been deleted.
		rm -f -- "${US_KUBECONFIG}" "${EU_KUBECONFIG}"
	fi
	exit "${exit_code}"
}
trap cleanup EXIT

require_commands() {
	local command_name
	for command_name in docker kind kubectl curl go; do
		command -v "${command_name}" >/dev/null || {
			log "missing required command: ${command_name}"
			return 1
		}
	done
	kubectl cnpg --help >/dev/null 2>&1 || {
		log "kubectl cnpg plugin is required"
		return 1
	}
}

record_versions() {
	{
		echo "REPOSITORY_HEAD=$(git -C "${ROOT_DIR}" rev-parse HEAD)"
		echo "CNPG_PLAYGROUND_BASELINE=1957b42b445532d284513964f53e3085b4f745f9"
		sha256sum "${ROOT_DIR}/DESIGN.md" "${ROOT_DIR}/IMPLEMENTATION_PLAN.md" \
			"${ROOT_DIR}/E2E_TEST_PLAN.md"
		docker version --format '{{.Server.Version}}'
		kind version
		kubectl version --client=true --output=yaml
		kubectl cnpg version
		go version
		echo "CNPG_VERSION=${CNPG_VERSION}"
		echo "CNPG_RELEASE_BRANCH=${CNPG_RELEASE_BRANCH}"
	} >"${ARTIFACT_DIR}/versions.txt" 2>&1 || true
	cp "${E2E_DIR}/kind-cluster.yaml" "${ARTIFACT_DIR}/kind-cluster.yaml"
	kubectl kustomize "${ROOT_DIR}/config" >"${ARTIFACT_DIR}/controller-install.yaml"
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
	if [[ "${region}" == "us" ]]; then
		created_us=true
	else
		created_eu=true
	fi
	# Mark the reserved name as run-owned before Kind starts so an interrupt
	# during partial node creation still removes exactly this run's cluster.
	kind create cluster --name "${cluster}" --config "${E2E_DIR}/kind-cluster.yaml" \
		--image "kindest/node:${KIND_NODE_VERSION}" --kubeconfig "${kubeconfig}"
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
	render_vault_manifest | redact >"${ARTIFACT_DIR}/vault-redacted.yaml"
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
	curl --fail --silent --show-error --connect-timeout 5 --max-time 15 \
		"${VAULT_ADDR}/v1/sys/health" >/dev/null
	{
		echo "VAULT_ADDR=${VAULT_ADDR}"
		echo "VAULT_HOST_PORT=${VAULT_HOST_PORT}"
	} >"${ARTIFACT_DIR}/vault-endpoint.txt"
	for region in us eu; do
		context=$(context_for "${region}")
		kubectl --context "${context}" -n e2e-bootstrap run "vault-reachability-${region}" \
			--image="${CURL_IMAGE}" --restart=Never --rm --attach \
			--overrides='{"spec":{"nodeSelector":{"node-role.kubernetes.io/control-plane":""},"tolerations":[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]}}' -- \
			--fail --silent --show-error --connect-timeout 5 --max-time 30 \
			"${VAULT_ADDR}/v1/sys/health" >/dev/null
	done
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
		sed 's/namespace: reporting/namespace: e2e-bootstrap/g' \
			"${ROOT_DIR}/config/rbac/${manifest}" | kubectl --context "${context}" apply -f -
	done
}

install_controller() {
	local region="$1"
	local context
	context=$(context_for "${region}")
	log "installing credential controller in ${context}"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/serviceaccount.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/system-role.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/system-rolebinding.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/installation/state-secret.yaml"
	kubectl --context "${context}" -n cnpg-system create secret generic vault-replica-controller-vault \
		--from-literal=token="${VAULT_ROOT_TOKEN}"
	apply_target_rbac "${context}"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/manager/deployment.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/manager/service.yaml"
	kubectl --context "${context}" -n cnpg-system set image deployment/vault-replica-controller \
		manager="${OPERATOR_IMAGE}"
	kubectl --context "${context}" -n cnpg-system set env deployment/vault-replica-controller \
		WATCH_NAMESPACE="${CNPG_WATCH_NAMESPACE}" \
		VAULT_ADDR="${VAULT_ADDR}" \
		VAULT_ALLOW_INSECURE_HTTP=true
	kubectl --context "${context}" -n cnpg-system rollout status \
		deployment/vault-replica-controller --timeout=5m
	kubectl --context "${context}" -n cnpg-system get deployment/vault-replica-controller \
		-o yaml | redact >"${ARTIFACT_DIR}/controller-${region}.yaml"
}

install_prometheus() {
	local region="$1"
	local context
	local local_port
	local port_forward_pid
	local manifest
	context=$(context_for "${region}")
	manifest="${ARTIFACT_DIR}/prometheus-${region}.yaml"
	if [[ "${region}" == "us" ]]; then local_port=19090; else local_port=19091; fi
	log "installing Prometheus observer in ${context}"
	sed "s|__PROMETHEUS_IMAGE__|${PROMETHEUS_IMAGE}|g" \
		"${E2E_DIR}/manifests/prometheus.yaml" >"${manifest}"
	kubectl --context "${context}" apply -f "${manifest}"
	kubectl --context "${context}" -n cnpg-system rollout status deployment/prometheus --timeout=5m
	kubectl --context "${context}" -n cnpg-system port-forward service/prometheus \
		"${local_port}:9090" >"${ARTIFACT_DIR}/prometheus-port-forward-${region}.log" 2>&1 &
	port_forward_pid=$!
	for _ in $(seq 1 30); do
		if curl --fail --silent --show-error --connect-timeout 2 --max-time 5 \
			"http://127.0.0.1:${local_port}/api/v1/query?query=up%7Bjob%3D%22vault-replica-controller%22%7D" \
			| jq -e '.data.result[0].value[1] == "1"' >/dev/null 2>&1; then
			kill "${port_forward_pid}" >/dev/null 2>&1 || true
			wait "${port_forward_pid}" 2>/dev/null || true
			return 0
		fi
		sleep 2
	done
	kill "${port_forward_pid}" >/dev/null 2>&1 || true
	wait "${port_forward_pid}" 2>/dev/null || true
	echo "Prometheus did not scrape the regional controller" >&2
	return 1
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

setup_environment() {
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
	log "complete: full no-database environment is running"
	log "artifacts: ${ARTIFACT_DIR}"
	log "set KEEP_E2E_CLUSTERS=true to retain the two Kind clusters after exit"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	setup_environment "$@"
fi
