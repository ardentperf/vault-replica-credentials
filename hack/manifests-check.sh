#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
rendered=$(mktemp)
trap 'rm -f "${rendered}"' EXIT
kubectl kustomize "${ROOT_DIR}/config" >"${rendered}"
[[ -s "${rendered}" ]]
if command -v python3 >/dev/null 2>&1; then
	python3 - "${rendered}" <<'PY'
import sys
try:
    import yaml
except ImportError:
    sys.exit(0)
with open(sys.argv[1], encoding="utf-8") as stream:
    documents = list(yaml.safe_load_all(stream))
if not documents or any(document is None for document in documents):
    raise SystemExit("manifest rendering produced an empty document")
for document in documents:
    for field in ("apiVersion", "kind", "metadata"):
        if field not in document:
            raise SystemExit(f"manifest is missing {field}")
PY
fi
if grep -qE '(^|:)latest([" ]|$)' "${rendered}"; then
	echo "floating latest image tag found" >&2
	exit 1
fi
echo "manifest rendering and validation passed"
