#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
KUBECONFORM_VERSION=${KUBECONFORM_VERSION:-v0.8.0}
rendered=$(mktemp "${TMPDIR:-/tmp}/vault-replica-manifests.XXXXXX.yaml")
trap 'rm -f "${rendered}"' EXIT

kubectl kustomize "${ROOT_DIR}/config" >"${rendered}"
test -s "${rendered}"
go run "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}" \
	-strict -summary "${rendered}"

# Contract assertions catch permission broadening that schema validation cannot.
target_role="${ROOT_DIR}/config/rbac/target-role.yaml"
secret_verbs=$(awk '/resources: \["secrets"\]/{getline; print}' "${target_role}")
if [[ "${secret_verbs}" != *'verbs: ["patch"]'* ]]; then
	echo "target Secret RBAC must contain patch only" >&2
	exit 1
fi
grep -Fq 'resources: ["pods/proxy"]' "${target_role}"
grep -Fq 'verbs: ["patch"]' "${target_role}"
if grep -RFq 'kind: ClusterRoleBinding' "${ROOT_DIR}/config"; then
	echo "cluster-wide binding is forbidden" >&2
	exit 1
fi
