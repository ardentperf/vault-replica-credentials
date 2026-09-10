# End-to-end test plan: Vault dynamic replica credentials

## Status

The plan decisions are accepted and implemented by the controller, Vault
client, and ordered scenario runner in this repository. The scenario runner
is an E2E test actor: SQL catalog access and fixture mutations remain outside
the controller boundary.

The following choices are accepted for this plan: host-network gateways rather
than NodePorts; Vault roles derived from source CNPG Cluster names; a static
SCRAM management account with `CREATEROLE`; a dev-mode Vault root token; and
controller-only namespace removal while CNPG continues watching the namespace;
PostgreSQL 18; two instances per source/replica Cluster; Distributed Topology
from creation; and one ordered suite with checkpoints.

## Objective and test boundary

The suite validates the complete credential path used by a CloudNativePG
replica cluster:

1. A regional controller observes a qualifying CNPG event.
2. It obtains a dynamic PostgreSQL credential from Vault.
3. It blindly patches the password key in the Secret named by the target
   `Cluster` CRD.
4. It patches the external-cluster username in that `Cluster`.
5. CNPG reconnects streaming replication using the new pair.
6. The controller verifies WAL receiver activity, revokes the old lease, and
   commits the new lease to its state Secret.

The E2E focus is dynamic PostgreSQL credential issuance, write-only Secret
patching, username rotation, WAL verification, and Vault lease revocation. It
does not test:

- Vault authentication or production Vault policy design;
- TLS or certificate authentication for Vault, PostgreSQL, or replication;
- backups, object storage, monitoring, or unrelated CNPG features; or
- the controller creating, owning, configuring, or lifecycle-managing CNPG
  `Cluster` resources.

The test harness is allowed to read Secrets, query PostgreSQL, and mutate CRs
because it is test orchestration code. Those permissions must not be granted
to the controller ServiceAccount.

## Upstream reference and release pin

