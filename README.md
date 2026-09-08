# Vault replica credentials

[![CI](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml/badge.svg)](https://github.com/ardentperf/vault-replica-credentials/actions/workflows/ci.yml)

A namespace-scoped Kubernetes controller rotates Vault dynamic PostgreSQL
credentials after a CloudNativePG replica primary changes or restarts. CNPG
continues to own the databases and replication configuration.

The controller watches CNPG Clusters and instance Pods, issues a credential
from `database/creds/<source-cluster-name>`, journals its lease, blindly patches
the referenced Secret's password, and patches the external-cluster username
after a five-second delay. Two minutes later it verifies the designated
primary's WAL receiver through Kubernetes `pods/proxy` and `/pg/status` before
revoking the old lease. It never connects to PostgreSQL or reads target Secrets.

The controller and its local E2E/CI harness are implemented. Completion-gate
evidence is in
[IMPLEMENTATION_STATUS.md](IMPLEMENTATION_STATUS.md). A checked-in workflow
does not imply that repository administration has been completed.

## Local development

Use Go 1.26.8, Docker, curl, jq, Bash, Make, and Git. Install the pinned test
tools, then run the same targets used by GitHub Actions:

```sh
make tools
export PATH="$PWD/.tools:$PATH"
make verify             # formatting check, vet, tests, build
make test-race
make manifests-check shell-check
make image-build
make security renovate-check
make ci                 # complete non-E2E gate, including coverage
make e2e                # complete ordered two-region suite
make act-check          # PR, push, and aggregate pass/fail fixtures
```

Run `make act-check` serially and wait for teardown before running other Go
targets in the checkout. Its root job bind-mounts the workspace and keeps live
artifacts private until cleanup exposes the redacted results.

`make fmt` formats Go files; `make fmt-check` only checks them. HTTP protocol
tests and controller-runtime fake-client tests run with `make test` and need
no credentials, running API server, or Vault instance. `make tools` installs
Kind, kubectl, the CNPG plugin, act, and validation tools in `.tools`; its
package-manager fallback installs shellcheck/ripgrep when absent. Renovate
validation uses a pinned public container.

## Installation and configuration

The example Kustomization in `config` expects `cnpg-system` and the target
namespace `reporting` to exist. Provision the state Secret separately with
`config/installation/state-secret.yaml`, and supply a Secret named
`vault-replica-controller-token` in `cnpg-system` with a `token` key. Provision
the password Secret in each target namespace through the database workflow.
Render with `kubectl kustomize config` and adapt the Deployment image locally.
There is no image publishing or release workflow in this project.

Set `WATCH_NAMESPACE` to the same explicit comma-separated namespace list as
CNPG. Copy the target Role and RoleBinding for every namespace. These grant
Secret `patch` only; Kubernetes cannot restrict that permission to a field or
a name prefix. Where other Secrets require additional protection, enforce the
target name and password-only patch through admission policy.

`VAULT_ADDR` must use HTTPS. `VAULT_TOKEN` is supplied through the environment
from the installation-owned token Secret. The token's lifecycle is an operator
prerequisite: this implementation does not renew or reauthenticate it. Plain
HTTP requires explicit `VAULT_ALLOW_INSECURE_HTTP=true` and is used only by the
ephemeral E2E fixture. Responses and authentication are not logged.

| Setting | Default |
| --- | --- |
| `VAULT_REQUEST_TIMEOUT` | `10s` |
| `VAULT_MIN_LEASE_DURATION` / `VAULT_LEASE_SAFETY_MARGIN` | `768h` / `24h` |
| `PASSWORD_PROPAGATION_DELAY` / `VERIFICATION_DELAY` | `5s` / `2m` |
| `ISSUE_STAGE_TIMEOUT` / `PASSWORD_USERNAME_STAGE_TIMEOUT` | `1m` / `1m` |
| `RECONNECT_STAGE_TIMEOUT` | `5m` |
| `RETRY_INITIAL_DELAY` / `RETRY_MAX_DELAY` | `1s` / `1m` |
| `ORPHAN_SWEEP_INTERVAL` / `ORPHAN_ABSENCE_SWEEPS` | `5m` / `2` |
| `STATE_MAX_BYTES` | `262144` decoded JSON bytes |
| `STATE_SECRET_NAME` / `STATE_SECRET_KEY` | `vault-replica-controller-state` / `state.json` |
| `WORKERS` / `LEADER_ELECTION` | `1` / `true` |
| `METRICS_BIND_ADDRESS` / `HEALTH_PROBE_BIND_ADDRESS` | `:8080` / `:8081` |

Configure Vault's database mount and per-source roles with effective default
and maximum TTLs of 768 hours, creation statements for LOGIN/REPLICATION users,
and revocation statements that remove those accounts. The database account
used by Vault must be provisioned outside CNPG declarative role management.
It must have enough authority for these statements; `CREATEROLE` alone cannot
grant PostgreSQL's REPLICATION attribute. The E2E fixture uses narrowly scoped
SQL functions for that privileged operation.

## Recovery and monitoring

The compact version 1 journal stores current and pending leases, expiration,
workflow stage, durable delays, and event identity. Passwords exist only in
process memory until the target PATCH is journaled. Restarting before that
boundary replaces the pending credential; restarting afterward resumes it.
Issuance and persistence cannot be atomic, so a crash between them can leave
an untracked lease bounded by its Vault TTL.

There is no scheduled rotation or lease renewal. Alert on approaching lease
expiration using [MONITORING.md](MONITORING.md). `/healthz` checks the process;
`/readyz` verifies that its configured state journal can be read and validated.

Promotion revokes obsolete replication leases and removes their state. Deleted
Clusters require two fresh sweep absences. Removing a namespace from this
controller's watch list preserves its state and leases; add it back to resume
processing. Never treat watch-list removal as deprovisioning.

## End-to-end environment

The self-contained runner creates only `k8s-us` and `k8s-eu`, refusing to reuse
existing clusters with those names. Each has a control plane and three tainted
PostgreSQL workers. It installs released CNPG 1.30.0, PostgreSQL 18, one dev
Vault in US, regional controllers, and two ephemeral Prometheus observers.
Host-network gateways provide cross-Kind database access without NodePorts.
No playground checkout, cloud account, private registry, or GitHub token is
needed. Allow sufficient Docker resources (the development host has 4 CPUs
and 16 GiB memory); the suite has a 110-minute outer timeout.

The actor provisions role-neutral `db01` through `db08` in order and checks
initial issuance, failover, Pod replacement, drain, distributed switchover,
promotion, rebuilds, namespace changes, restart safety, and deletion cleanup.
It independently queries PostgreSQL streaming views and sends WAL markers.
The controller never gains the test actor's SQL or Secret-read permissions.

Diagnostics go to `artifacts/e2e.*` after redaction. Kubeconfigs are kept outside
the artifact tree in a private temporary directory. On success, failure, or
interruption, the cleanup trap deletes only run-owned Kind clusters. For an
explicit debugging run, `KEEP_E2E_CLUSTERS=true make e2e-setup` retains them;
this option is never used by CI. `make e2e-setup` checks infrastructure without
creating database Clusters.

See [DESIGN.md](DESIGN.md), [ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md),
[E2E_TEST_PLAN.md](E2E_TEST_PLAN.md), and
[GITHUB_SETUP.md](GITHUB_SETUP.md) for the behavior contract, acceptance
mapping, scenario details, and administrator handoff.
