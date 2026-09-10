#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=../test/e2e/versions.env
source "${ROOT_DIR}/test/e2e/versions.env"
ACT_RUNNER_IMAGE=${ACT_RUNNER_IMAGE:-catthehacker/ubuntu:${ACT_RUNNER_VERSION}}
ACT_ARTIFACT_DIR=${ACT_ARTIFACT_DIR:-${ROOT_DIR}/.artifacts/act}
mkdir -p "${ACT_ARTIFACT_DIR}"
act_args=(
	--network host
	--concurrent-jobs 1
	--artifact-server-addr 127.0.0.1
	--artifact-server-path "${ACT_ARTIFACT_DIR}"
	-P "ubuntu-24.04=${ACT_RUNNER_IMAGE}"
)
if ! command -v act >/dev/null 2>&1; then
	echo "act ${ACT_VERSION} is required" >&2
	exit 1
fi
if [[ "$(act --version)" != "act version ${ACT_VERSION#v}" ]]; then
	echo "act ${ACT_VERSION} is required; found $(act --version)" >&2
	exit 1
fi

act pull_request -W "${ROOT_DIR}/.github/workflows/ci.yml" \
	-e "${ROOT_DIR}/.github/act/pull_request.json" "${act_args[@]}" --list
act push -W "${ROOT_DIR}/.github/workflows/ci.yml" \
	-e "${ROOT_DIR}/.github/act/push.json" "${act_args[@]}" --list

# Selecting the aggregate job also runs all of its prerequisites, including
# the full E2E job. This validates both supported event shapes against the
# same checked-in Make targets used by GitHub-hosted runners.
act pull_request -W "${ROOT_DIR}/.github/workflows/ci.yml" \
	-e "${ROOT_DIR}/.github/act/pull_request.json" "${act_args[@]}" -j ci
act push -W "${ROOT_DIR}/.github/workflows/ci.yml" \
	-e "${ROOT_DIR}/.github/act/push.json" "${act_args[@]}" -j ci

# Exercise the aggregate job under act in both terminal states. The failed
# prerequisite must yield a completed failed aggregate job, not a skip.
act workflow_dispatch -W "${ROOT_DIR}/.github/workflows/aggregate-fixture.yml" \
	--input force_failure=false "${act_args[@]}" -j ci
if act workflow_dispatch -W "${ROOT_DIR}/.github/workflows/aggregate-fixture.yml" \
	--input force_failure=true "${act_args[@]}" -j ci; then
	echo "act aggregate failure fixture unexpectedly succeeded" >&2
	exit 1
fi

bash "${ROOT_DIR}/hack/test-aggregate-ci.sh"
