#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export PATH="${ROOT_DIR}/.tools:${PATH}"
go mod verify
govulncheck ./...
