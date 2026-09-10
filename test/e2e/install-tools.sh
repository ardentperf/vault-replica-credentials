#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=versions.env
source "${ROOT_DIR}/test/e2e/versions.env"

TOOLS_DIR=${E2E_TOOLS_DIR:-${RUNNER_TEMP:-/tmp}/vault-replica-e2e-tools}
BIN_DIR="${TOOLS_DIR}/bin"
mkdir -p "${BIN_DIR}"
curl_args=(--fail --silent --show-error --location --retry 3 --connect-timeout 10 --max-time 300)

case "$(uname -m)" in
	x86_64) kind_arch=amd64; cnpg_arch=x86_64; act_arch=x86_64 ;;
	aarch64|arm64) kind_arch=arm64; cnpg_arch=arm64; act_arch=arm64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

curl "${curl_args[@]}" -o "${BIN_DIR}/kind" \
	"https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-linux-${kind_arch}"
curl "${curl_args[@]}" -o "${BIN_DIR}/kubectl" \
	"https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${kind_arch}/kubectl"

cnpg_archive="${TOOLS_DIR}/kubectl-cnpg.tar.gz"
curl "${curl_args[@]}" -o "${cnpg_archive}" \
	"https://github.com/cloudnative-pg/cloudnative-pg/releases/download/${CNPG_PLUGIN_VERSION}/kubectl-cnpg_${CNPG_PLUGIN_VERSION#v}_linux_${cnpg_arch}.tar.gz"
tar -C "${BIN_DIR}" -xzf "${cnpg_archive}" kubectl-cnpg

act_archive="${TOOLS_DIR}/act.tar.gz"
curl "${curl_args[@]}" -o "${act_archive}" \
	"https://github.com/nektos/act/releases/download/${ACT_VERSION}/act_Linux_${act_arch}.tar.gz"
tar -C "${BIN_DIR}" -xzf "${act_archive}" act
chmod 0755 "${BIN_DIR}/kind" "${BIN_DIR}/kubectl" "${BIN_DIR}/kubectl-cnpg" "${BIN_DIR}/act"

if [[ -n "${GITHUB_PATH:-}" ]]; then
	echo "${BIN_DIR}" >>"${GITHUB_PATH}"
fi
echo "installed pinned E2E tools in ${BIN_DIR}"