The setup patterns are taken from the clean upstream repository
[`cloudnative-pg/cnpg-playground`](https://github.com/cloudnative-pg/cnpg-playground)
at main commit
`1957b42b445532d284513964f53e3085b4f745f9`.

Patterns to copy conceptually are:

- `scripts/common.sh` for release variables, Kind naming, and prerequisites;
- `scripts/funcs_regions.sh` for region-to-cluster/context naming;
- `k8s/kind-cluster.yaml` for the control-plane/worker topology and PostgreSQL
  worker taints;
- `scripts/setup.sh` for creating multiple Kind clusters and labeling nodes;
- `demo/funcs_requirements.sh` for installing a released CNPG manifest and
  waiting for the operator; and
- `demo/setup.sh` and its templates for distributed replica topology and
  demotion/promotion sequencing.

The harness must copy the required logic into this repository. It must not
import, vendor, execute, mount, or require a `cnpg-playground` checkout at
runtime. It must also avoid taking the local playground checkout as an input.

### What the upstream playground actually connects

The upstream playground's cross-region path is object storage, not direct
PostgreSQL TCP streaming. `scripts/setup.sh` starts one RustFS container per
region, publishes a host port for operator access, and attaches each container
to the shared `kind` Docker network so the Kind nodes can resolve names such as
`objectstore-eu`. It distributes the object-store credentials to both
clusters. The generated Barman/Klio `externalClusters` entries reference those
object stores, and the recovery bootstrap uses the primary region's archived
WAL.

The upstream templates do not provide PostgreSQL
`externalClusters[].connectionParameters` for this path. Consequently, the
playground does not need a cross-Kind PostgreSQL endpoint, NodePort, or
host-network PostgreSQL gateway. That is appropriate for its backup/recovery
demonstration but cannot validate this operator's dynamic username/password
rotation for streaming replication. This plan retains the playground's Kind
and distributed-topology conventions, while adding a separate test-owned
PostgreSQL gateway only because the requested credential test requires direct
password-authenticated streaming.

The referenced upstream head currently sets `CNPG_VERSION=v1.30.0` in
`scripts/common.sh`. The E2E harness will use that latest released baseline
unless a newer released version has been selected by the time implementation
starts. The selected version must be a released CloudNativePG manifest, pinned
in the harness, and written to the test artifact directory. A trunk or source
build is not acceptable.

## Environment topology

The suite provisions and owns the complete test environment.

| Purpose | Value |
| --- | --- |
| Kind cluster for the US region | `k8s-us` |
| Kind context for the US region | `kind-k8s-us` |
| Kind cluster for the EU region | `k8s-eu` |
| Kind context for the EU region | `kind-k8s-eu` |
| Controller namespace in both clusters | `cnpg-system` |
| Single Vault location | US Kind cluster only |
| Initial no-database namespace | `e2e-bootstrap` |
| First database namespace | `e2e-first` |
| Second database namespace | `e2e-second` |

Each Kind cluster uses a simplified form of the upstream playground shape:

- one control-plane node;
- three PostgreSQL workers, labeled for PostgreSQL placement and tainted with
  `node-role.kubernetes.io/postgres:NoSchedule`.

The harness labels nodes with the regional topology keys and gives the three
PostgreSQL workers distinct regional zones. CNPG, Vault, this controller, and
the test gateways are placed on the control-plane node with the necessary
control-plane toleration. PostgreSQL workloads are placed on the tainted
workers. Resource requests must be sized so that the suite can run the
required source and replica instances without accidental eviction.

The harness creates `e2e-bootstrap` in both clusters before installing the
operators. It contains no CNPG `Cluster` resources. This gives the initial
bounded `WATCH_NAMESPACE` value a real namespace without starting the test
databases. The first real database namespace is added later by the test.

The harness uses a temporary shared kubeconfig and a per-run artifact
directory. It must install no resources into an existing user cluster and must
have a cleanup trap for both Kind clusters and the ephemeral Vault workload.

## Installation sequence

### 1. Preflight

Verify that the host has the required tools and that the run is isolated:

- Docker or the supported Kind container provider;
- `kind`, `kubectl`, and the `kubectl cnpg` plugin;
- Go and a local image build toolchain;
- `curl` and `jq` for control-plane checks; and
- the normal shell utilities used by the harness.

The harness records versions of Docker, Kind, Kubernetes, `kubectl`, the CNPG
plugin, Go, Prometheus, and the selected CNPG release. It creates a unique
temporary artifact directory and refuses to reuse an existing `k8s-us` or
`k8s-eu` cluster unless an explicit future debug mode is added.

### 2. Create the two Kind clusters

Create `k8s-us` and `k8s-eu` from the copied four-node Kind configuration. Use
the `kind-` context prefix and the `k8s-` cluster base name from the upstream
pattern. Set `KUBECONFIG` to the run-local shared file and verify both
contexts, all nodes, labels, taints, and cross-node scheduling.

No database `Cluster` resource is created in this phase. The harness asserts
that `kubectl --context <context> get clusters.postgresql.cnpg.io -A` returns
no test database objects after the infrastructure is ready.

### 3. Install CloudNativePG in both regions

Install the selected released CNPG manifest in each context. The preferred
rendering path is the released `kubectl cnpg install generate` flow with the
control-plane placement option and an explicit bounded watch namespace. If
the selected release requires its release URL instead, use the corresponding
official release manifest. Do not use the playground repository as the source
of the manifest.

The initial CNPG watch list is `e2e-bootstrap`. Wait for the CRDs, webhook,
controller Deployment, and the CNPG readiness condition in each cluster. The
harness records the rendered manifest and verifies the effective CNPG
`WATCH_NAMESPACE` configuration before continuing.

When namespaces are added or removed later, update CNPG using the selected
release's supported configuration mechanism (the generated install
configuration/ConfigMap path), roll out the CNPG controller, and verify the
effective namespace list. Do not depend on an undocumented Deployment-only
environment edit.

### 4. Install the single ephemeral Vault

Create one Vault namespace and one dev-mode Vault Pod/Deployment plus Service
in the US cluster. The deployment is intentionally ephemeral and has no
persistent volume. Use:

- plain HTTP only;
- no CA bundle and no TLS configuration;
- a test-only dev root token supplied through a Kubernetes Secret or equivalent
  test fixture; and
- one Vault instance and one Service, with readiness checked before database
  onboarding.

Vault auth is outside the scope of this suite. The controller configuration
uses the test token only to reach the dynamic database provider. The token and
all generated passwords must be redacted from logs and preserved artifacts.

Because the controller in `eu` must issue credentials for a source database in
either region, the harness must make Vault reachable from both Kind clusters.
The selected transport is a host-network listener on the US control-plane
node, addressed by that node's IP on the shared Docker/Kind network. Configure
both controllers with an explicit `http://<us-node-address>:<vault-host-port>`
address and run a health check from a temporary Pod in each region before
starting database tests. Kubernetes Service DNS names must not be used across
the two independent clusters. This avoids adding a NodePort that could be
mistaken for part of the PostgreSQL failover path.

