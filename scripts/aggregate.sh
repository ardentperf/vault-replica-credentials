#!/usr/bin/env bash
set -euo pipefail
jq -e 'length > 0 and all(.[]; .result == "success")' <<<"${NEEDS_JSON:?required job results missing}"
