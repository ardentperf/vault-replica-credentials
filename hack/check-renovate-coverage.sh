#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
required_variables=(
	CNPG_VERSION POSTGRES_VERSION VAULT_VERSION PROMETHEUS_VERSION
	CURL_VERSION KIND_VERSION KUBECTL_VERSION CNPG_PLUGIN_VERSION ACT_VERSION
	KIND_NODE_VERSION ACT_RUNNER_VERSION GOVULNCHECK_VERSION ACTIONLINT_VERSION
	KUBECONFORM_VERSION RENOVATE_VERSION SHELLCHECK_VERSION
)
mapfile -t marker_files < <(find "${ROOT_DIR}/hack" -type f -print)
marker_files+=("${ROOT_DIR}/test/e2e/versions.env" "${ROOT_DIR}/Makefile")
for variable in "${required_variables[@]}"; do
	if ! awk -v variable="${variable}" '
		FNR == 1 { marker = 0 }
		/^# renovate: datasource=[^[:space:]]+ depName=[^[:space:]]+( versioning=[^[:space:]]+)?$/ {
			marker = 1
			next
		}
		marker {
			if ($0 ~ ("^" variable "([[:space:]]+[?]=[[:space:]]+|=)[^[:space:]]+")) {
				found = 1
			}
			marker = 0
		}
		END { exit(found ? 0 : 1) }
	' "${marker_files[@]}"; then
		echo "Renovate dependency marker is missing or detached from ${variable}" >&2
		exit 1
	fi
done

jq -e '
	.ignorePaths == [] and
	([.enabledManagers[]] | all(. as $manager | ["gomod", "dockerfile", "github-actions", "kubernetes", "custom.regex"] | index($manager))) and
	(["gomod", "dockerfile", "github-actions", "kubernetes", "custom.regex"] - .enabledManagers | length == 0) and
	(.customManagers | length >= 2)
' "${ROOT_DIR}/renovate.json" >/dev/null || {
	echo "Renovate managers or test-path extraction coverage is incomplete" >&2
	exit 1
}

if grep -REn 'uses: [^@[:space:]]+@(main|master|v?[0-9]+([.][0-9]+)*)[[:space:]]' \
	"${ROOT_DIR}/.github/workflows"; then
	echo "GitHub Actions must be pinned by commit SHA" >&2
	exit 1
fi

if grep -RInE ':(latest|main|master)(@|$|["[:space:]])' \
	"${ROOT_DIR}/Dockerfile" "${ROOT_DIR}/Dockerfile.gateway" \
	"${ROOT_DIR}/test/e2e" "${ROOT_DIR}/.github/workflows"; then
	echo "floating dependency reference detected" >&2
	exit 1
fi
