#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
mapfile -t scripts < <(find "${ROOT_DIR}/test" -type f -name '*.sh' -print | sort)
for script in "${scripts[@]}"; do
	bash -n "${script}"
done

if command -v shellcheck >/dev/null 2>&1; then
	shellcheck --severity=warning "${scripts[@]}"
else
	args=()
	for script in "${scripts[@]}"; do
		args+=("/work/${script#"${ROOT_DIR}/"}")
	done
	docker run --rm --entrypoint shellcheck --volume "${ROOT_DIR}:/work:ro" koalaman/shellcheck-alpine:v0.10.0 \
		--severity=warning "${args[@]}"
fi
