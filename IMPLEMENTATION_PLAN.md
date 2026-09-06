# Implementation plan

This plan describes the work required to turn the current non-reconciling
scaffold into a fully tested project. It follows the responsibilities and
constraints in [`DESIGN.md`](DESIGN.md) and the scenario sequence in
[`E2E_TEST_PLAN.md`](E2E_TEST_PLAN.md).

The work has two milestones:

1. Build functional source code that compiles and passes the complete ordered
   E2E suite.
2. Establish a solid GitHub project with required PR CI, documentation,
   dependency automation, and guarded Renovate auto-merge.

Release automation, image publishing, and production release management are
explicitly out of scope for both milestones.

## Engineering principles

- Treat `DESIGN.md` as the behavior contract. Do not add responsibilities that
  are not specified there.
- Use red-green-refactor TDD for each behavior: write a failing test, implement
  the smallest behavior that passes it, then refactor without changing the
  contract.
- Every defect discovered during development or E2E testing adds a regression
  test before the defect is fixed.
- Keep the controller idempotent, restart-safe, observable, and bounded under
  retries and partial failures.
- Keep the controller independent of direct PostgreSQL connections. PostgreSQL
  catalog queries belong only to the E2E test actor.
- Never place passwords, Vault tokens, target Secret data, or current dynamic
  usernames in controller state, logs, metrics, or test artifacts.
- Make all external interactions injectable so unit and integration tests do
  not require a live Kubernetes cluster or Vault instance.
- Follow a local-first rule: every meaningful CI check must have a local
  command, and GitHub Actions must orchestrate the same scripts and Makefile
  targets rather than contain untested CI-only logic.
- Do not introduce a dependency on GitHub OIDC, GitHub API authentication,
  cloud credentials, private registries, or GitHub-hosted secrets for build or
  test execution. The E2E suite uses public images, locally built images, and
  test-only credentials.

## Milestone 1: functional operator and complete E2E coverage

### 1. Baseline and acceptance matrix

Before implementing behavior:

1. Read and reconcile `DESIGN.md`, `DESIGN_MAPPING.md`, and
   `E2E_TEST_PLAN.md`.
2. Convert each design requirement into an acceptance matrix covering:
   - watcher and predicate behavior;
   - Kubernetes API reads, patches, and permissions;
   - Vault issuance and revocation;
   - state transitions and restart points;
   - timing, retries, and cleanup;
   - logging and secret-redaction rules.
3. Record the intended test for every matrix row.
4. Resolve implementation ambiguities in the design documents before coding.

The responsibility boundary must remain explicit:

- watch CNPG `Cluster` and PostgreSQL `Pod` resources in approved namespaces;
- do not watch Kubernetes `Event` or `Node` resources;
- do not create, own, or lifecycle-manage CNPG `Cluster` resources;
- do not connect directly to PostgreSQL;
- use the Kubernetes Pod proxy and instance-manager `/pg/status` for controller
  health verification;
- use Vault for dynamic credentials and lease revocation;
- patch target Secrets without reading, listing, creating, updating, or
  deleting them.

### 2. Test foundation first

Build reusable test infrastructure before reconciliation behavior:

- CNPG `Cluster`, Pod, Secret, and state-Secret fixture builders;
- fake Kubernetes clients and API servers that can assert exact verbs and
  resources;
- a controllable clock and bounded polling/retry helpers;
- an interface-backed fake Vault HTTP server;
- Vault response fixtures for success, malformed responses, short leases,
  timeouts, retries, and revoke failures;
- `/pg/status` response fixtures;
- redacted logging and artifact helpers;
- E2E SQL helpers for the test actor only.

Tests must be deterministic and must not depend on arbitrary sleeps. Production
timers should use injected clocks where practical; E2E waits should use
bounded polling with diagnostics.

### 3. Configuration, logging, and health

Implement and test:

- `WATCH_NAMESPACE` parsing, normalization, and validation;
- Vault address, token, HTTP policy, and request timeout configuration;
- state Secret configuration;
- lease and verification timing settings;
- retry and backoff settings;
- structured startup and reconciliation logs;
- redaction of passwords, usernames, tokens, and lease responses;
- `/healthz`, `/readyz`, and metrics endpoints;
- safe startup failure for invalid or unsafe configuration.

Use the standard controller-runtime, client-go, and Go process/runtime metrics
for generic reconciliation, workqueue, Kubernetes API, and process behavior.
Implement only the domain metrics required by the design: rotation outcomes,
Vault operation outcomes, pending workflow phase, and per-replica lease time to
expiration. The lease metric must identify the namespace and Cluster, but must
not expose lease IDs, usernames, passwords, Secret names, Pod UIDs, event
fingerprints, or Vault paths. Calculate time to expiration at scrape time so it
continues to decrease without a Kubernetes event.

