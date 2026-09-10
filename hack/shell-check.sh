#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# renovate: datasource=docker depName=koalaman/shellcheck
SHELLCHECK_VERSION=v0.11.0
mapfile -t scripts < <(find "${ROOT_DIR}" -type f -name '*.sh' -not -path '*/.git/*' | sort)

for script in "${scripts[@]}"; do
	bash -n "${script}"
done

if command -v shellcheck >/dev/null 2>&1; then
	shellcheck --severity=warning "${scripts[@]}"
elif command -v docker >/dev/null 2>&1; then
	docker run --rm -v "${ROOT_DIR}:${ROOT_DIR}:ro" -w "${ROOT_DIR}" \
		"koalaman/shellcheck:${SHELLCHECK_VERSION}" --severity=warning "${scripts[@]}"
else
	echo "shellcheck v0.11.0 or Docker is required" >&2
	exit 1
fi
