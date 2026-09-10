#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
workflow="${ROOT_DIR}/.github/workflows/ci.yml"
test -f "${workflow}"

if command -v actionlint >/dev/null 2>&1; then
	actionlint "${workflow}"
else
	docker run --rm --volume "${ROOT_DIR}:/work:ro" rhysd/actionlint:1.7.10 \
		/work/.github/workflows/ci.yml
fi
