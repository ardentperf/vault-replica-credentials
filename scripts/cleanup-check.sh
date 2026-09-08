#!/usr/bin/env bash
# Exercise the real harness traps with only the Kind/Kubernetes boundaries
# replaced. No existing Docker or Kubernetes resources are touched.
set -euo pipefail
VRC_CLEANUP_TEST_DIR=$(mktemp -d)
export VRC_CLEANUP_TEST_DIR
# A root act job can leave an unreadable generated runner in the bind mount.
# A stale non-object cache also exercises this failure when the test is root.
mkdir -p .tools
printf 'stale generated runner cache\n' >"${VRC_CLEANUP_TEST_DIR}/stale-runner"
install -m 000 "${VRC_CLEANUP_TEST_DIR}/stale-runner" .tools/e2e-runner
kind() {
  case "${1:-} ${2:-}" in
    'get clusters')
      if [[ "${VRC_CLEANUP_MODE}" == inventory_failure ]]; then return 1; fi
      if [[ "${VRC_CLEANUP_MODE}" == cleanup_inventory_failure && -e "${VRC_CLEANUP_TEST_DIR}/deleted-${VRC_CLEANUP_MODE}" ]]; then return 1; fi
      if [[ "${VRC_CLEANUP_MODE}" == existing ]]; then echo k8s-us; fi
      return 0 ;;
    'create cluster')
      if [[ "${VRC_CLEANUP_MODE}" == interrupt ]]; then kill -TERM "$$"; else return 1; fi
      ;;
    'delete cluster') printf '%s\n' "$4" >>"${VRC_CLEANUP_TEST_DIR}/deleted-${VRC_CLEANUP_MODE}" ;;
    *) return 0 ;;
  esac
}
kubectl() { echo '{}'; }
docker() { echo 'fixture'; }
export -f kind kubectl docker
for VRC_CLEANUP_MODE in interrupt failure inventory_failure existing cleanup_inventory_failure; do
  export VRC_CLEANUP_MODE
  export E2E_ARTIFACT_DIR="${VRC_CLEANUP_TEST_DIR}/${VRC_CLEANUP_MODE}"
  set +e
  bash test/e2e/setup.sh >"${VRC_CLEANUP_TEST_DIR}/${VRC_CLEANUP_MODE}.log" 2>&1
  result=$?
  set -e
  if [[ "${VRC_CLEANUP_MODE}" == interrupt ]]; then test "${result}" -eq 143; else test "${result}" -eq 1; fi
  if [[ "${VRC_CLEANUP_MODE}" == inventory_failure || "${VRC_CLEANUP_MODE}" == existing ]]; then
    test ! -e "${VRC_CLEANUP_TEST_DIR}/deleted-${VRC_CLEANUP_MODE}"
  elif [[ "${VRC_CLEANUP_MODE}" == cleanup_inventory_failure ]]; then
    rg -q 'failed to remove run-owned cluster k8s-us' "${VRC_CLEANUP_TEST_DIR}/${VRC_CLEANUP_MODE}.log"
  else
    test "$(<"${VRC_CLEANUP_TEST_DIR}/deleted-${VRC_CLEANUP_MODE}")" = k8s-us
  fi
done
test "$(stat -c '%a' .tools/e2e-runner)" = 755
echo "Interruption and failure traps deleted only their run-owned Kind fixture."
