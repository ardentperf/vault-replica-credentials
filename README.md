# Vault replica credentials controller

[![CI](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml/badge.svg)](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml)

This repository contains an independent Kubernetes controller that runs in
`cnpg-system` beside CloudNativePG. It rotates dynamic PostgreSQL streaming
credentials from Vault when a watched replica topology changes. The controller
does not create or own CloudNativePG `Cluster` resources, connect to
PostgreSQL, watch target credential Secrets, or read target Secret data.

The implementation follows [DESIGN.md](DESIGN.md), with the traceability
inventory in [DESIGN_MAPPING.md](DESIGN_MAPPING.md). Vault passwords remain in
process memory only until the controller performs the write-only Secret patch;
the durable state Secret contains lease/workflow metadata but no passwords.

The detailed E2E design is in [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md). It uses
patterns from the upstream CloudNativePG playground but provisions its own two
Kind clusters and does not depend on a playground checkout.

The initial monitoring contract is in [MONITORING.md](MONITORING.md). It uses
standard controller-runtime/client-go metrics where available and adds only a
small set of domain metrics for rotations, Vault operations, pending workflows,
and per-Cluster lease time to expiration.

The repository preparation and administrator handoff are in
[GITHUB_SETUP.md](GITHUB_SETUP.md). Release publishing and production release
management are intentionally out of scope.

## Development

```sh
make verify       # format, vet, tests, shell, manifests, dependencies
make test-race
make coverage
make image-build
make ci           # complete non-E2E CI gate
```

Go 1.26 and Docker are required for the normal local commands. `make
manifests-check` renders and validates the Kustomize installation without a
cluster. `make shell-check` uses `bash -n` and runs ShellCheck when installed.

The controller expects a bounded comma-separated `WATCH_NAMESPACE`, a TLS
validated `VAULT_ADDR`, and `VAULT_TOKEN`. Plain HTTP is accepted only when
`VAULT_ALLOW_INSECURE_HTTP=true`, which is reserved for the ephemeral E2E
Vault. The state Secret must be pre-created in `cnpg-system`.

## End-to-end environment

`make e2e` provisions two clean, run-owned Kind clusters (`k8s-us` and
`k8s-eu`), released CloudNativePG, one ephemeral Vault, local controller and
gateway images, and one minimal Prometheus observer per region. It then runs
the ordered scenarios in [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md). Only the two
explicit Kind cluster names are removed during teardown. On failure, logs,
resource snapshots, Prometheus evidence, and rendered configuration remain in
`artifacts/e2e` after sensitive-value scrubbing.

For infrastructure inspection without the scenario runner use `make
e2e-setup`; setting `KEEP_E2E_CLUSTERS=true` retains the two run-owned
clusters. The harness never requires a CloudNativePG playground checkout.

The example manifests use `reporting` as one approved namespace. Copy the
target Role and RoleBinding for each namespace in the same namespace list used
by the CloudNativePG controller. The target credential Secret and the
`cnpg-system` state Secret are provisioned separately; this operator has no
permission to create, read, list, update, or delete target Secrets.
