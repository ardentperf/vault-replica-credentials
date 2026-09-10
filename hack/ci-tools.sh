#!/usr/bin/env bash
set -euo pipefail

TOOLS_DIR="${CI_TOOLS_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/vault-replica-tools}"
KIND_VERSION="${KIND_VERSION:-v0.30.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.35.0}"
JQ_VERSION="${JQ_VERSION:-1.8.1}"
CNPG_PLUGIN_VERSION="${CNPG_PLUGIN_VERSION:-v1.30.0}"
SHELLCHECK_VERSION="${SHELLCHECK_VERSION:-v0.11.0}"

case "$(uname -m)" in
	aarch64|arm64) ARCH=arm64; SHELLCHECK_ARCH=aarch64 ;;
	x86_64|amd64) ARCH=amd64; SHELLCHECK_ARCH=x86_64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p "${TOOLS_DIR}"

download_binary() {
	local destination="$1"
	local url="$2"
	if [[ ! -x "${destination}" ]]; then
		curl --fail --location --retry 3 --silent --show-error "${url}" -o "${destination}"
		chmod 0755 "${destination}"
	fi
}

download_binary "${TOOLS_DIR}/kubectl" \
	"https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"
download_binary "${TOOLS_DIR}/kind" \
	"https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${ARCH}"
download_binary "${TOOLS_DIR}/jq" \
	"https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-linux-${ARCH}"

if [[ ! -x "${TOOLS_DIR}/kubectl-cnpg" ]]; then
	cnpg_tmp=$(mktemp -d)
	trap 'rm -rf "${cnpg_tmp}"' EXIT
	curl --fail --location --retry 3 --silent --show-error \
		"https://github.com/cloudnative-pg/cloudnative-pg/releases/download/${CNPG_PLUGIN_VERSION}/kubectl-cnpg_${CNPG_PLUGIN_VERSION#v}_linux_${ARCH}.tar.gz" \
		| tar -xzf - -C "${cnpg_tmp}"
	install -m 0755 "$(find "${cnpg_tmp}" -type f -name kubectl-cnpg -print -quit)" "${TOOLS_DIR}/kubectl-cnpg"
fi

if [[ ! -x "${TOOLS_DIR}/shellcheck" ]]; then
	shellcheck_tmp=$(mktemp -d)
	trap 'rm -rf "${shellcheck_tmp}"' EXIT
	curl --fail --location --retry 3 --silent --show-error \
		"https://github.com/koalaman/shellcheck/releases/download/${SHELLCHECK_VERSION}/shellcheck-${SHELLCHECK_VERSION}.linux.${SHELLCHECK_ARCH}.tar.xz" \
		| tar -xJf - -C "${shellcheck_tmp}"
	install -m 0755 "$(find "${shellcheck_tmp}" -type f -name shellcheck -print -quit)" "${TOOLS_DIR}/shellcheck"
fi

if [[ -n "${GITHUB_PATH:-}" ]]; then
	printf '%s\n' "${TOOLS_DIR}" >>"${GITHUB_PATH}"
else
	echo "CI tools installed in ${TOOLS_DIR}; prepend this directory to PATH for subsequent commands"
fi
