#!/usr/bin/env bash
set -euo pipefail

# Renovate discovers these explicit pins through renovate.json. Keep the
# versions aligned with the selected released CNPG baseline in setup.sh.
KIND_VERSION="v0.31.0"
KUBECTL_VERSION="v1.34.2"
KUBECTL_CNPG_VERSION="v1.30.0"

architecture=$(uname -m)
case "${architecture}" in
	x86_64) architecture=amd64 ;;
	aarch64|arm64) architecture=arm64 ;;
	*) echo "unsupported architecture: ${architecture}" >&2; exit 1 ;;
esac

install_dir="${E2E_TOOL_DIR:-${HOME}/.local/bin}"
mkdir -p "${install_dir}"
export PATH="${install_dir}:${PATH}"

curl --fail --location --silent --show-error \
	"https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${architecture}" \
	-o "${install_dir}/kind"
chmod 0755 "${install_dir}/kind"

curl --fail --location --silent --show-error \
	"https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${architecture}/kubectl" \
	-o "${install_dir}/kubectl"
chmod 0755 "${install_dir}/kubectl"

GOBIN="${install_dir}" go install "github.com/cloudnative-pg/cloudnative-pg/cmd/kubectl-cnpg@${KUBECTL_CNPG_VERSION}"

kind version
kubectl version --client=true
kubectl cnpg version
