#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }

rendered=$(mktemp)
trap 'rm -f "${rendered}"' EXIT
kubectl kustomize "${ROOT_DIR}/config" >"${rendered}"
test -s "${rendered}"
# Kubeconform performs strict, offline validation for built-in resources. The
# released CNPG CRD is installed and exercised against a live API in E2E, so
# its custom resource is intentionally ignored by this schema-only command.
docker run --rm --volume "${rendered}:/work/rendered.yaml:ro" \
	ghcr.io/yannh/kubeconform:v0.7.0 -strict -ignore-missing-schemas /work/rendered.yaml