Unit tests must cover empty and malformed values, duplicate namespaces,
insecure HTTP handling, defaults, log redaction, metric labels, metric cleanup,
and the absence of sensitive values from metric exposition.

### 4. Durable state model

Define a versioned state schema stored in the controller state Secret. It may
contain workflow metadata such as:

- Cluster UID and generation;
- observed topology fingerprint;
- current and pending lease IDs;
- issue and verification timestamps;
- workflow phase;
- retry metadata;
- absence and cleanup metadata.

It must never contain credential material.

Implement:

1. state loading and schema validation;
2. version migration handling;
3. optimistic state updates;
4. stale-state detection;
5. restart recovery;
6. cleanup of finalized entries.

Test crashes and restarts before and after every external mutation. Verify that
recovery does not issue duplicate credentials or lose the lease that must be
revoked.

### 5. Resource watches and event qualification

Configure controller-runtime watches for CNPG `Cluster` and PostgreSQL `Pod`
resources in the approved namespaces.

Implement predicates and observation logic for:

- replica-mode changes;
- relevant external-cluster changes;
- target Secret name changes;
- designated-primary changes;
- primary Pod UID changes;
- replacement Pod readiness;
- Cluster deletion and confirmed absence.

A Pod deletion alone must not trigger rotation. The controller must wait until
the replacement topology and designated primary are observable.

Tests must prove that:

- qualifying observations enqueue reconciliation;
- irrelevant updates do not;
- repeated observations are idempotent;
- resources outside the watch list are ignored;
- no Kubernetes Event or Node informer is created;
- a Cluster name remains a stable identity when its role changes.

### 6. Vault client

Implement a narrow Vault client interface for:

- dynamic database credential issuance;
- lease duration validation;
- minimum lease and safety-margin checks;
- lease revocation;
- request timeouts and bounded retries;
- response validation and redaction.

Derive the Vault database role/path from the selected source CNPG Cluster name,
as specified by the design.

Test successful issuance, malformed responses, short leases, HTTP failures,
timeouts, retry behavior, duplicate requests, and revoke failures.

### 7. Rotation state machine

Implement the ordered workflow:

1. Read and validate the current Cluster.
2. Confirm replica mode and the current source relationship.
3. Query `/pg/status` through the Kubernetes API.
4. Issue a new Vault credential.
5. Persist the pending lease before mutating Kubernetes resources.
6. Patch the target Secret password using Secret `patch` only.
7. Patch the CNPG external-cluster username last.
8. Re-check WAL receiver status.
9. Revoke the old lease.
10. Persist finalized state.

The implementation must:

- never read target Secret data;
- never patch the target Secret username;
- never mutate a standalone primary;
- never revoke a lease still needed by the active topology;
- recover safely from partial failures;
- avoid duplicate issuance for one topology fingerprint;
- use timed requeues instead of blocking sleeps;
- keep retries bounded and observable.

Add unit and integration tests for every state transition, retry path, and
failure boundary.

### 8. Cleanup and namespace safety

Implement:

- confirmed Cluster absence tracking;
- the configured orphan sweep;
- two-consecutive-absence confirmation;
- lease revocation for deleted replica relationships;
- state entry cleanup;
- safe behavior when a namespace is removed from this controller but remains
  watched by CNPG.

The controller must not clean up resources solely because a namespace is
temporarily absent from its own watch list.

Test deletion races, stale-cache observations, reappearance before cleanup,
Vault outage during cleanup, and restart during cleanup.

### 9. Complete the E2E environment

The E2E harness must provision from a clean host:

- two Kind clusters, `k8s-us` and `k8s-eu`;
- one control-plane node and three tainted PostgreSQL workers per cluster;
- PostgreSQL 18;
- two CNPG instances per Cluster;
- the selected released CNPG version;
- one plain-HTTP dev-mode Vault in `us`;
- one regional controller per cluster;
- one minimal ephemeral Prometheus observer per cluster;
- host-network PostgreSQL gateways;
- no database Cluster resources during infrastructure setup.

The harness must copy useful patterns from upstream CNPG playground without
using that repository as a runtime dependency.

Use one run-wide monotonically increasing Cluster counter:

- `db01` in `us`, `db02` in `eu`;
- `db03` in `eu`, `db04` in `us`;
- `db05`, `db06`, and onward for rebuilt relationships;
- names do not encode role or region and are never reused during a run.