The Vault Pod uses `hostNetwork: true`, is pinned to the US control-plane node,
and listens on a dedicated host port. A normal ClusterIP Service is still
created for in-cluster US access, but the EU controller uses the explicit US
node address. This remains one ephemeral Vault Pod and one Service in the US
cluster.

### 5. Build and install this operator

Build the local controller image, load it into both Kind clusters, and install
the ServiceAccount, `cnpg-system` state Secret, system Role/RoleBinding, and
target namespace Roles/RoleBindings. Start one operator Deployment in each
cluster with:

- `WATCH_NAMESPACE=e2e-bootstrap` initially;
- the same HTTP Vault address and test-only token;
- the selected Vault database role mapping/configuration;
- the agreed operational timing defaults from `DESIGN.md`; and
- one active worker with leader election enabled.

The two Deployments must be independently regional. The suite verifies that
each Deployment is Ready and that its logs contain only non-sensitive startup
configuration, including the bounded namespace set.

The target Role in every watched namespace grants `patch` on Secrets without
`resourceNames`. It grants no target Secret `get`, `list`, `watch`, `update`,
`create`, or `delete`. The harness explicitly checks this with
`kubectl auth can-i` for the controller ServiceAccount. The state Secret's
normal read/write permissions remain limited to `cnpg-system`.

### 6. Install minimal per-cluster Prometheus

Install one ephemeral Prometheus instance in each Kind cluster after the
regional controller is ready. This is a test observer, not part of the
operator's control loop. Each instance consists only of a pinned Prometheus
Deployment, short-retention ephemeral storage, a ConfigMap, and a ClusterIP
Service. Do not install Grafana, Alertmanager, Prometheus Operator, dashboards,
recording rules, or a cross-cluster monitoring stack.

Expose the controller's metrics port through a namespaced ClusterIP Service and
configure the local Prometheus instance to scrape only that Service at the
controller's `/metrics` endpoint. The scrape interval must be recorded by the
harness so metric-change tolerances are deterministic. Prometheus and its
image version must be pinned and Renovate-managed like the other E2E images.

The harness must wait for Prometheus readiness and verify through its HTTP API
that the local controller target is `up` before creating any database
Clusters. It must retain the target-health response, rendered scrape config,
and redacted metric-query evidence in the run artifacts. Prometheus access from
the test actor uses a temporary port-forward with distinct local ports for the
two clusters; no NodePort is introduced.

## Network and database access

Independent Kind clusters need explicit endpoints for both Vault and
cross-region PostgreSQL. The selected design uses a test-owned TCP gateway in
each source cluster. The gateway is a small `hostNetwork: true` Pod pinned to
the control-plane node and listens on a dedicated port on that node's Kind
Docker-network IP. It forwards to the source Cluster's stable CNPG `-rw`
Service inside that cluster.

The `-rw` Service is deliberately the backend boundary: CNPG updates its
endpoints as the primary changes, while the gateway's externally reachable
address and port stay constant. The gateway is not a CNPG resource and is not
owned or mutated by this controller. No NodePort is used for PostgreSQL.

The harness records an endpoint such as
`<source-node-address>:<gateway-host-port>`. It must first verify that Pods in
the remote Kind cluster can reach that address. If the selected Docker/Kind
network cannot route to a host-network listener, the harness must fail
preflight rather than silently switch to a NodePort.

The endpoint is used in both:

- the replica Cluster's `externalClusters[].connectionParameters.host/port`;
  and
- the Vault database connection configuration.

The harness validates connectivity from the remote region before creating a
replica Cluster. After every failover or switchover it rechecks that the
gateway still reaches the new primary. The test-owned gateway and internal
Services are not owned or mutated by this controller.

### Cross-Kind transport alternatives

The following alternatives were considered:

| Option | Benefit | Cost/risk | Decision |
| --- | --- | --- | --- |
| Host-network gateway to CNPG `-rw` Service | Stable external address; CNPG owns failover endpoint changes; no NodePort or CNPG Service mutation | Requires validating cross-cluster reachability to the Kind node IP and dedicating host ports | Selected |
| Docker-network relay plus `kubectl port-forward` | No Kubernetes exposure and easy to inspect from the host | Port-forwards must be restarted when the selected Pod changes; a stale relay can make failover look like an auth failure | Fallback only |
| Direct routed Pod CIDRs | Closest to direct cross-cluster PostgreSQL networking | Requires non-overlapping Pod CIDRs, explicit routes, and CNI-specific behavior; adds routing failure modes to the test | Rejected for this suite |
| Multi-cluster CNI/overlay | Most production-like for a real multi-cluster platform | Adds a large networking dependency and its own control plane to a credential-focused E2E test | Out of scope |
| NodePort | Simple and commonly supported by Kind | Adds per-cluster port allocation and another externally visible Service path around the failover tests | Not selected |

