#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if ! command -v act >/dev/null 2>&1; then
	echo "act is not installed; fixtures are available under .github/act"
	exit 0
fi

ACT_PLATFORM="${ACT_PLATFORM:-catthehacker/ubuntu:act-24.04}"
if [[ -z "${ACT_CONTAINER_ARCHITECTURE:-}" ]]; then
	case "$(uname -m)" in
		aarch64|arm64) ACT_CONTAINER_ARCHITECTURE=linux/arm64 ;;
		x86_64|amd64) ACT_CONTAINER_ARCHITECTURE=linux/amd64 ;;
		*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
	esac
fi
ACT_ARGS=(--platform "ubuntu-24.04=${ACT_PLATFORM}" --container-architecture "${ACT_CONTAINER_ARCHITECTURE}" --pull=false --reuse)
ACT_ARGS+=(--env "PASSWORD_PROPAGATION_DELAY=${PASSWORD_PROPAGATION_DELAY:-2s}")
ACT_ARGS+=(--env "VERIFICATION_DELAY=${VERIFICATION_DELAY:-5s}")
ACT_ARGS+=(--env "ORPHAN_SWEEP_INTERVAL=${ORPHAN_SWEEP_INTERVAL:-5s}")
ACT_ARGS+=(--env ACT=true)
for event in pull_request push; do
	act "${event}" --eventpath "${ROOT_DIR}/.github/act/${event}.json" --job ci "${ACT_ARGS[@]}"
done

if act push --workflows "${ROOT_DIR}/.github/act/aggregate-failure-workflow.yml" \
	--eventpath "${ROOT_DIR}/.github/act/push.json" --job ci "${ACT_ARGS[@]}"; then
	echo "the intentionally failing aggregate fixture unexpectedly passed" >&2
	exit 1
else
	echo "the intentionally failing aggregate fixture failed as expected"
fi
