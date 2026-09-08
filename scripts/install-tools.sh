#!/usr/bin/env bash
# Pin the same tools for a clean developer host and an Actions/act container.
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=test/e2e/versions.sh
source "${ROOT_DIR}/test/e2e/versions.sh"
TOOLS_DIR="${ROOT_DIR}/.tools"
mkdir -p "${TOOLS_DIR}"
arch=$(uname -m)
case "${arch}" in aarch64|arm64) arch=arm64 ;; x86_64) arch=amd64 ;; *) exit 1 ;; esac
act_arch="${arch}"
if [[ "${act_arch}" == amd64 ]]; then act_arch=x86_64; fi
curl -fsSL "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${arch}" -o "${TOOLS_DIR}/kind"
curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" -o "${TOOLS_DIR}/kubectl"
curl -fsSL "https://github.com/cloudnative-pg/cloudnative-pg/releases/download/${CNPG_VERSION}/kubectl-cnpg_${CNPG_VERSION#v}_linux_${arch}.tar.gz" -o "${TOOLS_DIR}/cnpg.tar.gz"
tar -C "${TOOLS_DIR}" -xzf "${TOOLS_DIR}/cnpg.tar.gz" kubectl-cnpg
curl -fsSL "https://github.com/nektos/act/releases/download/${ACT_VERSION}/act_Linux_${act_arch}.tar.gz" -o "${TOOLS_DIR}/act.tar.gz"
tar -C "${TOOLS_DIR}" -xzf "${TOOLS_DIR}/act.tar.gz" act
chmod +x "${TOOLS_DIR}/kind" "${TOOLS_DIR}/kubectl" "${TOOLS_DIR}/kubectl-cnpg" "${TOOLS_DIR}/act"
# renovate: datasource=go depName=github.com/yannh/kubeconform
KUBECONFORM_VERSION=v0.7.0
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION=v1.7.0
GOBIN="${TOOLS_DIR}" go install "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}"
GOBIN="${TOOLS_DIR}" go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
if ! command -v shellcheck >/dev/null || ! command -v rg >/dev/null; then
  sudo apt-get update -qq
  sudo apt-get install -y shellcheck ripgrep
fi
echo "Pinned tools installed in ${TOOLS_DIR}; add this directory to PATH."
