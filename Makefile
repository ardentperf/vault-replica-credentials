SHELL := /bin/bash

IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials:dev
GATEWAY_IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials-gateway:dev
E2E_OUTER_TIMEOUT ?= 110m

.PHONY: all build build-gateway test test-race fmt fmt-check vet lint verify \
	static-check shell-check manifests-check image-build docker-build docker-build-gateway \
	e2e e2e-setup ci renovate-check workflow-check

all: verify build

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

fmt-check:
	@test -z "$$(gofmt -d $$(rg --files -g '*.go'))" || { gofmt -d $$(rg --files -g '*.go'); exit 1; }

vet:
	go vet ./...

static-check: vet

lint: fmt-check vet shell-check

shell-check:
	bash test/scripts/check-shell.sh

manifests-check:
	bash test/scripts/check-manifests.sh

verify: fmt-check vet test manifests-check shell-check

docker-build:
	docker build --tag $(IMAGE) .

docker-build-gateway:
	docker build --file Dockerfile.gateway --tag $(GATEWAY_IMAGE) .

image-build: docker-build docker-build-gateway

e2e-setup:
	bash test/e2e/setup.sh

e2e:
	timeout $(E2E_OUTER_TIMEOUT) bash test/e2e/run.sh

ci: verify test-race image-build renovate-check workflow-check

renovate-check:
	bash test/scripts/check-renovate.sh

workflow-check:
	bash test/scripts/check-workflows.sh
