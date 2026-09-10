SHELL := /bin/bash

GO ?= go
IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials:dev
GATEWAY_IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials-gateway:dev
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION ?= v1.1.4
# renovate: datasource=github-releases depName=rhysd/actionlint
ACTIONLINT_VERSION ?= v1.7.7
# renovate: datasource=github-releases depName=yannh/kubeconform
KUBECONFORM_VERSION ?= v0.8.0
# renovate: datasource=docker depName=renovate/renovate versioning=docker
RENOVATE_VERSION ?= 44.69.11
E2E_ARTIFACT_DIR ?= $(CURDIR)/.artifacts/e2e

.PHONY: all build build-gateway fmt fmt-check vet test test-integration test-coverage \
	test-race verify shell-check workflows-check manifests manifests-check \
	image-build docker-build docker-build-gateway vulnerability-check \
	renovate-validate ci e2e e2e-setup act-check

all: ci

build:
	$(GO) build ./...

build-gateway:
	$(GO) build ./cmd/cnpg-test-gateway

fmt:
	find . -type f -name '*.go' ! -path './.git/*' -exec gofmt -w {} +

fmt-check:
	@files=$$(find . -type f -name '*.go' ! -path './.git/*' -exec gofmt -l {} +); \
	if [[ -n "$$files" ]]; then echo "Go files require gofmt:"; echo "$$files"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-integration:
	$(GO) test -count=1 ./internal/controller ./internal/kubernetes ./internal/state ./internal/vault

test-coverage:
	@mkdir -p .artifacts
	$(GO) test -covermode=atomic -coverprofile=.artifacts/coverage.out ./...

test-race:
	$(GO) test -race ./...

verify: fmt-check vet test test-integration

shell-check:
	bash hack/shell-check.sh

workflows-check:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	bash hack/test-aggregate-ci.sh

manifests:
	kubectl kustomize config

manifests-check:
	KUBECONFORM_VERSION=$(KUBECONFORM_VERSION) bash hack/manifests-check.sh

docker-build:
	docker build --tag $(IMAGE) .

docker-build-gateway:
	docker build --file Dockerfile.gateway --tag $(GATEWAY_IMAGE) .

image-build: docker-build docker-build-gateway

vulnerability-check:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

renovate-validate:
	docker run --rm -i --entrypoint sh renovate/renovate:$(RENOVATE_VERSION) \
		-c 'cp /dev/stdin /tmp/renovate.json && renovate-config-validator /tmp/renovate.json' < renovate.json
	bash hack/check-renovate-coverage.sh

ci: verify test-race test-coverage manifests-check shell-check workflows-check image-build vulnerability-check renovate-validate

e2e-setup:
	E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_DIR) bash test/e2e/setup.sh

e2e:
	E2E_ARTIFACT_DIR=$(E2E_ARTIFACT_DIR) bash test/e2e/run.sh

act-check:
	bash hack/act-check.sh
