# Vault replica credentials controller

[![tests](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml/badge.svg)](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml)

This repository contains a namespace-scoped Kubernetes controller that rotates
the Vault-issued PostgreSQL streaming credentials used by CloudNativePG replica
clusters. Rotation is coupled to an existing disruptive topology event—initial
bootstrap, primary failover, Pod replacement, or switchover—and is not a
periodic password-renewal loop.

The controller watches CNPG `Cluster` objects and their instance Pods in an
explicit `WATCH_NAMESPACE` list, gets credentials from the Vault database
secrets engine, blind-patches only the referenced Secret password, patches the
selected external-cluster username last, and verifies the WAL receiver through
Kubernetes `pods/proxy` before revoking the old lease. It never reads or
watches target Secret data, connects to PostgreSQL, watches Nodes or Events,
owns a CNPG Cluster, or uses cluster-wide RBAC.

## Architecture and safety

One active controller runs in `cnpg-system` per Kubernetes cluster with leader
election and one reconciliation worker. A pre-created Secret named
`vault-replica-controller-state` stores the compact, password-free workflow
journal. Pending state is persisted before either Kubernetes credential
mutation, making every later stage restart-safe. Deleted relationships are
cleaned only after two fresh API-server absences on the default five-minute
sweep; removing a namespace from this controller's watch list is never treated
as deletion.

Target-namespace RBAC permits `patch` on Secrets but no `get`, `list`, `watch`,
`create`, `update`, or `delete`. Kubernetes cannot restrict this permission to
a dynamically referenced Secret or one JSON field, so installations with other
sensitive Secrets should add an admission policy that constrains the
controller's patches.

The complete contract is in [DESIGN.md](DESIGN.md), its implementation index is
[DESIGN_MAPPING.md](DESIGN_MAPPING.md), and operational metrics and alert
starting points are in [MONITORING.md](MONITORING.md).

## Installation configuration

Render the base manifests with:

```sh
kubectl kustomize config
```

Before starting the Deployment:

1. Pre-create `cnpg-system/vault-replica-controller-state` from
   `config/installation/state-secret.yaml`.
2. Pre-create `cnpg-system/vault-replica-controller-vault` with key `token`.
3. Copy the target Role and RoleBinding into every namespace named by
   `WATCH_NAMESPACE`.
4. Pre-create each CNPG target credential Secret. The controller will not
   create it.
5. Configure one Vault database role per source CNPG Cluster, using that
   Cluster name as the role name and a lease of at least `768h`.

`WATCH_NAMESPACE`, `VAULT_ADDR`, and `VAULT_TOKEN` are required. HTTP Vault
addresses are rejected unless `VAULT_ALLOW_INSECURE_HTTP=true`, which exists
for the ephemeral E2E Vault only. Timing, state, probe, retry, and worker
settings are documented alongside their defaults in
[DESIGN_MAPPING.md](DESIGN_MAPPING.md).

## Local development

The Make targets used by GitHub Actions are also the supported local checks:

```sh
make verify              # formatting, vet, unit and focused integration tests
make test-race           # race-enabled Go tests
make test-coverage       # writes .artifacts/coverage.out
make manifests-check     # render plus schema/RBAC validation
make shell-check         # bash syntax and shellcheck
make workflows-check     # actionlint and aggregate-job fixtures
make image-build         # controller and test-gateway images
make vulnerability-check
make renovate-validate
make ci                  # complete non-E2E gate
```

Run the GitHub workflow fixtures locally with pinned `act` after installing the
tools below:

```sh
bash test/e2e/install-tools.sh
make act-check
```

## Complete E2E suite

`make e2e` provisions two new four-node Kind clusters (`k8s-us` and `k8s-eu`),
the pinned CNPG release, PostgreSQL 18, one dev Vault, regional controllers,
gateways, and regional Prometheus observers. It then executes the ordered
initialization, failover, Pod replacement, drain, invalid-password,
cross-region switchover, promotion, rebuild, namespace-scope, deletion, RBAC,
metrics, and redaction scenarios from [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md).

```sh
bash test/e2e/install-tools.sh
make e2e
```

The runner refuses to reuse Kind clusters with its reserved names and deletes
only clusters it created. Diagnostics go to `.artifacts/e2e`; sensitive values
are redacted, and ephemeral kubeconfigs are removed after cleanup. Set
`KEEP_E2E_CLUSTERS=true` only for local debugging. `make e2e-setup` validates
only the clean no-database environment and then cleans it up unless the keep
flag is set.

GitHub repository settings, required-check setup, and Renovate app enablement
remain administrator-owned and are described in [GITHUB_SETUP.md](GITHUB_SETUP.md).
There is intentionally no release, image-publishing, or deployment-promotion
automation in this milestone.
