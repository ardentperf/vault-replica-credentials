#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mapfile -t scripts < <(find "${ROOT_DIR}" -type f -name '*.sh' -not -path '*/vendor/*' -print)
for script in "${scripts[@]}"; do
	bash -n "${script}"
done
if command -v shellcheck >/dev/null 2>&1; then
	shellcheck --shell=bash --severity=warning "${scripts[@]}"
else
	echo "shellcheck is not installed; bash syntax checks passed"
fi
