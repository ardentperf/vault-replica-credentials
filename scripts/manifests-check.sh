#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export PATH="${ROOT_DIR}/.tools:${PATH}"
kubectl kustomize "${ROOT_DIR}/config" | kubeconform -strict -summary -kubernetes-version 1.35.0