The host-network gateway must use one dedicated port per source database (or a
single gateway with explicit database routing). Its backend must resolve the
stable in-cluster `-rw` Service, not pin to a Pod IP. The same gateway pattern
can be used for source PostgreSQL in both regions; only Vault runs in `us`.

The controller's WAL verification remains through the Kubernetes API
`pods/proxy` subresource and the CNPG instance-manager `/pg/status` endpoint.
The test harness may use `kubectl cnpg psql`, a port-forward, or a temporary
SQL client for fixture setup and WAL markers; this does not add PostgreSQL
permissions to the controller.

## PostgreSQL and Vault fixtures

### Database topology

The first namespace starts with two independent replication flows, each with a
two-instance source Cluster and a two-instance cross-region replica Cluster.
Two instances provide a designated primary and one standby for failover,
replacement, and node-drain scenarios while keeping the test environment
small enough to run two flows in both regions.

| Logical flow | Initial source Cluster | Source region | Initial replica Cluster | Replica region |
| --- | --- | --- | --- | --- |
| `flow-a` | `db01` | `us` | `db02` | `eu` |
| `flow-b` | `db03` | `eu` | `db04` | `us` |

The names above illustrate allocation order: the harness allocates from one
monotonically increasing counter shared by both regions and all namespaces.
The first four Cluster objects are `db01`, `db02`, `db03`, and `db04`, followed
by `db05`, `db06`, and so on. Names do not encode source/replica role or
region, so a promotion does not make the identity stale. A Cluster keeps its
name for its lifetime; every replacement or newly added Cluster gets the next
unused name, and names are not reused during a run. Every Cluster's
external-cluster entry has a unique, explicit name and references its target
Secret by name and key. The target Secret name is deliberately not a
controller configuration value; it is read from that Cluster CRD by the
controller.

### Static management account

After each new source Cluster is Ready, the harness uses SQL to create a
dedicated static management account with a fixed test-only name and initial
password, for example `vault_replica_admin` and a generated per-run static
password. The account is created with password/SCRAM authentication and a
`CREATEROLE` grant sufficient for the Vault database plugin to create and
revoke `LOGIN` users with `REPLICATION`.

The account is deliberately created with SQL rather than through a CNPG
declarative account-management resource. If CNPG owns the account, it will
reconcile the declared password and can overwrite the password that Vault is
using or has rotated. SQL creation leaves this privileged account outside
CNPG's ownership while still giving Vault the static initial credentials it
needs to manage dynamic accounts.

The account's initial password is static for the lifetime of the fixture. The
suite does not test Vault static-role rotation of this management account. It
tests Vault's dynamic credentials created using this account.

The source Cluster configuration must permit password/SCRAM connections for
the management account and Vault-issued accounts, including replication
connections in `pg_hba.conf`. TLS is disabled for this test path and client
configuration uses password authentication; no certificate credentials are
created or mounted.

### Vault database engine

For every source Cluster, the harness configures the database secrets engine
with its explicit gateway connection and static management account. It then
creates a database role with:

- `default_ttl` and effective `max_ttl` of `768h`;
- creation SQL that produces a random `LOGIN` replication user with a SCRAM
  password; and
- revocation SQL that drops or disables that dynamic account cleanly.

The role's creation and revocation statements must be compatible with the
source PostgreSQL version, username limits, `pg_hba.conf`, and replication-slot
behavior. The harness waits for the database configuration and role to be
usable by issuing one disposable credential before it starts the corresponding
replica fixture, then revokes that disposable lease.

Multiple source Clusters mean multiple Vault database configurations and
roles. The E2E convention gives each role the source CNPG Cluster name:
`database/roles/<source-cluster-name>` is issued through
`database/creds/<source-cluster-name>`. Each target's selected source external
cluster entry uses that same source Cluster name, so the controller can
derive the role from the current source reference without a Secret-name-based
guess or an additional per-database mapping.

### Dummy initial credential

When each replica Cluster is created, the harness also creates its target
Opaque Secret with a dummy `password` value and writes a dummy username into
the selected `externalClusters[].connectionParameters.user`. The fixture does
not provide the actual Vault username or password to the Cluster.

