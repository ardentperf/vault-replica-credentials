#!/usr/bin/env bash
set -euo pipefail
# renovate: datasource=docker depName=ghcr.io/renovatebot/renovate
RENOVATE_IMAGE=ghcr.io/renovatebot/renovate:44.69.8
docker run --rm -v "${PWD}:/work:ro" -w /work --entrypoint renovate-config-validator "${RENOVATE_IMAGE}" --strict renovate.json
# Local structural discovery checks complement Renovate's schema validator.
go test ./test/repository -run TestDependencyInventory -count=1
