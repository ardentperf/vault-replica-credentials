---
id: vault_replica_credentials_design
title: Vault dynamic credentials for replica clusters
---

# Vault dynamic credentials for replica clusters
<!-- SPDX-License-Identifier: CC-BY-4.0 -->

## Status and scope

This is an implementation design for an external controller. It is not a
CloudNativePG feature and does not require changes to the CloudNativePG source
code.

## Goal

Rotate the PostgreSQL streaming-replication credentials used by a CloudNativePG
replica cluster from HashiCorp Vault without granting the controller read
access to the Kubernetes Secret containing the password.

The controller rotates credentials only in response to an event that already
involves a restart, replacement, failover, or switchover of the replica
cluster's designated primary instance. It does not run a periodic password
rotation loop and does not rely on Secret events to initiate rotation.

The intended sequence is:

1. Ask Vault for a new dynamic database credential.
2. Persist the new Vault lease ID in controller state.
3. Patch the password in the target Kubernetes Secret.
4. Patch the username in the CloudNativePG `Cluster` resource last.
5. Allow CloudNativePG to reconcile the changed username and reconnect
   streaming replication.
6. Verify the new replication connection.
7. Revoke the old Vault lease and finalize controller state.

The password patch happens before the username patch. After the password patch
is accepted, the controller waits a few seconds before changing the username.
This gives the API/cache path time to observe the new password before
CloudNativePG is triggered to reconnect. The delay is implemented with a
short timed requeue rather than blocking a controller worker. A short
authentication failure window is still accepted because the operation is tied
to an already disruptive cluster event. No additional defensive Secret reload
is required.

## Architecture

The design has one controller deployment in the `cnpg-system` namespace. The
controller watches CloudNativePG resources and Pods in the namespace list used
by the CloudNativePG controller. It uses a single ServiceAccount, with
separate namespaced `RoleBinding` objects in each approved namespace.

The cache configuration has two scopes: `Cluster` and target `Pod` watches use
only the namespaces in `WATCH_NAMESPACE`, while the state Secret and
leader-election Lease use `cnpg-system`. `cnpg-system` does not need to be in
`WATCH_NAMESPACE` unless it is also an approved replica-cluster namespace.

Each replica cluster participates in two Secret roles:

| Object | Namespace | Purpose | Controller access |
| --- | --- | --- | --- |
| Target credential Secret | Replica cluster namespace | Password consumed by CloudNativePG | `patch` only, any Secret name in the watched namespace |
| Consolidated Vault state Secret | `cnpg-system` | Compact current and pending Vault lease IDs for all managed replica clusters | Normal namespaced Secret cache plus `get`, `list`, `watch`, `update`, and `patch` |

The controller also patches the replica cluster's `Cluster` resource to
change the dynamic username. CloudNativePG remains responsible for reading the
target credential Secret and writing PostgreSQL's connection configuration.

Direct access to etcd is not part of this design. The Kubernetes API is the
supported persistence and authorization boundary; direct etcd access would
bypass normal Kubernetes RBAC and API auditing.

## Namespace configuration

The controller accepts its own `WATCH_NAMESPACE` environment variable and
expects operators to set it to the same comma-separated namespace list used by
the CloudNativePG controller. There is no runtime dependency on, or watch of,
the CloudNativePG controller Deployment.

For example, both Deployments should receive the same value from a shared
Helm or Kustomize value:

```yaml
env:
  - name: WATCH_NAMESPACE
    value: "reporting,analytics"
```

The controller parses, trims, and validates the list at startup, then creates
namespace-scoped caches and watches for exactly those namespaces. It adds a
separate `cnpg-system` cache scope only for the state Secret and
leader-election Lease. If the value is missing, empty, malformed, or
represents an unrestricted cluster-wide watch, the controller fails closed
rather than expanding its scope.

Changing the namespace list requires rolling out the controller with the new
value. The namespace list should have one configuration owner so that the CNPG
and Vault-replica controller Deployments cannot drift accidentally. The
controller should log the resulting namespace set, but not any Secret
contents.

Do not remove a namespace from `WATCH_NAMESPACE` while it contains a managed
replica `Cluster`. If a namespace is removed anyway, the controller must stop
processing it and must not revoke or otherwise clean up its Vault leases. The
state entry and leases remain untouched until the namespace is added back and
becomes visible again. Once it returns, the controller can resume pending work
and run the normal deprovisioning sweep. If the Cluster is then absent, the
sweep may revoke or allow the leases to expire according to the normal cleanup
policy.

## CloudNativePG configuration

The replica cluster's external cluster entry refers to a target Secret for the
password. The username is kept in `connectionParameters.user` and is updated
by the controller.

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: reporting-replica
  namespace: reporting
spec:
  replica:
    enabled: true
    source: production
  externalClusters:
    - name: production
      connectionParameters:
        host: production-rw.production.svc
        user: v-replica-initial
        dbname: postgres
        sslmode: verify-full
      password:
        name: replica-source-credentials
        key: password
```

The target Secret is created by the database-provisioning workflow. It may be
created before or after the controller and replica `Cluster` start. The
controller does not need `create` permission for it.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: replica-source-credentials
  namespace: reporting
type: Opaque
data:
  # Base64-encoded password. The controller patches this key only.
  password: <base64-password>
```

The Secret does not need to contain the Vault username. The username in the
`Cluster` resource is the source of truth for the replication connection.

Changing `connectionParameters.user` changes the generated replication
connection configuration. CloudNativePG reconciles that change and reloads the
instance configuration, causing PostgreSQL's WAL receiver to reconnect.

For verification, use the CloudNativePG instance-manager status API rather than
direct PostgreSQL access. The `GET /pg/status` response for an instance includes
`isWalReceiverActive`. This is the same instance status data used by
`kubectl cnpg status`; it is not a field in the `Cluster.status` object. The
controller reads it through the Kubernetes API server's `pods/proxy` subresource
for the designated-primary Pod.