The harness captures the dummy values, Cluster UID, target Secret name/key,
and source endpoint. It then waits for the region-local controller to issue a
credential, patch the Secret, patch the username, and allow CNPG to establish
streaming replication. The test actor may read the Secret to assert the
result, but the controller must remain write-only. The test actor also runs
the independent SQL catalog assertions described below; those assertions are
not delegated to CNPG status.

## Common assertions and evidence

Every rotation assertion collects a redacted snapshot containing:

- Cluster UID and resource generation;
- designated-primary Pod name, UID, node, and restart identity;
- current and pending lease fingerprints, stages, and expiry times from the
  state Secret;
- the external-cluster username and target Secret resource version;
- Vault lease status using the test token; and
- `/pg/status` with `isWalReceiverActive`; and
- source/replica SQL catalog-view results from the fixture actor.

Passwords and Vault tokens are never written to artifacts. Lease IDs may be
hashed in human-readable artifacts, while the test process retains exact IDs
only in memory for lookup/revocation assertions.

For a successful rotation, assert all of the following:

- the username differs from the dummy or previous username;
- the target Secret's decoded password differs from the dummy or previous
  password;
- the username and Secret password came from the same Vault response;
- `/pg/status` reports `isWalReceiverActive: true` for the designated primary;
- the source primary's `pg_stat_replication` row is in `streaming` state;
- the replica cluster's designated primary reports recovery mode and an active
  `pg_stat_wal_receiver` stream;
- a WAL marker written on the source is visible on the replica;
- the new lease is current in state; and
- the prior current lease is revoked after verification.

Also assert that a repeated observation of the same Pod/topology fingerprint
does not issue a second lease. The harness checks controller logs and Events
for absence of passwords, Secret data, and Vault tokens.

### Independent SQL replication assertions

The E2E test actor, never the controller, opens PostgreSQL connections through
the test gateway or a temporary port-forward using the fixture's known
credentials. It queries PostgreSQL catalog views directly so the replication
assertion is independent of CloudNativePG status and instance-manager signals.
The fixture must check the current topology dynamically, not infer roles from
Cluster names or regions.

For the current source primary, query `pg_stat_replication` and require the
connection for the replica cluster to be present with `state = 'streaming'`.
For the replica cluster's designated primary, require `pg_is_in_recovery()` to
be true and query `pg_stat_wal_receiver`, requiring an active receiver with
`status = 'streaming'`. The exact application-name and endpoint filters are
discovered from the current topology and not hard-coded to an initial role.

For each successful rotation and topology transition, write a uniquely tagged
WAL/data marker on the current source, wait for it to cross the stream, and
read it from the replica. The fixture records the source and replica LSNs or
equivalent catalog evidence and fails if the marker is not visible, even when
CNPG reports Ready or a status endpoint reports healthy. A small
PostgreSQL-version-aware query adapter preserves these invariants for
PostgreSQL 18 and later versions without depending on CNPG-specific status
fields.

These SQL checks are test-only assertions. No PostgreSQL DSN, catalog query,
or database credential is added to the controller; the controller continues
to verify only through the Kubernetes API and `pods/proxy`.

### Monitoring assertions

The E2E actor queries the regional Prometheus API through a temporary
port-forward. It also may scrape the controller endpoint directly when
diagnosing a mismatch. Metric semantics are validated from Prometheus query
results and the underlying exposition format; dashboards are not part of this
test.

For a successful rotation, assert that:

- `vault_replica_rotations_total` records one successful outcome;
- the corresponding Vault issue and old-lease revoke operations are counted;
- the pending workflow gauge eventually returns to zero for that Cluster's
  phase;
- `vault_replica_current_lease_time_to_expiration_seconds` identifies the
  correct namespace and Cluster and reports a positive remaining lifetime; and
- no metric sample or label contains a password, username, Secret name, Vault
  token, lease ID, Pod UID, or event fingerprint.

Induce a bounded Vault or verification failure and assert a failure counter and
pending workflow are observable without creating duplicate rotation metrics or
credential leases. The monitoring tests must also confirm that standard
controller-runtime/client-go metrics are reused rather than shadowed by custom
reconciliation, queue, or API metrics.

For metric shipping and accuracy, the fixture must:

1. query Prometheus `/api/v1/targets` and require the local controller target
   to remain healthy;
2. record counter values before a known action and require exactly the expected
   delta after Prometheus has scraped the result;
