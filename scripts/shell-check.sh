#!/usr/bin/env bash
set -euo pipefail
mapfile -t scripts < <(rg --files -g '*.sh')
for script in "${scripts[@]}"; do bash -n "${script}"; done
shellcheck "${scripts[@]}"
