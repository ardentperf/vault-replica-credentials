#!/usr/bin/env bash
set -euo pipefail
export PATH="${PWD}/.tools:${PATH}"
# shellcheck source=test/e2e/versions.sh
source test/e2e/versions.sh
mkdir -p artifacts/act
for event in pull_request push; do
  act "${event}" -W .github/workflows/ci.yml -e ".act/${event}.json" --network bridge --bind --rm -P "ubuntu-24.04=${ACT_RUNNER_IMAGE}" >"artifacts/act/${event}.log" 2>&1
done
make aggregate-check
