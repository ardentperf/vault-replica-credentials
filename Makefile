SHELL := /bin/bash
GOFLAGS ?= -buildvcs=false
export GOFLAGS

IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials:0.1.0
GATEWAY_IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials-gateway:0.1.0

.PHONY: all build build-gateway test test-integration test-race coverage fmt fmt-check vet static lint shell-check manifests manifests-check image-build docker-build docker-build-gateway dependency-check renovate-check verify ci e2e e2e-setup act-check

all: verify build

build:
	go build ./...

build-gateway:
	go build ./cmd/cnpg-test-gateway

test:
	go test ./...

test-integration: test

test-race:
	go test -race ./...

coverage:
	mkdir -p artifacts
	go test ./... -coverprofile=artifacts/coverage.out -covermode=atomic

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './vendor/*' -print)

fmt-check:
	@test -z "$$(gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*' -print))" || { echo 'Go files need gofmt; run make fmt' >&2; exit 1; }

vet:
	go vet ./...

static: vet
lint: fmt-check static

shell-check:
	bash hack/shell-check.sh

manifests:
	kubectl kustomize config

manifests-check:
	bash hack/manifests-check.sh

image-build:
	docker build --tag $(IMAGE) .
	docker build --file Dockerfile.gateway --tag $(GATEWAY_IMAGE) .

docker-build: image-build

docker-build-gateway:
	docker build --file Dockerfile.gateway --tag $(GATEWAY_IMAGE) .

dependency-check:
	go mod verify
	@if command -v govulncheck >/dev/null 2>&1; then govulncheck ./...; else echo 'govulncheck not installed; go mod verify completed'; fi

renovate-check:
	bash hack/renovate-check.sh

verify: fmt-check static test shell-check manifests-check dependency-check renovate-check

ci: verify coverage image-build

e2e-setup:
	bash test/e2e/setup.sh

e2e:
	bash test/e2e/run.sh

act-check:
	bash hack/act-check.sh
