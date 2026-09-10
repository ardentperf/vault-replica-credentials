#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
pass='{"validate":{"result":"success"},"race":{"result":"success"},"e2e":{"result":"success"}}'
fail='{"validate":{"result":"success"},"race":{"result":"failure"},"e2e":{"result":"skipped"}}'

bash "${ROOT_DIR}/hack/aggregate-ci.sh" "${pass}" >/dev/null
if bash "${ROOT_DIR}/hack/aggregate-ci.sh" "${fail}" >/dev/null 2>&1; then
	echo "aggregate CI checker accepted failed/skipped dependencies" >&2
	exit 1
fi
