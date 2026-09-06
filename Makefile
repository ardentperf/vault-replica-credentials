SHELL := /bin/bash

IMAGE ?= ghcr.io/ardentperf/vault-replica-credentials:dev

.PHONY: all build build-gateway test test-race fmt vet lint docker-build docker-build-gateway manifests e2e e2e-setup

all: test build

build:
	go build ./cmd/vault-replica-controller

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
	@echo "E2E plan: see E2E_TEST_PLAN.md."
	@echo "Use 'make e2e-setup' to provision the complete no-database environment."
	@echo "The ordered scenario runner will be enabled with reconciliation behavior."
	@echo "It must not require a cnpg-playground checkout at runtime."