3. compare the lease time-to-expiration value for the identified Cluster with
   the Vault-issued lease duration and test-observed issue time, allowing only
   scrape and clock tolerance;
4. verify that the lease gauge decreases between scrapes without another
   Kubernetes event;
5. verify pending-workflow gauges become zero after the workflow completes;
6. verify cleanup removes the deleted Cluster's lease-time series; and
7. verify the same observations through both regional Prometheus instances.

Metric assertions must tolerate process counter resets during the deliberate
controller-restart scenarios by using Prometheus counter semantics or a fresh
baseline. They must fail on missing targets, stale samples, unexpected counter
increments, incorrect Cluster labels, or sensitive metric content.

## Ordered scenario suite

The following scenarios run serially because later cases intentionally depend
on the topology produced by earlier cases. Each phase has a checkpoint that
can be saved with the run artifacts. A failed phase stops the suite before it
performs destructive follow-on changes.

### Phase 0: full environment, no databases

1. Create the two Kind clusters and verify the node topology.
2. Create `e2e-bootstrap` in both clusters.
3. Install the released CNPG operator in both clusters with only
   `e2e-bootstrap` watched.
4. Install one HTTP dev-mode Vault in `us` and verify reachability from `us`
   and `eu`.
5. Build/load/install this operator in both clusters.
6. Install and verify one minimal Prometheus instance per cluster, including
   an `up` controller scrape target.
7. Verify all Deployments are Ready, both watch lists are bounded, and no
   database Cluster exists.
8. Verify controller RBAC, including Secret write-only behavior and absence of
   cluster-wide bindings.

### Phase 1: first namespace and initial source/replica pairs

1. Create `e2e-first` in both clusters.
2. Add it to the CNPG and controller watch list in both clusters. The resulting
   list is `e2e-bootstrap,e2e-first`.
3. Create source Cluster `db01` in `us`.
4. Wait for `db01` to become Ready, create its static SQL management account,
   expose its primary endpoint, and onboard it to Vault.
5. Create replica Cluster `db02` in `eu` pointing to `db01`, then create its
   dummy password Secret and dummy username.
6. Create source Cluster `db03` in `eu`, wait for it to become Ready, create
   its static SQL management account, expose its primary endpoint, and onboard
   it to Vault.
7. Create replica Cluster `db04` in `us` pointing to `db03`, then create its
   dummy password Secret and dummy username.
8. Assert initial dynamic issuance, Secret patching, username patching, WAL
   receiver activity, and WAL marker propagation for both directions.
9. Confirm that the controller in the replica's local region performed the
   work and that the source region's controller did not mutate a standalone
   primary.

### Phase 2: failover, Pod replacement, and node drain

Run these cases against a replica Cluster while preserving the other database
as a control fixture.

#### 2.1 Explicit failover in a replica Cluster

Trigger an explicit failover/promotion in the selected replica Cluster with
the released CNPG plugin command:

```text
kubectl cnpg --context kind-k8s-eu --namespace e2e-first \
  promote db02 db02-2
```

The candidate instance is selected from the current standby Pods rather than
hard-coded if CNPG names differ. Record the pre-failover designated-primary
identity and the post-failover identity. This command tests an in-cluster
failover; the cross-region distributed switchover remains the declarative
`demotionToken`/`promotionToken` sequence in Phase 3.

Assert that the topology episode causes exactly one credential rotation, the
new designated primary can authenticate, WAL continues, and the former lease
is revoked. A duplicate Cluster status update or repeated Pod observation
must not cause another issuance.

#### 2.2 Delete the designated primary Pod

Delete only the current designated-primary Pod and wait for CNPG to replace
it. Do not delete the Cluster or target Secret. Assert a new Pod UID/restart
episode, exactly one rotation, WAL recovery, and revocation of the old lease.

The controller must not rotate merely because it saw a deletion event; it must
wait until the replacement/designated primary is observed and the event
fingerprint is complete.

#### 2.3 Cordon and drain the primary's node

Identify the node holding the current designated primary, cordon it, and drain
it with bounded, test-controlled eviction flags. Wait for CNPG to replace or
promote the affected instance onto a different PostgreSQL worker. The three
PostgreSQL workers are specifically required so the drained node is not the
only eligible destination. Assert one rotation after the replacement primary
is observed, successful WAL recovery, and old-lease revocation. Uncordon the
node and verify that the cluster is stable before the next phase.

