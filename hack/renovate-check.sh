#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
RENOVATE_IMAGE=${RENOVATE_IMAGE:-renovate/renovate:41.118.0}
docker run --rm -v "${ROOT_DIR}:/work" -w /work "${RENOVATE_IMAGE}" \
	renovate-config-validator --strict renovate.json