The designated-primary Pod must expose the normal CloudNativePG instance
status endpoint. If the endpoint is unavailable, verification fails and the
controller retries; it must not fall back to treating unchanged Pod Ready or
Cluster status as proof of successful authentication.

## External prerequisites

### Vault

Configure the Vault database secrets engine and role before enabling rotation:

- The controller's Vault authentication method must work unattended for the
  lifetime of the controller. Database-lease renewal is not implemented, but
  the controller's Vault client may still need to re-authenticate or maintain
  its own Vault token.
- The database role must permit the controller identity to issue credentials
  and revoke the resulting leases, limited to the required role/path.
- The role's `default_ttl` must equal its effective `max_ttl`, targeted at
  `768h` (32 days), and the controller must validate the returned
  `lease_duration` against the configured minimum. A shorter returned lease is
  a configuration failure for this design, not a reason to silently proceed.
- The role must have creation and revocation statements that produce a
  PostgreSQL `LOGIN` user with the `REPLICATION` privilege and cleanly revoke
  or drop it when its lease is revoked.
- In a multi-database installation, the E2E convention derives the Vault
  database role from the source CNPG `Cluster` name: the selected source
  external-cluster entry has that Cluster name and the role is addressed as
  `/database/creds/<source-cluster-name>`. The future controller must derive
  the role from the current source reference rather than use one global role
  for every database.

### Source PostgreSQL

The source cluster's `pg_hba.conf` and TLS configuration must allow the Vault-
generated usernames to connect for the replication connection. The Vault role
must create users compatible with the configured host, database, TLS mode, and
any username length or character restrictions. The source-side replication
slot policy must also tolerate the credential rotation and delayed cleanup.

The privileged PostgreSQL account used by Vault must be provisioned outside
CloudNativePG's declarative account-management resources. If CNPG owns that
account, CNPG may reconcile its declared password and overwrite a password
rotation performed by Vault. The E2E fixture therefore creates the account
with SQL using a static initial name/password, grants `CREATEROLE`, and leaves
that account outside CNPG management. Vault rotates only the dynamic accounts
it creates; this controller does not create or manage either kind of account.

### CloudNativePG release baseline

The E2E environment uses the latest released CloudNativePG version, not a
trunk or source build. The upstream `cloudnative-pg/cnpg-playground` main
commit used as the test-plan reference is
`1957b42b445532d284513964f53e3085b4f745f9`; its `scripts/common.sh` sets the
current released baseline to `v1.30.0`. The test harness may update this pinned
baseline when a newer release is selected, but must install a released
manifest and record the exact version in its artifacts. The controller source
must not import, vendor, or otherwise depend on `cnpg-playground`.

## Operational defaults

The following values are the initial defaults for implementation and E2E
testing. They remain configurable where noted, but a test must not silently
change them:

| Setting | Default |
| --- | --- |
| Minimum returned Vault lease duration | `768h` |
| Vault lease safety margin | `24h` |
| Password-to-username propagation delay | `5s` |
| Verification delay after username patch | `2m` |
| Issue/persist stage deadline | `1m` |
| Password-to-username stage deadline | `1m` |
| Reconnect verification stage deadline | `5m` |
| Orphan sweep interval | `5m` |
| Confirmed absences before deletion cleanup | `2` consecutive sweeps |
| Decoded state JSON ceiling | `256 KiB` |
| Active reconciliation workers | `1` |

Leader election is enabled by default and uses one namespaced Lease in
`cnpg-system`. The propagation and verification waits are persisted timestamps
and timed requeues, not blocking sleeps.

## E2E environment and coverage

The detailed, reviewable test plan is [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md). It
defines a self-contained two-region Kind environment with regions named `us`
and `eu`, one CloudNativePG installation per region, this operator in each
`cnpg-system` namespace, and one ephemeral HTTP Vault instance in `us`. Vault
authentication is intentionally represented by a test-only static token; the
tests cover the dynamic database provider, not Vault auth setup or certificate
authentication.

Each E2E Kind cluster has one control-plane node and three tainted PostgreSQL
worker nodes. CloudNativePG, Vault, this operator, and the test gateways run on
the control-plane node; PostgreSQL instances run on the PostgreSQL workers.
The three workers are required so a cordon/drain test can move a replica
cluster's primary Pod to a different eligible PostgreSQL node and verify that
the resulting replacement episode causes credential rotation.