The harness records the node, Pod UID, replacement identity, and timing so a
failure can distinguish node-drain behavior from a normal Pod restart.

### Phase 3: cross-region switchover

Use `flow-a` (`db01` and `db02`) for this scenario. The harness performs
the distributed-topology
switchover as a declarative two-step operation:

1. Demote `db01` in `us` using the CNPG-supported demotion flow.
2. Wait for and capture its `demotionToken`.
3. Apply the token with the required `promotionToken` to `db02` in `eu`.
4. Wait for `db02` to become the new primary.
5. Reconfigure the former `db01` as the new replica of the promoted
   cluster using the CNPG distributed-topology contract. All CR mutations are
   made by the harness, not this controller.

The final topology must have the former primary demoted and streaming from
the new primary. Assert that:

- the demoted former primary is recognized as a replica and gets a new
  region-local dynamic credential for the new source;
- the promoted primary remains healthy and is not treated as a target for
  replica credential rotation;
- the new source endpoint is reachable from the demoted region;
- `/pg/status` reports an active WAL receiver on the demoted cluster; and
- a source WAL marker crosses the new region boundary.

### Phase 4: promote a replica to standalone and rebuild replicas

Use `db04` for this independent promotion scenario so the original `db03`
remains available as a source that lost its replica.

1. Promote `db04` to a standalone primary and remove its replica
   declaration according to the supported CNPG workflow.
2. Ensure it is no longer in replica mode.
3. Assert that the controller does not patch its username or target Secret,
   revokes any current/pending dynamic replication leases associated with the
   old replica relationship, and removes or settles its state entry according
   to the design cleanup path.
4. Create replacement replica Cluster `db05` for `db03`.
5. Create new replica Cluster `db06` for the newly promoted standalone
   `db04`.
6. Create fresh dummy Secrets and dummy usernames for both new targets.
7. Onboard any newly promoted source management account into Vault and verify
   dynamic issuance, WAL receiver activity, WAL markers, and old-lease
   revocation for both new replica relationships.

The test confirms that a Cluster UID or name reuse cannot inherit a stale
lease/workflow from a previous incarnation.

### Phase 5: second namespace

1. Create `e2e-second` in both clusters.
2. Add it to both CNPG and controller watch lists while retaining
   `e2e-first` and `e2e-bootstrap`.
3. Create source Cluster `db07` in `us` and replica Cluster `db08` in `eu`,
   with `db08` pointing to `db07`.
4. Create the static SQL management account, expose the source, onboard it to
   Vault, and create the replica's dummy Secret/username.
5. Verify dynamic credentials and WAL replication.

This proves namespace addition installs the required target RBAC and updates
both independent controllers without disturbing existing state.

### Phase 6: remove and restore the first namespace

This scenario directly tests the design's safety rule that a namespace absent
from `WATCH_NAMESPACE` is out of scope, not evidence that its Clusters were
deleted.

1. Remove `e2e-first` from this controller's watch configuration in both
   regions, leaving it in CNPG's watch configuration and leaving the database
   resources and target Roles intact. This is an intentional controller-only
   misconfiguration.
2. Roll out both regional controllers and assert that their effective watch
   lists no longer include `e2e-first`. Separately assert that CNPG still
   watches `e2e-first` and remains Ready.
3. Confirm that the state entries and all known Vault leases for
   `e2e-first` remain unchanged and that no cleanup/revocation occurs.
4. Trigger a failover in one of the first-namespace replica Clusters.
5. Assert that no credential rotation occurs while that namespace is outside
   the controller watch list, while existing replication continues.
6. Keep `e2e-first` in CNPG's watch list and re-add it to this controller's
   watch list, then wait for both caches and controllers to become Ready.
7. Trigger another supported failover in that replica and assert that a
   credential rotation now occurs, followed by WAL recovery and old-lease
   revocation.

This phase deliberately keeps CNPG watching the namespace so that its normal
failover behavior remains available. The expected result is that CNPG changes
the primary and keeps replication working, while the regional credential
controller does nothing because the namespace is outside its own watch scope.

### Phase 7: deprovision a database

Use the Phase 5 database pair so that the first-namespace state remains
available for final namespace assertions.

1. Delete the source and replica CNPG `Cluster` resources for the second-
   namespace database through the test harness.
2. Leave the target Secret in place to verify that this controller does not
   delete it.
3. Wait for the controller's default five-minute orphan sweep and the required
   two consecutive confirmed absences.
4. Assert that all known dynamic leases for the deleted replica are revoked,
   the corresponding state entries are removed, and no finalizer was needed.
