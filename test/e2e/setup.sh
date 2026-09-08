#!/usr/bin/env bash

# Provision the complete no-database E2E environment. Later ordered scenarios
# are intentionally separate from this infrastructure setup so this script cannot
# accidentally create or mutate CNPG Cluster resources.
set -euo pipefail
umask 077

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
export PATH="${ROOT_DIR}/.tools:${PATH}"
E2E_DIR="${ROOT_DIR}/test/e2e"
mkdir -p "${ROOT_DIR}/artifacts"
ARTIFACT_DIR="${E2E_ARTIFACT_DIR:-$(mktemp -d "${ROOT_DIR}/artifacts/e2e.XXXXXX")}"
PRIVATE_DIR=$(mktemp -d "${TMPDIR:-/tmp}/vrc-private.XXXXXX")
export ARTIFACT_DIR
KEEP_E2E_CLUSTERS="${KEEP_E2E_CLUSTERS:-false}"

# shellcheck source=test/e2e/versions.sh
source "${E2E_DIR}/versions.sh"
CNPG_VERSION_BARE="${CNPG_VERSION#v}"
CNPG_RELEASE_BRANCH="${CNPG_VERSION_BARE%.*}"
CNPG_WATCH_NAMESPACE="${CNPG_WATCH_NAMESPACE:-e2e-bootstrap}"
VAULT_HOST_PORT="${VAULT_HOST_PORT:-18200}"
VAULT_ROOT_TOKEN="${VAULT_ROOT_TOKEN:-e2e-dev-root-token}"
export VAULT_ROOT_TOKEN
RUN_ID=$(basename "${ARTIFACT_DIR}")
OPERATOR_IMAGE="${OPERATOR_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials:e2e-${RUN_ID}}"
GATEWAY_IMAGE="${GATEWAY_IMAGE:-ghcr.io/ardentperf/vault-replica-credentials-gateway:e2e-${RUN_ID}}"
export OPERATOR_IMAGE GATEWAY_IMAGE

US_CLUSTER="k8s-us"
EU_CLUSTER="k8s-eu"
US_CONTEXT="kind-${US_CLUSTER}"
EU_CONTEXT="kind-${EU_CLUSTER}"
US_KUBECONFIG="${PRIVATE_DIR}/kubeconfig-us.yaml"
EU_KUBECONFIG="${PRIVATE_DIR}/kubeconfig-eu.yaml"
export KUBECONFIG="${US_KUBECONFIG}:${EU_KUBECONFIG}"

mkdir -p "${ARTIFACT_DIR}"

created_us=false
created_eu=false
runner_connected=false
runner_container=""

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
		if [[ "${region}" == us && "${created_us}" != true ]] || [[ "${region}" == eu && "${created_eu}" != true ]]; then continue; fi
		context=$(context_for "${region}")
		kubectl --context "${context}" get pods -A -o wide 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/pods-${region}.txt"
		kubectl --context "${context}" get events -A 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/events-${region}.txt"
		kubectl --context "${context}" get clusters.postgresql.cnpg.io -A -o json 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/clusters-${region}.json"
		kubectl --context "${context}" -n cnpg-system get secret vault-replica-controller-state -o json 2>/dev/null | jq -r '.data["state.json"] | @base64d' | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/state-${region}.json"
		kubectl --context "${context}" -n cnpg-system logs deployment/cnpg-controller-manager \
			--all-containers=true 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/cnpg-${region}.log"
		kubectl --context "${context}" -n cnpg-system logs deployment/vault-replica-controller \
			--all-containers=true 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/controller-${region}.log"
		while read -r namespace gateway; do
			[[ -n "${namespace}" && -n "${gateway}" ]] || continue
			kubectl --request-timeout=10s --context "${context}" -n "${namespace}" logs "deployment/${gateway}" --tail=100 2>&1 |
				"${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/gateway-${region}-${namespace}-${gateway}.log"
		done < <(kubectl --request-timeout=10s --context "${context}" get deployments -A -o json 2>/dev/null | jq -r '.items[] | select(.metadata.name | startswith("gateway-db")) | [.metadata.namespace,.metadata.name] | @tsv')
		while read -r namespace primary; do
			[[ -n "${namespace}" && -n "${primary}" ]] || continue
			kubectl --request-timeout=10s --context "${context}" get --raw "/api/v1/namespaces/${namespace}/pods/https:${primary}:8000/proxy/pg/status" 2>&1 |
				"${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/status-${region}-${namespace}-${primary}.json"
		done < <(kubectl --request-timeout=10s --context "${context}" get clusters.postgresql.cnpg.io -A -o json 2>/dev/null | jq -r '.items[] | select(.status.currentPrimary != null) | [.metadata.namespace,.status.currentPrimary] | @tsv')
	done
	if [[ "${created_us}" == true ]]; then
		kubectl --context "${US_CONTEXT}" -n vault logs deployment/vault \
			--all-containers=true 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact >"${ARTIFACT_DIR}/vault.log"
	fi
}