The harness copies useful setup patterns from the upstream
[`cloudnative-pg/cnpg-playground`](https://github.com/cloudnative-pg/cnpg-playground)
repository, pinned for this plan to commit
`1957b42b445532d284513964f53e3085b4f745f9`. It must not import, vendor, invoke,
or otherwise require that repository at runtime. Both regional CNPG operators
are installed from the latest selected released manifest; the upstream commit's
current release baseline is `v1.30.0`, and the exact installed version must be
recorded in test artifacts.

The test environment starts without database `Cluster` resources. It then
creates watched namespaces and source/replica pairs, uses SQL to create a
static SCRAM/password management account in each source database, onboards the
source into Vault, and starts each replica with a dummy password Secret value
and dummy external-cluster username. The assertions cover dynamic credential
issuance, write-only Secret patching, username rotation, WAL receiver status,
cross-region switchover, failover and node replacement, promotion cleanup,
namespace watch removal and restoration, and Vault lease revocation.

For cross-Kind connectivity, the selected E2E transport is a test-only
host-network TCP gateway on each source cluster's control-plane node. It
forwards to the stable CNPG `-rw` Service and uses no PostgreSQL NodePort. The
single E2E Vault instance uses the same host-network reachability pattern in
the `us` cluster; its normal ClusterIP Service remains available for local
access.

The E2E database fixtures use PostgreSQL 18, two instances per source or
replica Cluster, and CNPG Distributed Topology from creation. Two instances
are sufficient for the required in-cluster failover, Pod replacement, and
node-drain cases while keeping the two-region Kind environment manageable.
The scenarios run as one ordered suite because later promotion, re-replication,
namespace, and cleanup cases intentionally depend on earlier state.

The E2E fixture uses role-neutral CNPG Cluster names from one run-wide,
monotonically increasing counter: `db01`, `db02`, `db03`, and so on. The
counter is shared across regions and namespaces, and a new Cluster always gets
the next name regardless of whether it is initially a source or replica. A
name identifies one Cluster object, not its current role or region. Names
remain unchanged when a replica is promoted; replacement or newly added
Clusters receive the next unused name and names are not reused during a run.

The controller never opens a PostgreSQL connection. Its replication check uses
the Kubernetes API and the instance-manager `/pg/status` endpoint described
below. Independently, the E2E test actor may connect to PostgreSQL and query
catalog views to prove streaming replication, so the acceptance test does not
depend solely on CloudNativePG status fields that may differ in later
PostgreSQL/CNPG combinations.

The namespace-removal scenario intentionally removes a namespace from this
controller's watch list while leaving it in CNPG's watch list. This is a
misconfiguration test: CNPG continues to perform failover and replication,
while this controller must neither rotate nor clean up credentials until the
namespace is watched again.

## Consolidated Vault state Secret

Use one dedicated state Secret in `cnpg-system`. Keep its `state.json` compact:
store only the lease and workflow information that cannot be reconstructed from
the current `Cluster` resource. Do not duplicate the target namespace,
external-cluster name, credential Secret name, issued timestamps, or current
username in state.

The state never contains a Vault password:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vault-replica-controller-state
  namespace: cnpg-system
type: Opaque
data:
  state.json: <base64-encoded-state-json>
```

The decoded `state.json` can have this shape:

```json
{
  "version": 1,
  "clusters": {
    "reporting/reporting-replica": {
      "clusterUID": "7f4c1d2e-...",
      "currentLeaseID": "vault/database/creds/replica/...",
      "currentExpiresAt": "2026-09-22T00:00:00Z",
      "pending": null,
      "lastEvent": "pod-restart/<primary-pod-uid>/r3"
    }
  }
}
```

The actual controller writes the Secret value through the `data` field, using
base64 encoding. The state Secret should be pre-created once during controller
installation. The simplest implementation uses the normal controller-runtime
cache/client for the `cnpg-system` namespace, so the controller has namespaced
Secret `get`, `list`, `watch`, `update`, and `patch` access. It writes only the
named state object.

The important recovery fields are the per-cluster `clusterUID`,
`currentLeaseID`, `currentExpiresAt`, `pending` object, and `lastEvent`. The
Cluster UID distinguishes a newly created Cluster that reuses the same
namespace/name from the previous Cluster. The pending object's `stage` records
whether the password patch completed. These fields allow the controller to
resume a rotation after a process crash when the password patch has already
succeeded, and allow it to clean up a lease that was issued but not yet
applied.

An in-progress `pending` object contains only `leaseID`, `username`,
`expiresAt`, `stage`, `stageDeadline`, `triggerID`, and, when a deliberate
delay is required, `nextActionAt`. Valid stages are `issued`,
`waiting-secret`, `password-patched`, `reconnect-pending`, and `verified`.
`replacement-backoff` is also valid when a pending credential attempt timed
out or expired and must be replaced. In that stage, the old pending lease ID
is retained until best-effort revocation succeeds; no password is stored.
`stageDeadline` is the durable timeout for the current action stage.
`triggerID` identifies the restart or topology-change episode being processed.
`nextActionAt` is written only for the short password-propagation pause and the
delayed verification check; it is cleared when the next stage begins.

The controller should write state only when a phase or lease value changes. It
should not write heartbeat timestamps on every retry. This keeps the state
small and limits unnecessary etcd writes. Set a practical application ceiling,
such as 256 KiB for the decoded JSON, alert before reaching it, and reject new
state entries rather than allowing the object to grow without bound. The
expected deployment population is small enough that one compact state object
is sufficient. The controller uses the normal namespaced Secret cache for this
object, so its `cnpg-system` Role intentionally grants Secret list/watch access
there. This is simpler than maintaining a second uncached API path. The
controller writes only the named state Secret.

## Idempotency and restart recovery

The controller is an at-least-once reconciler. Its correctness must not depend
on a particular event being delivered exactly once or on an in-memory timer
surviving a process restart. Treat the consolidated state Secret as a compact
write-ahead journal for one pending rotation per cluster.

Each reconciliation loads the cluster entry and resumes from its current
`pending.stage`. Persist the next stage immediately after the corresponding
side effect succeeds, before attempting the next side effect:

1. After Vault returns a credential, persist the complete pending lease,
   `triggerID`, and stage deadline, then set the stage to `issued`.
2. After the target Secret password patch succeeds, set the stage to
   `password-patched`, set `nextActionAt` to a few seconds in the future and a
   deadline for the username patch, then requeue without blocking a worker.
3. Once `nextActionAt` is reached, patch the username and set the stage to
   `reconnect-pending` with a new `nextActionAt` and deadline for verification.
4. Once verification succeeds, set the stage to `verified`.
5. After old-lease revocation succeeds, promote the pending lease to current,
   clear `pending`, and record the event fingerprint.

Every step after Vault issuance must be an `ensure` operation that is safe to
repeat: write the same password, write the same username, re-read status, or
revoke the same lease. A restart therefore replays the current stage rather
than starting a new rotation. If `nextActionAt` is in the future, requeue for
the remaining duration; do not rely on the controller-runtime queue timer as
durable state. If it is absent, use the stage's normal immediate behavior.

Vault issuance is the one non-idempotent boundary. Vault database credential
issuance and the Kubernetes state write cannot be committed atomically. A
crash after Vault issues a lease but before its `leaseID` is persisted can
leave an untracked lease, and a retry can issue another one. Keep this
unavoidable window small by persisting the lease immediately after issuance;
the untracked lease is bounded by the configured maximum TTL and expires
without renewal. Exact once-only issuance would require Vault-side request
idempotency or a transaction spanning both systems and is outside this design.

State writes must handle normal Kubernetes resource-version conflicts by
re-reading and retrying. A pending lease must never be replaced merely because
the same event was delivered again. The event fingerprint is recorded only
when the rotation is committed, so duplicate events during a pending rotation
continue the existing state machine.

Each stage has a persisted deadline in addition to any retry backoff. A
controller restart may reset in-memory exponential backoff, but it must not
reset a stage deadline or turn a pending operation into a new Vault issuance.
When a stage deadline is reached, retain the current credential state, record a
failure, and retry the required operation with exponential backoff. If the
pending lease is expired or too close to expiry to complete safely, persist the
`replacement-backoff` stage before abandoning the attempt. Best-effort revoke
the old pending lease, then issue a replacement when the retry is due. Do not
clear the replacement intent before a new lease is persisted. If replacement
issuance fails, retain `replacement-backoff` and retry with backoff; a
controller restart may reset that backoff. Do not treat an expired pending
lease as verified or commit it as the current lease.

Suggested initial stage timeouts are one minute for issuing/persisting and for
the password-to-username transition, five minutes for reconnect verification,
and a configurable safety margin before `expiresAt`. These are operation
deadlines, not blocking sleeps; all waits use persisted timestamps and
requeues.

## Event handling

The controller watches only the namespaces discovered from `WATCH_NAMESPACE`
for:

- CloudNativePG `Cluster` resources;
- CloudNativePG instance Pods; and
- optionally, explicitly managed restart or failover signals.

These are Kubernetes API watches on the underlying resources, not watches on
Kubernetes `Event` objects. The controller does not watch `core/v1` Nodes or
`core/v1`/`events.k8s.io` Events. Node drain and maintenance are inferred from
the designated-primary Pod replacement observed through the Pod watch. Event
objects may be emitted as an audit signal, but they are not an input to
rotation.

It does not need to watch the target credential Secret. The password update is
performed directly with a write-only patch, and the subsequent username patch
is the CloudNativePG reconciliation trigger.

Candidate events include:

- creation of a new replica `Cluster` or its initial primary Pod;
- deletion and replacement of the designated primary Pod;
- a change to the designated or current primary in the `Cluster` status;
- an explicitly requested CloudNativePG restart;
- a failover or switchover that changes the primary; and
- a node-drain or node-maintenance event that replaces the designated primary
  Pod.

For every candidate event, the controller should fetch the current `Cluster`
object and verify all of the following before mutating anything:

1. The cluster is still a replica cluster.
2. The event corresponds to the designated primary, not an unrelated instance.
3. The configured external cluster and its password reference are present in
   the expected configuration. Read the target Secret name and password key
   from the current `Cluster` object; do not discover them by reading or
   watching the target Secret.
4. No rotation for the same cluster is already in progress.

The immutable Cluster name is a stable queue key, but it is not sufficient for
deduplication. A deleted Cluster can later be recreated with the same
namespace/name, and a Pod deletion and replacement have different UIDs. The
controller should therefore construct a durable `triggerID` from the Cluster
UID and the observed restart/topology episode. For a Pod replacement, use the
new designated-primary Pod UID; for an in-place restart, include the relevant
container restart count; for a topology transition, include the relevant
CloudNativePG primary transition identity, such as the target-primary identity
and transition timestamp. The exact predicates must be defined so a deletion
event and its replacement do not create two rotations. Treat deletion of the
old Pod as a hint only; begin the rotation after the current observation
identifies the replacement/designated primary or the in-place restart count.
Do not trigger on every generic Cluster status update.

Persist the Cluster UID in the state entry and the `triggerID` in `pending`;
record the committed trigger in `lastEvent`. A repeated observation of the
same trigger is ignored or resumes the pending state. A later, genuinely new
restart episode may enqueue another rotation after the current one commits.
The controller must also ignore changes caused solely by its own username
patch. If the observed Cluster UID differs from the state entry, do not resume
the old workflow for the reused namespace/name. Treat the old entry as stale,
revoke its known current/pending leases (or wait for them to expire), and retry
that cleanup with backoff if Vault is unavailable. Do not mutate the new
Cluster or discard the old lease IDs until the stale entry is cleaned. Then
reset the entry for the new Cluster UID and process the new Cluster as a fresh
initialization.

After a promotion, the controller must re-check replica status immediately
before patching the username. If the cluster is no longer a replica, the
rotation is skipped. The cleanup path should revoke both the pending lease and
the current replication lease when they are no longer needed after promotion.
If the Cluster is later made a replica again, it must receive a new rotation
and must not rely on the old lease.

A transition out of replica mode is a cleanup event even though it is not a
rotation event. Enqueue it, perform no replica-specific credential mutation,
revoke the known pending/current leases, and remove the state entry after
cleanup succeeds.

### New replica clusters and missing Secrets

The target credential Secret may be created after the controller and the
replica `Cluster` are already running. A new `Cluster` event is therefore an
eligible initialization event, even when the Secret does not yet exist.

Because the controller has write-only access to the target Secret, it does not
probe it with `GET` and does not watch it. Instead, it attempts the password
patch and handles an API `404 NotFound` as `WaitingForCredentialSecret`.

The controller should then requeue the cluster with exponential backoff and
jitter. The delayed retry is the mechanism that notices the later Secret
creation. It should not request a new Vault credential on every retry. Keep
the lease metadata in the per-cluster `pending` state and retain the password
only in the controller's process memory while retrying the patch. If that
pending lease expires before the Secret appears, revoke it, clear the pending
state, and request a replacement on a later retry.

If the controller restarts while waiting, the pending password is not
recoverable from state by design. Revoke the pending lease and begin a fresh
issuance on a later retry; do not store the password merely to avoid this
case.

This also applies when the Secret is deleted or temporarily unavailable during
an ordinary rotation. A startup sweep should requeue new replica clusters,
clusters whose pending stage is `waiting-secret`, and clusters with a pending
rotation. Stage deadlines and lease expiration must be checked on every such
reconciliation, not only while waiting for the Secret.

### Deprovisioning and orphan cleanup

Do not use a Kubernetes finalizer for credential cleanup. Instead, run a
periodic orphan sweep, for example every five minutes, that compares the
Cluster identities recorded in the state Secret with the current `Cluster`
objects in the namespaces from `WATCH_NAMESPACE`. A Cluster that still exists
but has been promoted is handled by the promotion cleanup path, not treated as
a deleted Cluster.

For a state entry whose Cluster no longer exists, the controller should:

1. Revoke `currentLeaseID`, if present.
2. Revoke the pending lease, if present.
3. Remove the state entry.

If the namespace/name exists but its Cluster UID differs from the state entry,
the sweep uses the same stale-incarnation handling described under event
deduplication: clean the known old leases before replacing the state identity.

The sweep should use a fresh API read and require a confirmed `404` or absence
from two consecutive sweeps before revoking. This prevents a temporary cache
or API-listing problem from being interpreted as deprovisioning. A sweep is the
cleanup guarantee if a Cluster deletion event is missed; with a five-minute
interval and two required absences, the normal cleanup delay is up to roughly
ten minutes plus API and Vault request time.

If a namespace is absent from `WATCH_NAMESPACE`, it is outside the controller's
scope. The orphan sweep must not interpret that absence as deprovisioning and
must not touch its state or leases. Cleanup resumes only after the namespace is
returned to the watch list. If the Cluster is then confirmed absent, revoke
the known leases and remove the state entry; any lease left untouched can still
expire naturally in Vault.

## Rotation state machine

Reconciliation work is serialized per replica cluster by the controller-runtime
work queue; no separate distributed rotation lock is used.

```text
Ready
  |
  | eligible new-cluster/restart/failover event
  v
Issuing
  |
  | Vault returns username, password, lease_id, expiration
  v
PendingPersisted
  |
  | patch target Secret password
  +----------------------+
  |                      |
  | 404 NotFound         | patch succeeds
  v                      v
WaitingForCredentialSecret  PasswordPatched
  |                      |
  | backoff and retry    | patch Cluster externalClusters[].connectionParameters.user
  |                      | after a short timed requeue
  +------->--------------+
                         v
ReconnectPending
  | verification succeeds
  v
Verified
  |
  | revoke previous lease
  v
Ready

ReconnectPending
  | timeout or expiry
  v
ReplacementBackoff
  |
  | revoke failed attempt, then issue replacement
  v
Issuing
```

The detailed workflow is:

### 1. Enqueue and serialize cluster work

Use the replica `Cluster`'s `namespace/name` as the reconciliation key. Map
candidate `Cluster` and Pod events to that key and rely on the
controller-runtime work queue to prevent the same key from being reconciled
concurrently. Duplicate events for the key are coalesced and, when received
while reconciliation is running, are processed again after the current
reconciliation returns.

If two controller Pods are deployed for availability, use normal controller
leader election so that only one Pod is active at a time. This design does not
require a separate distributed per-cluster lock or coordination between two
active controllers. Do not launch rotation work in unmanaged background
goroutines, because that would bypass the work queue's serialization.

Because all clusters share one compact state Secret and there is no scale
requirement, configure one active reconciliation worker initially. This avoids
most cross-cluster state-Secret conflicts; the resource-version retry logic is
still required for leader handoff and unexpected concurrency.

The queue only serializes active reconciliations; it does not protect a
workflow across a requeue or process restart. The persisted `pending` object
is therefore recovery state, not a lock. It records which Vault lease and
rotation stage must be resumed or cleaned up.

Read the state Secret and inspect the entry's `pending` object. If a valid
pending rotation exists and the controller still has the password in memory,
resume it instead of issuing a new Vault lease. If a process restarted before
the password patch completed, the password is intentionally unavailable from
state; revoke that abandoned pending lease and issue a replacement rather than
storing the password in Kubernetes state.

The controller must compare `stageDeadline` and `expiresAt` with the current
time on every reconciliation. Backoff is in-memory and may restart from its
initial value after a controller restart; stage deadlines are persisted and
must not be extended by that restart.

### 2. Issue a dynamic Vault credential

Read the Vault database dynamic role endpoint, for example:

```text
/database/creds/<role>
```

The response supplies the username, password, `lease_id`, and lease
duration. For this design, configure the Vault database role with
`default_ttl` equal to its effective `max_ttl`, normally targeted at 32 days
(`768h`). Do not implement Vault database-lease renewal. The returned
`lease_duration` remains authoritative, because the system, mount, or role
configuration can impose a lower maximum. Validate that the returned duration
meets the configured minimum required for the weekly-event plus grace-period
model; do not silently accept a shorter lease. Keep the password only in
process memory until the target Secret patch succeeds.

Persist the new lease as the entry's `pending` object, including its
`stageDeadline` and `triggerID`, before changing either Kubernetes credential
object. Never persist the returned password in the state Secret.

### 3. Patch the target password Secret

Read the target Secret name from the selected external-cluster entry's
`password.name` field in the current `Cluster` object, then use a direct JSON
merge patch against that name:

```json
{"data":{"password":"<base64-new-password>"}}
```

This patch changes only the password key and requires no read permission on
the target Secret. If the Secret does not yet exist, the API returns
`404 NotFound`; set `pending.stage` to `"waiting-secret"` and requeue with
exponential backoff and jitter.
Do not issue another Vault credential while the pending lease remains valid.

Because the patch is intentionally blind, the target Secret must have one
authoritative writer.

After the API server accepts the password patch, persist
`pending.stage` as `"password-patched"`, set `pending.nextActionAt` to a few
seconds in the future (for example, five seconds), and return a timed
requeue. Do not block the worker with a sleep. The next reconciliation resumes
the same pending rotation and performs the username patch. If the controller
restarts after the password patch, it must honor the persisted
`nextActionAt`; the password is not needed to perform the username patch.

If the password-to-username stage deadline expires, retry the username patch
with backoff. If the pending lease is expired or within the configured safety
margin, do not continue the old attempt indefinitely; start replacement
issuance with backoff and repeat the credential sequence as needed.

Kubernetes RBAC does not provide field-level or prefix-based restrictions. The
target namespace `patch` grant therefore allows the controller to patch any
Secret in that namespace, while the controller itself targets only the Secret
name read from the selected `Cluster` reference. An admission policy is needed
if the cluster must also prevent modifications to other Secret names or fields
and should be considered mandatory for installations where another Secret in
the namespace is sensitive. The controller still must not use `get`, `list`, or
`watch` on target Secrets.

### 4. Patch the username in the `Cluster` resource

After the short timed requeue following the password patch, patch the username
in the configured external cluster entry. This is deliberately the last
credential change. If reconciliation resumes with `pending.stage` already set
to `"password-patched"`, skip the password patch; if `nextActionAt` is still
in the future, requeue for the remaining duration.

The controller should construct the patch using the configured external
cluster name rather than assuming a fixed array index. It should also avoid
rewriting unrelated `Cluster` fields owned by CloudNativePG or GitOps.

The username change causes CloudNativePG to reconcile the external connection
configuration. The resulting PostgreSQL reload causes the WAL receiver to
disconnect and retry with the new username and the password from the Secret.

After the username patch, set `pending.stage` to `reconnect-pending`, set
`pending.nextActionAt` to the verification time (typically two to five minutes
later), set the reconnect stage deadline, and requeue. After verification,
clear `nextActionAt`, set the stage to `verified`, and start lease revocation.

The controller does not need to force a second Secret reload. The Kubernetes
API server acknowledges the Secret patch before the username patch is sent; the
CloudNativePG cache may still briefly lag, but PostgreSQL's connection retry
behavior provides eventual convergence.

### 5. Verify replication

Do not revoke the old lease until the new connection is confirmed. Verification
should not block a worker with a long sleep. Requeue until the persisted
`pending.nextActionAt` is reached, typically two to five minutes after the
username patch, and then read only resources the controller already has
permission to inspect:

- CloudNativePG `Cluster` status, including `currentPrimary`, `targetPrimary`,
  and `readyInstances`; and
- the designated-primary Pod's Ready condition; and
- the designated-primary Pod's CloudNativePG instance-manager
  `GET /pg/status` response, specifically `isWalReceiverActive`.

The required success check is that the expected designated primary reports
`isWalReceiverActive: true` after the username change, while the cluster remains
a replica and the designated primary is Ready. `currentPrimary` and
`readyInstances` are supporting topology and health signals, not the
authentication result. The controller obtains `/pg/status` through the
Kubernetes API server's `pods/proxy` subresource, as `kubectl cnpg status` does;
it does not connect directly to PostgreSQL or query `pg_stat_replication`.

The E2E test actor performs a separate SQL-level assertion and must not be
confused with this controller verification. Against the source primary it
checks `pg_stat_replication` for a row in `streaming` state. Against the
replica cluster's designated primary it checks `pg_is_in_recovery()` and
`pg_stat_wal_receiver` for an active streaming receiver, then verifies that a
source-side WAL/data marker becomes visible on the replica. These catalog-view
checks are the independent E2E replication-health gate; CNPG `Cluster` status,
Pod Ready, and instance-manager status remain supporting/controller workflow
signals. The fixture uses a version-aware SQL adapter so this invariant does
not depend on a CNPG status field that may be unavailable with PostgreSQL 19.

If the status endpoint cannot be read, or if `isWalReceiverActive` is false,
verification fails and the controller retains the pending/current lease state
according to the stage timeout and retries with backoff. It must not revoke the
old lease or commit the pending lease as current based only on unchanged
Cluster status.

During implementation testing, intentionally configure an invalid replication
password and confirm that the controller detects that replication did not
resume. This negative test is required to demonstrate that the verification
logic observes `isWalReceiverActive: false` when authentication fails, rather
than merely accepting an unchanged Ready or replica status.

The corresponding E2E fixture assertion must also observe that the source's
`pg_stat_replication` row is no longer streaming and that the replica's
`pg_stat_wal_receiver` is not actively streaming. The fixture performs these
queries; the controller still does not connect to PostgreSQL.

### 6. Revoke and commit

If `currentLeaseID` is present, revoke the old dynamic credential using its
full Vault `lease_id`, not only the generated username. Treat an
already-revoked lease as success for recovery purposes. On the first rotation
of a newly provisioned Cluster, `currentLeaseID` may be empty because the
initial username/password were provisioned outside Vault; skip old-lease
revocation in that case.

After revocation, update the cluster entry in the state Secret:

- copy the pending lease ID and expiration to `currentLeaseID` and
  `currentExpiresAt`;
- clear the pending object;
- record the event fingerprint in `lastEvent`.

If revocation fails after replication has been verified, keep the new
credential active, leave `currentLeaseID` unchanged, and retry revocation
independently while the new credential remains in `pending`. A Vault cleanup
failure must not roll back a working replication connection.

## Failure recovery

| Failure point | Recovery behavior |
| --- | --- |
| Vault issue fails | Leave the existing Secret, username, and lease unchanged; retry the event. |
| Crash after Vault issuance but before its lease is persisted | The lease is untracked; a retry may issue another lease. Its lifetime is bounded by the configured maximum TTL, and it expires without renewal. Exact once-only issuance is not guaranteed across Vault and Kubernetes. |
| Crash before Vault issuance begins | The old credential remains active; retry the rotation normally. |
| Crash after pending state is written but before the password patch | The password is no longer available; revoke the pending lease and issue a replacement. |
| Crash after the password patch | Resume using the pending lease and username; no password read is required. |
| Crash while waiting for a timed action | The queue timer is lost, but the persisted stage, `nextActionAt`, and `stageDeadline` survive; requeue for the remaining delay or retry with the restarted backoff. |
| Password patch fails | Keep the old username and lease; retry the password patch. |
| Target Secret is absent | Record `WaitingForCredentialSecret`, retain the valid pending lease, and retry with exponential backoff. |
| Password patch succeeds, username patch fails | Retry the username patch using the same pending lease. The old username may temporarily have the new password. |
| Username patch succeeds, reconnect is not observed | Keep both leases; retry verification with backoff until the stage deadline. If the pending lease expires or the deadline is reached, issue a replacement and repeat the credential sequence. |
| Crash after old lease revocation | Recognize the pending lease and treat an already-revoked old lease as complete. |
| Old lease revocation fails | Keep the new connection; retry revocation through cleanup. |
| Cluster is promoted during rotation | Stop replica-specific mutation, skip username changes, revoke pending/current replication leases when no longer needed, and require a new lease after any future re-demotion. |
| Namespace is removed from `WATCH_NAMESPACE` | Leave the state entry and all known leases untouched. Do not run orphan cleanup until the namespace is watched again. |
| No qualifying event for 32 days | The current Vault credential can expire; expose its remaining lease time and alert on the affected replica Cluster because lease renewal is intentionally not implemented. |

## Monitoring and metrics

The controller exposes the standard controller-runtime, client-go, and Go
process/runtime metrics already provided by its dependencies. Do not create
custom equivalents for reconciliation count, reconciliation failures or
latency, workqueue depth/retries, Kubernetes API request behavior, process
health, or runtime resource usage. The exact built-in metric names are verified
against the pinned controller-runtime version during implementation.

The following small set of custom metrics covers behavior that those libraries
cannot observe:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `vault_replica_rotations_total` | Counter | `result`, `trigger` | Completed rotation outcomes. `result` is `success` or `failure`; `trigger` is a bounded value such as `initialization`, `topology_change`, or `pod_replacement`. |
| `vault_replica_vault_operations_total` | Counter | `operation`, `result` | Vault operations performed by the controller. `operation` is `issue` or `revoke`; `result` is `success` or `failure`. |
| `vault_replica_pending_workflows` | Gauge | `phase` | Number of non-terminal workflows currently in each bounded phase. |
| `vault_replica_current_lease_time_to_expiration_seconds` | Gauge | `namespace`, `cluster` | Remaining lifetime of the current Vault lease for each managed replica Cluster, calculated at scrape time from persisted expiration metadata. |

The lease metric intentionally has one identifying series per active managed
replica Cluster. Namespace and Cluster identity are required so an alert can
identify the database that needs attention; they must not be joined by
lease ID, username, password, Secret name, Pod UID, event fingerprint, Vault
path, or other unbounded or sensitive values. The series is removed when the
replica relationship is cleaned up. A missing current lease has no series,
while a value at or below zero represents an expired lease.

Per-Cluster identity, event fingerprint, phase detail, lease ID, and failure
diagnostics may be present in redacted structured logs or permitted Kubernetes
Events. They must not be used as metric labels, and passwords, Secret data,
Vault tokens, and dynamic usernames must never appear in logs, Events, metrics,
CRD status, or state.

Monitoring must alert on failed rotations, failed Vault operations, workflows
that remain pending beyond their phase deadline, and leases whose
`vault_replica_current_lease_time_to_expiration_seconds` value crosses the
configured safety margin. An expiring lease identifies a replica Cluster that
needs credential remediation; metrics do not themselves initiate a database
restart.

## RBAC model

### Target namespace Role

Create one `Role` in every approved replica-cluster namespace and bind it to
the controller ServiceAccount from `cnpg-system`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: vault-replica-controller
  namespace: reporting
rules:
  - apiGroups: ["postgresql.cnpg.io"]
    resources: ["clusters"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/proxy"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: vault-replica-controller
  namespace: reporting
subjects:
  - kind: ServiceAccount
    name: vault-replica-controller
    namespace: cnpg-system
roleRef:
  kind: Role
  name: vault-replica-controller
  apiGroup: rbac.authorization.k8s.io
```

Repeat the `Role` and `RoleBinding` only for the approved namespace list.
Do not use a `ClusterRoleBinding` for this controller.

The target Secret name is read from the selected external-cluster entry in the
current `Cluster` object. The target namespace Role intentionally grants
`patch` on any Secret in that namespace because Kubernetes RBAC cannot express
a name prefix or a dynamic `resourceNames` list. `get`, `list`, `watch`,
`update`, `create`, and `delete` remain absent. Pre-create each target Secret
through the database-provisioning workflow; that workflow may run after this
controller is installed. Installations that need to protect unrelated Secrets
should add a validating admission policy/webhook that constrains the
controller's Secret patches to approved names and the `data.password` field.

### `cnpg-system` Role

In `cnpg-system`, grant the controller normal namespaced Secret access so its
cache can read the pre-created state Secret, plus Lease access for leader
election:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: vault-replica-controller
  namespace: cnpg-system
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
```

The state Secret must be pre-created once if the controller is not allowed to
create Secrets. The Secret rule is intentionally namespace-wide because that is
the simplest way to use the normal controller-runtime cache; the controller
still writes only `vault-replica-controller-state`. The Lease rule is for the
single leader-election Lease in `cnpg-system`. No permission to read or watch
the CloudNativePG controller Deployment is required.

The controller's cache must not attempt to list or watch target credential
Secrets, because the controller intentionally has no `list` or `watch`
permission for them. Read the target Secret name from the `Cluster` CRD and use
a direct patch request. The state Secret is read and updated through the normal
client cache/client.

## Security considerations

- A write-only target Secret permission prevents the controller from reading
  the current password, but it can still overwrite the named Secret. Treat the
  controller as trusted for credential injection.
- Keep Vault passwords out of the state Secret, CRD status, Events, logs, and
  metrics.
- Use a Kubernetes Secret, not a ConfigMap, for lease state if lease IDs are
  treated as security-sensitive.
- Enable encryption at rest for Kubernetes Secrets.
- Give the controller's Vault identity permission to issue credentials for
  only the required database role and to revoke only the required leases.
- Use TLS and certificate validation for Vault. CloudNativePG remains
  responsible for the generated PostgreSQL connection configuration.
- Because the controller uses the normal `cnpg-system` Secret cache for
  simplicity, its ServiceAccount can observe Secrets in that namespace. Keep
  unrelated sensitive Secrets out of `cnpg-system`, or use a separate system
  namespace if that visibility is unacceptable.
- Ensure no GitOps reconciler or Secret operator writes the target credential
  Secret or the `Cluster` username. If two controller Pods are deployed for
  availability, leader election must leave only one active controller.

## Important limitations

### Event delivery is not a clock

The design assumes that the relevant Pod or `Cluster` event is observed. A
32-day Vault TTL gives recovery time for missed weekly events, but it does not
make an event-driven design safe indefinitely. Alert when the current lease is
approaching expiration. Lease renewal is deliberately outside the design; if
the controller disappears, Vault expiration is the fallback cleanup mechanism.

### Blind patching sacrifices conflict detection

The write-only Secret update uses a partial patch without first reading
`resourceVersion`. This avoids Secret read permission but also means the
controller cannot perform an optimistic-concurrency check. Enforce a single
writer or add an admission policy if this is not acceptable.

### CNPG status is obtained through the instance manager

The active WAL-receiver signal used by this design is not persisted in the
Cluster CRD's `status`. It is returned by the instance manager's `/pg/status`
endpoint and must be read through `pods/proxy`. This keeps the controller out
of PostgreSQL while adding a dependency on the CNPG status endpoint and its
corresponding Pod proxy permission.

### Promotion changes the contract

Once a replica cluster is promoted, it is no longer safe to blindly apply
replica-specific username changes. The controller must re-check replica status
at every mutation boundary and stop if promotion has occurred.

### No source-code modification is required

The controller relies on existing CloudNativePG behavior: a change to the
external cluster username causes CloudNativePG to reconcile the generated
replication configuration. The controller does not need to manage
`primary_conninfo` directly or restart PostgreSQL itself.

## Implementation checklist

- [ ] Define the controller configuration format and state Secret name
      (`vault-replica-controller-state`).
- [ ] Pre-create the consolidated state Secret. Target credential Secrets may
      be created later by each database-provisioning workflow.
- [ ] Install namespace-scoped `Role` and `RoleBinding` objects only in the
      namespaces listed by the CNPG controller's `WATCH_NAMESPACE` value.
- [ ] Set the controller's `WATCH_NAMESPACE` to the same value as CNPG's,
      preferably from a shared deployment configuration value.
- [ ] Install the `cnpg-system` Role for the consolidated state Secret.
- [ ] Add namespaced Lease permissions for controller-runtime leader election.
- [ ] Configure the controller cache for its `WATCH_NAMESPACE` list.
- [ ] Grant only `patch` on Secrets in each watched namespace; do not grant
      target Secret `get`, `list`, or `watch`. Read each target Secret name from
      the selected external-cluster entry in the Cluster CRD.
- [ ] Do not add target credential Secrets to the controller's watch set.
- [ ] Grant `get` on `pods/proxy` for the CNPG instance-manager status check.
- [ ] Implement new-cluster initialization with missing-Secret backoff.
- [ ] Implement Cluster-UID/restart-episode trigger IDs and event
      deduplication, together with controller-runtime work-queue serialization;
      do not add a separate distributed per-cluster lock.
- [ ] Implement pending-lease recovery after process crashes.
- [ ] Implement per-stage deadlines, retry backoff, Vault issue/lease-duration
      validation, lease revoke, expiration handling, and orphan cleanup. Do
      not implement database lease renewal.
- [ ] Expose only the domain-specific metrics in the monitoring contract;
      reuse controller-runtime, client-go, and Go process/runtime metrics for
      generic controller, queue, API, and process behavior.
- [ ] Expose per-replica lease time-to-expiration with namespace and Cluster
      identity labels, without lease IDs, usernames, passwords, Secret names,
      Pod UIDs, event fingerprints, or Vault paths.
- [ ] Implement password patch followed by username patch.
- [ ] Implement verification from the designated primary's `/pg/status`
      response and require `isWalReceiverActive`.
- [ ] Document the Vault authentication and database-role prerequisites,
      including PostgreSQL `LOGIN`/`REPLICATION` and source `pg_hba.conf`.
- [ ] Never remove a namespace from `WATCH_NAMESPACE` while it contains a
      managed Cluster; test restore-and-cleanup behavior if it is removed.
- [ ] Test node drain, Pod replacement, CNPG restart, failover, switchover,
      missed events, controller restart, Vault errors, and promotion during
      rotation.
- [ ] Add alerts for failed rotations, failed Vault operations, pending
      workflows past their deadlines, and leases nearing expiration.

## References

- [CloudNativePG replica clusters](docs/src/replica_cluster.md)
- [CloudNativePG bootstrap from another cluster](docs/src/bootstrap.md#bootstrap-from-another-cluster)
- [CloudNativePG kubectl status](https://cloudnative-pg.io/documentation/current/kubectl-plugin/)
- [CloudNativePG cluster API reference](docs/src/cloudnative-pg.v1.md)
- [Vault lease behavior](https://developer.hashicorp.com/vault/docs/concepts/lease)
- [Vault database secrets engine](https://developer.hashicorp.com/vault/docs/secrets/databases)
- [Vault database role API](https://developer.hashicorp.com/vault/api-docs/secret/databases)
- [Kubernetes API update and patch semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/#updates-to-existing-resources)
- [Kubernetes RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Kubernetes Deployments and Pod-template rollouts](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
- [Kubernetes Secrets](https://kubernetes.io/docs/concepts/configuration/secret/)
- [Kubernetes ConfigMaps](https://kubernetes.io/docs/concepts/configuration/configmap/)