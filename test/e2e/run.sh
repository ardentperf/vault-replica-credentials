#!/usr/bin/env bash
# Complete clean-environment E2E gate. It owns only k8s-us and k8s-eu.
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
ARTIFACT_DIR="${E2E_ARTIFACT_DIR:-${ROOT_DIR}/artifacts/e2e}"
mkdir -p "${ARTIFACT_DIR}"
export E2E_ARTIFACT_DIR="${ARTIFACT_DIR}"
setup_completed=false

redact_artifacts() {
	local file
	while IFS= read -r -d '' file; do
		sed -E -i \
			-e 's/(password|token|secret|authorization)([=:][[:space:]]*)[^[:space:]]+/\1\2<redacted>/Ig' \
			-e 's/(VAULT_ROOT_TOKEN|VAULT_TOKEN|DUMMY_PASSWORD)=[^[:space:]]+/\1=<redacted>/g' \
			"${file}"
	done < <(find "${ARTIFACT_DIR}" -type f -print0)
}

collect_diagnostics() {
	local kubeconfig="${ARTIFACT_DIR}/kubeconfig-us.yaml:${ARTIFACT_DIR}/kubeconfig-eu.yaml"
	export KUBECONFIG="${kubeconfig}"
	set +e
	for context in kind-k8s-us kind-k8s-eu; do
		kubectl --context "${context}" get events -A >"${ARTIFACT_DIR}/${context}-events.log" 2>&1
		kubectl --context "${context}" get pods -A -o wide >"${ARTIFACT_DIR}/${context}-pods.txt" 2>&1
		kubectl --context "${context}" -n cnpg-system logs deployment/vault-replica-controller --all-containers=true >"${ARTIFACT_DIR}/${context}-controller.log" 2>&1
		done
	kubectl --context kind-k8s-us -n vault logs deployment/vault --all-containers=true >"${ARTIFACT_DIR}/vault.log" 2>&1
	redact_artifacts
}

cleanup() {
	local exit_code=$?
	if [[ "${exit_code}" -ne 0 && "${setup_completed}" == "true" ]]; then collect_diagnostics; fi
	if [[ -f "${ARTIFACT_DIR}/prometheus.pids" ]]; then
		while IFS= read -r pid; do kill "${pid}" >/dev/null 2>&1 || true; done <"${ARTIFACT_DIR}/prometheus.pids"
	fi
	if [[ "${setup_completed}" == "true" ]] && command -v kind >/dev/null 2>&1; then
		env -u KUBECONFIG kind delete cluster --name k8s-us >/dev/null 2>&1 || true
		env -u KUBECONFIG kind delete cluster --name k8s-eu >/dev/null 2>&1 || true
	fi
	if [[ "${exit_code}" -eq 0 ]]; then redact_artifacts; fi
	exit "${exit_code}"
}
trap cleanup EXIT

KEEP_E2E_CLUSTERS=true E2E_ARTIFACT_DIR="${ARTIFACT_DIR}" bash "${ROOT_DIR}/test/e2e/setup.sh"
setup_completed=true
KUBECONFIG="${ARTIFACT_DIR}/kubeconfig-us.yaml:${ARTIFACT_DIR}/kubeconfig-eu.yaml" \
	E2E_ARTIFACT_DIR="${ARTIFACT_DIR}" bash "${ROOT_DIR}/test/e2e/scenarios.sh"
