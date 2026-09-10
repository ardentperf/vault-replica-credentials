# Vault replica credentials controller

![CI](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml/badge.svg)

This Kubernetes controller rotates the CloudNativePG streaming-replication
credential for a replica Cluster after a qualifying primary replacement,
restart, failover, or source-topology change. It is an external controller;
it neither creates nor manages CloudNativePG `Cluster` resources.

The normative contract is [DESIGN.md](DESIGN.md), with a line-item inventory
in [DESIGN_MAPPING.md](DESIGN_MAPPING.md). The operational metric contract is
[MONITORING.md](MONITORING.md), and the clean-host two-region acceptance suite
is defined in [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md).

## How it works

For an eligible replica primary, the controller issues a Vault database
credential using the selected source Cluster name as the Vault role. It writes
only a compact, password-free pending lease record to its state Secret, blind
patches the referenced Secret's password key, then patches the selected
external-cluster username. It checks the designated primary through the
Kubernetes `pods/proxy` `/pg/status` endpoint and commits the new lease only
after `isWalReceiverActive` is true and the previous lease is revoked.

The controller never reads, lists, watches, creates, updates, or deletes a
target credential Secret. It never connects to PostgreSQL; catalog checks
belong only to the E2E actor. Its only resource watches are CNPG `Cluster` and
Pod resources in the explicit `WATCH_NAMESPACE` list. The state Secret and
leader-election Lease remain in `cnpg-system`.

## Installation configuration

`WATCH_NAMESPACE`, `VAULT_ADDR`, and `VAULT_TOKEN` are required. Namespace
configuration is a bounded comma-separated list; empty, wildcard, malformed,
or duplicate values fail at startup. HTTPS is required unless the explicit
test-only `VAULT_ALLOW_INSECURE_HTTP=true` is set.

The deployment reads `VAULT_TOKEN` from the separately provisioned
`cnpg-system/vault-replica-controller-vault` Secret, key `token`. Do not put
that Secret or its value in Git. Pre-create the state Secret and apply one
copy of the target Role/RoleBinding for every namespace in `WATCH_NAMESPACE`.
The example manifests use `reporting`.

## Local development and gates

Install Go 1.26.4, Docker, `kubectl`, `shellcheck`, and a Renovate config
validator. The E2E gate additionally needs Kind and `kubectl cnpg`; the pinned
installer is [test/e2e/install-tools.sh](test/e2e/install-tools.sh).

```sh
make verify          # formatting, vet, unit/integration tests, manifests, shell lint
make test-race
make image-build
make renovate-check
make workflow-check
make ci              # all non-E2E PR checks
make e2e             # clean, ordered Kind/CNPG/Vault acceptance suite
```

`make e2e` owns only `k8s-us` and `k8s-eu`, refuses to reuse clusters of those
names, records redacted diagnostics under `test/e2e/artifacts`, and removes
only the clusters it created. Set `KEEP_E2E_CLUSTERS=true` only for local
failure investigation.

## Repository automation

The workflow at `.github/workflows/ci.yml` invokes the same Make targets.
Its aggregate `ci` job is the stable branch-protection and Renovate gate.
`renovate.json` covers Go, Dockerfiles, GitHub Actions, CNPG, Vault and pinned
E2E tools; patch/minor/security-style updates are declaratively eligible for
auto-merge while majors remain manual.

GitHub-side enablement, ownership mapping, branch protection, Renovate App
installation, and auto-merge permission are administrator actions documented
in [GITHUB_SETUP.md](GITHUB_SETUP.md). They are deliberately not performed by
this repository.