5. Verify that the source-side Vault database configuration can be removed by
   the fixture cleanup after the controller has finished lease cleanup.

The test must allow the design's normal cleanup delay—approximately ten
minutes plus API/Vault time—rather than reducing the configured sweep and
absence defaults without review.

## Negative and safety checks

These checks run as part of the ordered suite or as a small isolated fixture
before teardown.

### Invalid replication password

After a successful rotation, deliberately replace the target Secret's
password with an invalid test value using the test actor, while retaining the
Cluster username. Trigger or observe the corresponding reconnect window and
assert that `/pg/status` reports `isWalReceiverActive: false`.

The controller must not treat an unchanged Ready condition as authentication
proof, must not revoke the still-needed valid lease prematurely, and must not
commit a failed pending rotation. Restore the correct password through the
normal controller rotation path and verify that WAL recovery and lease cleanup
resume.

### Write-only Secret access

For every watched namespace, assert with `kubectl auth can-i` that the
controller ServiceAccount can patch a named Secret but cannot get, list, watch,
update, create, or delete Secrets. Verify that the controller cache has no
target Secret informer and that no target Secret event is a rotation trigger.

### No data leakage

Scan controller logs, Kubernetes Events, rendered state, and test summaries
for the known dummy values, dynamic usernames, passwords, Vault token, and
unredacted lease responses. The state Secret may contain lease IDs and
workflow metadata, but never a password, target Secret data, or current
username.

### Event deduplication and recovery

Replay or wait for duplicate Cluster/Pod observations for one event and assert
that only one Vault credential is issued. Restart the regional controller at
each persisted workflow boundary where practical, then assert that it resumes
from the state Secret without issuing a duplicate credential or losing the
lease to revoke.

## Timing and wait policy

Use the defaults in `DESIGN.md`:

| Setting | Value |
| --- | --- |
| Minimum Vault lease | `768h` |
| Lease safety margin | `24h` |
| Password propagation delay | `5s` |
| Verification delay | `2m` |
| Issue/persist deadline | `1m` |
| Password-to-username deadline | `1m` |
| Reconnect verification deadline | `5m` |
| Orphan sweep | `5m` |
| Required deletion absences | `2` |

All waits are bounded polling with diagnostics and an overall phase timeout.
The controller must use timed requeues, not blocking sleeps. The E2E harness
may have longer outer timeouts, but it must not silently shorten the design
defaults. Every timeout prints the relevant redacted Cluster, Pod, state,
Vault, and `/pg/status` evidence.

## Artifacts and teardown

For every run, preserve:

- selected release, Git/plan baseline, tool versions, and Kind configs;
- rendered CNPG, Vault, and controller manifests with Secrets redacted;
- namespace/watch-list transitions;
- redacted Cluster, Pod, state, lease, and `/pg/status` snapshots;
- controller and CNPG logs with credential values scrubbed; and
- a scenario result summary with lease fingerprints and WAL markers.

On success or failure, collect diagnostics before teardown. Then revoke any
remaining test leases, delete the ephemeral Vault workload, and delete only
the two run-owned Kind clusters. The harness must not delete arbitrary Docker
containers, volumes, namespaces, or user clusters.

## Review gates before implementation

The following implementation checks remain:

1. **Host-network reachability.** Validate that Pods in each Kind cluster can
   reach the other cluster's control-plane node IP and dedicated gateway port.
   If this fails, stop and review the Docker-network relay fallback; do not
   introduce NodePorts implicitly.
2. **CNPG release behavior.** Confirm the exact candidate standby name and
   command output from `kubectl cnpg promote` on the selected v1.30.0 release.
3. **Vault role naming.** Keep source Cluster names and selected external
   cluster entry names aligned so the role convention remains deterministic.

## Implementation checks

The design choices are settled. Implementation should still validate, rather
than assume, the following environment-specific details:

1. Pods in each Kind cluster can reach the other cluster's control-plane node
   IP and dedicated host-network gateway port.
2. The selected released CNPG plugin accepts the chosen standby instance for
   `kubectl cnpg promote` and reports the expected topology transition.
3. Source Cluster names and selected external-cluster entry names remain
   aligned with the Vault role convention.

These checks do not change the controller contract or authorize additional
responsibilities.

Acceptance of this plan authorizes the next phase—implementing the self-
contained harness and then the controller behavior described in `DESIGN.md`—
but does not authorize adding responsibilities absent from that design.