The fixture must create the Vault management account with SQL and keep it
outside CNPG declarative account management. This prevents CNPG from
reconciling a password that Vault has rotated.

### 10. Ordered E2E acceptance suite

Implement the ordered scenarios from `E2E_TEST_PLAN.md`:

1. Initial dynamic issuance and Secret/username patching.
2. Failover in a replica Cluster.
3. Designated-primary Pod deletion and replacement.
4. Cordon and drain of the primary's PostgreSQL worker.
5. Cross-region switchover and re-replication.
6. Promotion to standalone primary and cleanup of replica credentials.
7. Rebuilding replicas with newly allocated Cluster names.
8. Second namespace onboarding and replication.
9. Controller-only namespace removal while CNPG continues watching.
10. Failover without rotation while unwatched.
11. Namespace re-addition and resumed rotation.
12. Database deletion and dynamic lease cleanup.

The E2E test actor, not the controller, must query PostgreSQL catalog views to
verify:

- `pg_stat_replication` has a streaming connection;
- the replica primary reports `pg_is_in_recovery()` as true;
- `pg_stat_wal_receiver` reports an active streaming receiver;
- a WAL marker crosses the replication boundary.

Add negative and safety scenarios for invalid passwords, duplicate
observations, write-only Secret RBAC, controller restarts, and sensitive-value
leakage.

### 11. Milestone 1 completion gates

Milestone 1 is complete only when all of the following pass:

- `go build ./...`;
- formatting and static checks;
- unit and integration tests;
- race-enabled tests;
- manifest rendering and validation;
- shell-script validation;
- the complete ordered E2E suite from a clean environment;
- Kubernetes RBAC assertions;
- credential and token redaction assertions;
- custom metric and metric-redaction assertions;
- Prometheus target-health, metric-shipping, and metric-accuracy assertions;
- failure diagnostics and cleanup of only run-owned Kind clusters.

Every gate must be runnable from a local checkout. The implementation should
provide Makefile targets for formatting checks, unit/integration tests, race
tests, static analysis, manifest validation, image builds, and the complete
E2E suite. A developer must be able to reproduce the same commands used by CI
without GitHub credentials or a cloud account.

## Milestone 2: GitHub-ready project and CI artifacts

### 12. CI workflow definitions

Write GitHub Actions workflow definitions for:

- Go formatting;
- `go vet`;
- unit and integration tests;
- race-enabled tests;
- coverage reporting;
- manifest rendering and validation;
- shell syntax and shell linting;
- local Docker image builds;
- dependency and vulnerability scanning;
- the complete Kind/CNPG/Vault E2E suite.

Monitoring tests must scrape the controller metrics endpoint locally and
assert the custom rotation, Vault-operation, pending-workflow, and lease-time
series through a minimal per-cluster Prometheus observer. Do not install
dashboards, Alertmanager, Prometheus Operator, or a cross-cluster monitoring
stack. Direct exposition checks remain useful for diagnosing a Prometheus
shipping mismatch, and local alert-query tests are sufficient; dashboard tests
are not required.

The workflow must be a thin orchestration layer. Each job calls a checked-in
Makefile target or test script that can be run locally. Do not put important
build, test, filtering, cleanup, or artifact-redaction behavior only in YAML.

The workflow must not require:

- GitHub OIDC or cloud-provider authentication;
- GitHub API tokens;
- repository or environment secrets;
- access to a private image registry; or
- a GitHub-only service unavailable to `act`.

Local Docker, public dependency/image sources, and the test-only Vault token
are sufficient. Local image builds must be loaded directly into the Kind
clusters rather than pulled from a registry.

The E2E job must:

- run serially;
- install pinned tool versions;
- use explicit outer and phase timeouts;
- collect Kubernetes, CNPG, Vault, controller, and gateway diagnostics;
- upload only redacted artifacts;
- always delete the two run-owned Kind clusters.

Use workflow concurrency cancellation so obsolete PR runs do not consume
runner capacity.

The repository will contain the workflow definitions, but enabling Actions,
granting workflow permissions, selecting required checks, and enforcing those
checks on branches are administrator tasks described in `GITHUB_SETUP.md`.

### 13. Local execution of CI

Define local commands before finalizing the workflow, for example:

- `make verify` for formatting, vetting, unit/integration tests, and static
  checks;
- `make test-race` for race-enabled tests;
- `make manifests-check` for rendered Kubernetes resources;
- `make image-build` for all local images;
- `make e2e` for the complete clean-environment suite; and
- `make ci` for the complete non-E2E PR gate.

