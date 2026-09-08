#!/usr/bin/env bash
set -euo pipefail
export PATH="${PWD}/.tools:${PATH}"
# shellcheck source=test/e2e/versions.sh
source test/e2e/versions.sh
mkdir -p artifacts/act
act workflow_dispatch -W .github/workflows/aggregate-fixture.yml --input outcome=success --network bridge --rm -P "ubuntu-24.04=${ACT_RUNNER_IMAGE}" >artifacts/act/aggregate-success.log 2>&1
if act workflow_dispatch -W .github/workflows/aggregate-fixture.yml --input outcome=failure --network bridge --rm -P "ubuntu-24.04=${ACT_RUNNER_IMAGE}" >artifacts/act/aggregate-failure.log 2>&1; then
  echo "Aggregate incorrectly accepted failed dependency" >&2
  exit 1
fi
rg -q 'Failure.*aggregate|Failure.*Evaluate' artifacts/act/aggregate-failure.log
