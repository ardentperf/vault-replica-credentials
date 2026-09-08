#!/usr/bin/env bash
# renovate: datasource=github-releases depName=cloudnative-pg/cloudnative-pg
CNPG_VERSION=v1.30.0
# renovate: datasource=docker depName=hashicorp/vault
VAULT_IMAGE=hashicorp/vault:1.20.4
# renovate: datasource=docker depName=ghcr.io/cloudnative-pg/postgresql
POSTGRES_IMAGE=ghcr.io/cloudnative-pg/postgresql:18.1-system-trixie
# renovate: datasource=docker depName=prom/prometheus
PROMETHEUS_IMAGE=prom/prometheus:v3.5.0
# renovate: datasource=docker depName=kindest/node
KIND_NODE_IMAGE=kindest/node:v1.35.0
# renovate: datasource=github-releases depName=kubernetes-sigs/kind
KIND_VERSION=v0.31.0
# renovate: datasource=github-releases depName=kubernetes/kubernetes
KUBECTL_VERSION=v1.35.0
# renovate: datasource=github-releases depName=nektos/act
ACT_VERSION=v0.2.89
# renovate: datasource=docker depName=catthehacker/ubuntu
ACT_RUNNER_IMAGE=catthehacker/ubuntu:act-24.04
export CNPG_VERSION VAULT_IMAGE POSTGRES_IMAGE PROMETHEUS_IMAGE KIND_NODE_IMAGE
export KIND_VERSION KUBECTL_VERSION ACT_VERSION ACT_RUNNER_IMAGE