The GitHub workflow should invoke these same targets. Validate it locally with
`act` for both pull-request and push events using checked-in or generated local
event fixtures. Exercise the individual fast-test and E2E jobs with the same
Docker network mode intended for CI, and ensure the E2E cleanup trap works when
the job is interrupted or fails.

The local `act` run must be part of the implementation checklist for every
workflow change. Workflow syntax, action composition, environment variables,
Docker builds, test commands, artifact paths, and failure cleanup must all be
observable locally. CI caching may improve performance, but correctness must
not depend on a cache and cache misses must work under `act`.

GitHub branch protection, required-status enforcement, the Actions badge after
the workflow is enabled, and the final Renovate merge performed by GitHub
cannot be fully emulated by `act`. Keep those pieces declarative and minimal;
the administrator handoff document describes the later repository setup.

### 14. Repository preparation artifacts

Write and review:

- `CODEOWNERS`;
- documented local development and E2E commands;
- Makefile targets that match CI;
- a README describing scope, architecture, testing, E2E setup, and cleanup;
- a GitHub Actions test badge in the README; and
- `GITHUB_SETUP.md` with the administrator tasks required after this milestone.

Do not add a pull-request template. Do not configure branch protection, create
GitHub teams, enable repository Actions settings, install the Renovate app, or
grant repository automation permissions as part of the local implementation.

Do not add image publishing, release tags, changelog automation, or deployment
promotion workflows in this phase.

### 15. Renovate configuration

Configure Renovate to monitor every dependency source, including:

- Go modules and the Go toolchain;
- Docker base images;
- Vault image versions;
- CNPG versions;
- PostgreSQL test images;
- Kind, kubectl, and `kubectl-cnpg` versions;
- GitHub Actions;
- shell-script version variables;
- manifest image references.

Use explicit version declarations and Renovate custom managers for versions
stored in shell variables or other nonstandard locations. Avoid floating
`latest` tags. Pin GitHub Actions and update their commit pins through
Renovate.

Validate the configuration locally with Renovate's config validator or
equivalent offline validation. Confirm that every intended dependency source is
discoverable without requiring a Renovate token or GitHub API access.

### 16. Renovate auto-merge policy

Document the intended policy for later administrator configuration. Once the
Renovate app and repository permissions are installed, Renovate-created pull
requests may auto-merge only when:

- all required CI checks pass;
- the full E2E suite passes when relevant;
- the branch is current and conflict-free;
- dependency and security checks pass;
- the PR contains only dependency changes;
- repository branch protection requirements are satisfied.

Initially keep source, RBAC, design, and workflow changes out of auto-merge.
Patch, minor, and security updates should be the default auto-merge set.
Major updates should remain manual until the project has sufficient test
history; expanding auto-merge to major updates requires an explicit policy
change.

The policy itself is a local repository artifact. Installing Renovate,
granting it permissions, enabling auto-merge, and verifying an actual merged
Renovate pull request are administrator tasks and are not Milestone 2 local
completion gates.

### 17. Milestone 2 completion gates

Milestone 2 is complete when:

- the CI workflow definitions pass locally under `act` for the supported
  pull-request and push event fixtures;
- the same local Makefile targets pass outside `act`;
- the Renovate configuration exists and passes local validation;
- the README contains the prepared Actions test badge;
- `CODEOWNERS`, `MONITORING.md`, `E2E_TEST_PLAN.md`, and the other project
  documentation are complete;
- `GITHUB_SETUP.md` documents the administrator-owned GitHub setup steps;
- failed E2E runs retain useful redacted diagnostics locally; and
- no release or registry-publishing behavior has been introduced.

The following are deliberately not Milestone 2 completion gates because they
require GitHub administrator access:

- enabling or configuring repository Actions settings;
- creating GitHub teams and assigning repository permissions;
- configuring branch protection or required status checks;
- installing and authorizing the Renovate app;
- enabling repository auto-merge; and
- merging a real Renovate pull request.

## Definition of done

The local implementation is done when the source satisfies the design
contract, the complete ordered E2E suite passes repeatedly from a clean
environment, the CI workflows pass under `act`, the Renovate configuration is
locally validated, and the README, design/operations documents, `CODEOWNERS`,
and `GITHUB_SETUP.md` are complete.

GitHub-side enablement and enforcement are intentionally deferred to an
administrator following `GITHUB_SETUP.md`. This definition of done does not
claim that branch protection, teams, Actions settings, Renovate installation,
or Renovate auto-merge have been configured.