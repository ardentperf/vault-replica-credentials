SHELL := /bin/bash

IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials:dev

.PHONY: all build build-gateway test test-race fmt fmt-check vet lint docker-build docker-build-gateway manifests e2e e2e-setup verify manifests-check shell-check image-build ci security renovate-check tools act-check aggregate-check monitoring-check

all: test build

build:
	go build ./...

build-gateway:
	go build ./cmd/cnpg-test-gateway

test:
	go test ./...

test-race:
	go test -race ./...

fmt:
	gofmt -w $$(rg --files -g '*.go')

vet:
	go vet ./...

lint: fmt vet

fmt-check:
	@test -z "$$(gofmt -l $$(rg --files -g '*.go'))" || { gofmt -l $$(rg --files -g '*.go'); exit 1; }

verify: fmt-check vet test build

tools:
	bash scripts/install-tools.sh

manifests-check:
	bash scripts/manifests-check.sh

shell-check:
	bash scripts/shell-check.sh
	bash scripts/cleanup-check.sh

image-build:
	$(MAKE) docker-build IMAGE=ghcr.io/ardentperf/vault-replica-credentials:e2e
	$(MAKE) docker-build-gateway GATEWAY_IMAGE=ghcr.io/ardentperf/vault-replica-credentials-gateway:e2e

security:
	bash scripts/security-check.sh

renovate-check:
	bash scripts/renovate-check.sh

monitoring-check:
	bash scripts/monitoring-check.sh

ci: verify test-race manifests-check shell-check image-build security renovate-check monitoring-check
	go test -coverprofile=coverage.out ./...

act-check:
	bash scripts/act-check.sh

aggregate-check:
	bash scripts/aggregate-check.sh

docker-build:
	docker build --tag $(IMAGE) .

GATEWAY_IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials-gateway:dev

docker-build-gateway:
	docker build --file Dockerfile.gateway --tag $(GATEWAY_IMAGE) .

manifests:
	kubectl apply --dry-run=client -k config

e2e-setup:
	bash test/e2e/setup.sh

e2e:
	timeout --signal=TERM --kill-after=90s 110m bash test/e2e/setup.sh --suite
