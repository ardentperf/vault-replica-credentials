#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=test/e2e/versions.sh
source test/e2e/versions.sh
docker run --rm --entrypoint promtool -v "${PWD}/test/monitoring:/tests:ro" -w /tests "${PROMETHEUS_IMAGE}" test rules alerts_test.yml
