#!/usr/bin/env bash
set -euo pipefail

results=${1:-}
if [[ -z "${results}" ]]; then
	echo "aggregate job results JSON is required" >&2
	exit 2
fi

if ! jq -e 'length > 0 and all(.[]; .result == "success")' <<<"${results}" >/dev/null; then
	echo "one or more required CI jobs did not succeed" >&2
	jq -r 'to_entries[] | "\(.key): \(.value.result)"' <<<"${results}" >&2
	exit 1
fi

echo "all required CI jobs succeeded"
