#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
if command -v renovate-config-validator >/dev/null 2>&1; then
	renovate-config-validator "${ROOT_DIR}/renovate.json"
	exit 0
fi
if command -v renovate >/dev/null 2>&1; then
	renovate-config-validator "${ROOT_DIR}/renovate.json"
	exit 0
fi
docker run --rm --entrypoint renovate-config-validator \
	--volume "${ROOT_DIR}:/work:ro" renovate/renovate:41.173.0 /work/renovate.json