cleanup() {
	exit_code=$?
	trap - EXIT INT TERM
	log "collecting diagnostics in ${ARTIFACT_DIR} (exit ${exit_code})"
	collect_logs
	if [[ "${KEEP_E2E_CLUSTERS}" == "true" ]]; then
		log "keeping run-owned Kind clusters; artifacts are in ${ARTIFACT_DIR}"
	else
		if [[ "${created_us}" == "true" ]]; then
			delete_owned_cluster "${US_CLUSTER}" || exit_code=1
		fi
		if [[ "${created_eu}" == "true" ]]; then
			delete_owned_cluster "${EU_CLUSTER}" || exit_code=1
		fi
		if [[ "${exit_code}" == 0 ]] || ! kind get clusters 2>/dev/null | rg -qx "${US_CLUSTER}|${EU_CLUSTER}"; then
			rm -f -- "${US_KUBECONFIG}" "${EU_KUBECONFIG}" "${PRIVATE_DIR}/e2e-runner"
			rmdir -- "${PRIVATE_DIR}"
		fi
	fi
	if [[ "${runner_connected}" == true ]]; then
		docker network disconnect kind "${runner_container}" || exit_code=1
	fi
	# act's root job writes into a bind-mounted checkout. Make only the
	# redacted run artifacts readable to the invoking host user.
	if [[ "${ACT:-false}" == true ]]; then chmod -R a+rX "${ARTIFACT_DIR}"; fi
	exit "${exit_code}"
}
delete_owned_cluster() {
	local cluster="$1"
	local attempt
	local remaining
	for attempt in 1 2 3; do
		log "cleanup ${cluster}, attempt ${attempt}"
		kind delete cluster --name "${cluster}" 2>&1 | "${ROOT_DIR}/.tools/e2e-runner" --redact
		if remaining=$(kind get clusters) && ! rg -qx "${cluster}" <<<"${remaining}"; then return 0; fi
	done
	log "failed to remove run-owned cluster ${cluster}"
	return 1
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

require_commands() {
	local command_name
	for command_name in docker kind kubectl curl go jq rg; do
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
	cp "${E2E_DIR}/kind-cluster.yaml" "${ARTIFACT_DIR}/kind-cluster.yaml"
	{
		docker version --format '{{.Server.Version}}'
		kind version
		kubectl version --client=true --output=yaml
		kubectl cnpg version
		go version
		echo "CNPG_VERSION=${CNPG_VERSION}"
		echo "CNPG_RELEASE_BRANCH=${CNPG_RELEASE_BRANCH}"
		echo "VAULT_IMAGE=${VAULT_IMAGE}"
		echo "POSTGRES_IMAGE=${POSTGRES_IMAGE}"
		echo "PROMETHEUS_IMAGE=${PROMETHEUS_IMAGE}"
		git -C "${ROOT_DIR}" rev-parse HEAD
	} >"${ARTIFACT_DIR}/versions.txt" 2>&1 || true
}

refuse_existing_clusters() {
	local existing
	if ! existing=$(kind get clusters); then
		log "cannot verify existing Kind clusters; refusing to claim ownership"
		return 1
	fi
	if rg -qx "${US_CLUSTER}|${EU_CLUSTER}" <<<"${existing}"; then
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
	kind create cluster --name "${cluster}" --image "${KIND_NODE_IMAGE}" --config "${E2E_DIR}/kind-cluster.yaml" \
		--kubeconfig "${kubeconfig}" --wait 5m
	# act runs on Docker's bridge network. Its loopback is not the Docker
	# host's loopback, and bridge isolation prevents reaching the Kind network.
	# Join only this job container and use Kind's internal, TLS-verified API.
	if [[ "${ACT:-false}" == true ]]; then
		runner_container=$(docker inspect --format '{{.Id}}' "$(hostname)")
		if [[ -z "$(docker inspect --format '{{with index .NetworkSettings.Networks "kind"}}{{.NetworkID}}{{end}}' "${runner_container}")" ]]; then
			docker network connect kind "${runner_container}"
			runner_connected=true
		fi
		kind export kubeconfig --name "${cluster}" --kubeconfig "${kubeconfig}" --internal
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
	# The released renderer may omit its watch flag; use CNPG's documented
	# operator ConfigMap and reload it explicitly.
	jq -n --arg namespaces "${CNPG_WATCH_NAMESPACE}" '{apiVersion:"v1",kind:"ConfigMap",metadata:{namespace:"cnpg-system",name:"cnpg-controller-manager-config"},data:{WATCH_NAMESPACE:$namespaces}}' |
		kubectl --context "${context}" apply --server-side -f -
	kubectl --context "${context}" -n cnpg-system rollout restart deployment/cnpg-controller-manager
	kubectl --context "${context}" -n cnpg-system rollout status \
		deployment/cnpg-controller-manager --timeout=5m
	test "$(kubectl --context "${context}" -n cnpg-system get configmap cnpg-controller-manager-config -o jsonpath='{.data.WATCH_NAMESPACE}')" = "${CNPG_WATCH_NAMESPACE}"
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
		sed 's/namespace: reporting/namespace: e2e-bootstrap/g' \
			"${ROOT_DIR}/config/rbac/${manifest}" | kubectl --context "${context}" apply -f -
	done
}

install_controller() {
	local region="$1"
	local context
	context=$(context_for "${region}")
	log "installing controller in ${context}"
	jq -n '{apiVersion:"v1",kind:"Secret",metadata:{name:"vault-replica-controller-token",namespace:"cnpg-system"},stringData:{token:env.VAULT_ROOT_TOKEN}}' | kubectl --context "${context}" apply -f -
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/serviceaccount.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/system-role.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/rbac/system-rolebinding.yaml"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/installation/state-secret.yaml"
	apply_target_rbac "${context}"
	kubectl --context "${context}" apply -f "${ROOT_DIR}/config/manager/deployment.yaml"
	kubectl --context "${context}" -n cnpg-system set image deployment/vault-replica-controller \
		manager="${OPERATOR_IMAGE}"
	kubectl --context "${context}" -n cnpg-system set env deployment/vault-replica-controller \
		WATCH_NAMESPACE="${CNPG_WATCH_NAMESPACE}" \
		VAULT_ADDR="${VAULT_ADDR}" \
		VAULT_ALLOW_INSECURE_HTTP=true
	kubectl --context "${context}" -n cnpg-system rollout status \
		deployment/vault-replica-controller --timeout=5m
}

assert_empty_database_inventory() {
	for context in "${US_CONTEXT}" "${EU_CONTEXT}"; do
		if kubectl --context "${context}" get clusters.postgresql.cnpg.io -A \
			-o name | rg -q .; then
			log "unexpected CNPG Cluster resource exists in ${context}"
			return 1
		fi
	done
}

main() {
	require_commands
	refuse_existing_clusters
	mkdir -p "${ROOT_DIR}/.tools"
	# Build away from the shared cache: an earlier root act job can leave a
	# binary that a host user cannot inspect or overwrite through go build.
	go build -o "${PRIVATE_DIR}/e2e-runner" ./test/e2e/runner
	install -m 0755 "${PRIVATE_DIR}/e2e-runner" "${ROOT_DIR}/.tools/e2e-runner"
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
	assert_empty_database_inventory
	if [[ "${1:-}" == "--suite" ]]; then
		"${ROOT_DIR}/.tools/e2e-runner"
	else
		"${ROOT_DIR}/.tools/e2e-runner" --infrastructure
	fi
	log "complete: full no-database environment is running"
	log "artifacts: ${ARTIFACT_DIR}"
	log "set KEEP_E2E_CLUSTERS=true to retain the two Kind clusters after exit"
}

main "$@"
