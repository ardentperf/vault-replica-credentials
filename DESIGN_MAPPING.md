# DESIGN.md implementation mapping

This document is the implementation inventory extracted from `DESIGN.md`. It
is intentionally a mapping, not an implementation of reconciliation behavior.
The initial scaffold must not create, own, configure, or lifecycle-manage
CloudNativePG `Cluster` objects.

## Scope and boundaries

- One controller Deployment runs in `cnpg-system` in each Kubernetes region.
- `WATCH_NAMESPACE` is the only source of the approved replica-cluster
  namespaces. It is a required, comma-separated list and is parsed, trimmed,
  and validated at startup. Missing, empty, malformed, wildcard, or otherwise
  unrestricted values fail closed.
- `cnpg-system` is a separate cache/RBAC scope for controller state and leader
  election. It need not be included in `WATCH_NAMESPACE`.
- The controller does not watch or manage the CloudNativePG controller
  Deployment, target credential Secret events, PostgreSQL directly, or etcd.
- A target credential Secret is provisioned by another workflow. The
  controller never creates it and never reads its data.
- No Kubernetes finalizer is used for credential cleanup. Cleanup is driven by
  the periodic orphan sweep.

## Watchers and event sources

| Source and scope | Watch/use | Candidate work or constraint |
| --- | --- | --- |
| CloudNativePG `Cluster` (`postgresql.cnpg.io/v1`), each `WATCH_NAMESPACE` | `get`, `list`, `watch`; patch only after current-object validation | New replica Cluster, replica initialization, promotion/demotion, designated/current-primary topology changes, and cleanup. Generic status updates and the controller's own username patch must not independently trigger rotations. |
| CloudNativePG instance Pods, each `WATCH_NAMESPACE` | `get`, `list`, `watch` | Initial primary Pod, designated-primary deletion/replacement, in-place restart/container restart episode, and primary replacement caused by drain or maintenance. An old-Pod deletion is only a hint; act after the replacement/designated primary is observed. |
| Explicitly managed restart/failover signals, each `WATCH_NAMESPACE` | Optional watch only if a concrete signal resource is selected by a later implementation | The design permits explicitly requested restart, failover, or switchover signals but does not name a GVK or API. Do not invent one in the scaffold. |
| Consolidated state Secret `vault-replica-controller-state`, `cnpg-system` | Normal namespaced cache; `get`, `list`, `watch`, `update`, `patch` | Durable per-cluster lease/workflow journal. The cache permission is intentionally namespace-wide, but only the named Secret is written. |
| Leader-election Lease, `cnpg-system` | Controller-runtime leader-election coordination | One active reconciler; no separate distributed per-cluster lock. |
| Periodic orphan sweep | Every five minutes by default; fresh API read/list rather than cache-only evidence | Compare state identities to watched Cluster objects. Require confirmed 404/absence in two consecutive sweeps before revoking a deleted Cluster's leases. Never interpret a namespace absent from `WATCH_NAMESPACE` as deletion. |

The target credential Secret is deliberately not watched. A blind direct patch
and handling of `404 NotFound` as `WaitingForCredentialSecret` is the intended
way to notice a Secret created later. No Secret event should initiate a
rotation.

Candidate events must be mapped to the current `namespace/name` Cluster queue
key. Durable trigger identity includes the Cluster UID and the restart or
topology episode: replacement Pod UID, relevant in-place restart count, or an
appropriate CloudNativePG primary-transition identity. Duplicate observations
resume the existing pending workflow; they do not issue another lease.

## Kubernetes API interactions

| Interaction | Scope and semantics |
| --- | --- |
| Fetch current Cluster before mutation | Read the current object and confirm replica mode, designated-primary relevance, configured external-cluster name, and target Secret name. Re-check replica mode immediately before the username patch and at every mutation boundary. |
| List/read Cluster objects for startup and orphan sweeps | Only namespaces currently in `WATCH_NAMESPACE`; stale-incarnation handling compares Cluster UID before replacing state identity. A fresh read and two consecutive confirmed absences are required for deletion cleanup. |
| Observe Pod objects | Read designated-primary Ready state and topology/restart identity. Pod Ready is supporting evidence only, never authentication proof. |
| Patch target credential Secret | Read the Secret name and password key from the selected external-cluster entry in the current `Cluster`, then issue a direct JSON merge patch changing only that key. The target namespace permission is `patch` on any Secret; no `GET`, list, watch, create, update, or delete is allowed. A 404 enters `waiting-secret` and keeps the valid pending lease until expiry. |
| Patch Cluster username | Patch only `externalClusters[].connectionParameters.user`, selecting the configured external-cluster entry by name rather than array index. This is after the password patch and propagation requeue; unrelated CNPG/GitOps-owned fields must remain untouched. |
| Read instance-manager status | Kubernetes API server `GET` through `pods/proxy` for the designated-primary Pod, requesting `/pg/status`; require `isWalReceiverActive: true`. An unavailable endpoint or false value fails verification and retries. Do not use direct PostgreSQL, `pg_stat_replication`, unchanged Ready, or unchanged Cluster status as proof. |
| Read/update/patch state Secret | Decode/write compact `state.json` under `data`, never passwords. Persist lease/workflow transitions immediately after side effects, retry resource-version conflicts, and reject decoded state over the 256 KiB application ceiling. |
| Leader-election coordination | Use the namespaced Lease in `cnpg-system`; no ClusterRole or cluster-wide lock. |
| Emit Events or metrics | Emit state transitions, identity/fingerprint, phase, lease expiration, and failure reason without Secret data or passwords. The design's example RBAC permits namespaced Kubernetes Events; metrics may be used instead/also. |

The workflow order is: issue Vault credential, persist pending lease, patch
password, wait a short timed requeue, patch username, wait/requeue for
verification, verify WAL receiver activity, revoke the previous lease, then
promote pending lease to current and record the committed event fingerprint.
All post-issuance steps are repeatable `ensure` operations. No long blocking
sleeps or unmanaged background rotation goroutines are allowed.

## Required Kubernetes permissions

The ServiceAccount is created in `cnpg-system` and is bound with separate
namespaced Roles only; there is no ClusterRoleBinding.

### Role in every approved target namespace

```yaml
apiGroups: ["postgresql.cnpg.io"]
resources: ["clusters"]
verbs: ["get", "list", "watch", "patch"]
```

```yaml
apiGroups: [""]
resources: ["pods"]
verbs: ["get", "list", "watch"]
```

```yaml
apiGroups: [""]
resources: ["pods/proxy"]
verbs: ["get"]
```

```yaml
apiGroups: [""]
resources: ["events"]
verbs: ["create", "patch"]
```

```yaml
apiGroups: [""]
resources: ["secrets"]
verbs: ["patch"]
```

The target Secret name and password key are read from the selected
`Cluster.spec.externalClusters[].password` reference. Kubernetes RBAC cannot
express a name prefix or a dynamic `resourceNames` list, so this Role permits
`patch` on any Secret in each watched namespace. It grants no Secret read,
list, watch, create, update, or delete permission. Kubernetes RBAC also cannot
restrict the patch to `data.password`; installations that need protection for
unrelated Secrets or fields should add a validating admission policy/webhook.

### Role in `cnpg-system`

```yaml
apiGroups: [""]
resources: ["secrets"]
verbs: ["get", "list", "watch", "update", "patch"]
```

```yaml
apiGroups: ["coordination.k8s.io"]
resources: ["leases"]
verbs: ["get", "list", "watch", "create", "update", "patch"]
```

The state Secret rule is intentionally namespace-wide to support the normal
controller-runtime cache. The controller still writes only
`vault-replica-controller-state`. No permission to read/watch the CNPG
controller Deployment is required.

## Vault interactions

| Interaction | Contract |
| --- | --- |
| Authenticate/maintain controller Vault session | The configured auth method must work unattended for the controller lifetime. The client may re-authenticate or maintain its own token. The design does not select a particular auth endpoint. |
| Issue database credential | Read the configured database role endpoint, for example `GET /database/creds/<role>`. Consume `username`, `password`, `lease_id`, and `lease_duration`. Persist the lease immediately, but keep the password only in process memory until the target Secret patch succeeds. |
| Validate lease duration | Treat returned `lease_duration` as authoritative and reject a value below the configured minimum. Do not silently proceed with a short lease. The role targets `default_ttl == effective max_ttl`, normally `768h` (32 days). |
| Revoke lease | Revoke the full Vault `lease_id` for old current and pending credentials. Already-revoked leases are successful for recovery; failed revocations are retried independently without rolling back verified replication. |
| Lease renewal | Not implemented. Expiration is the fallback cleanup for an untracked lease/controller outage, and alerting is required as leases approach expiry. |

The Vault identity must be limited to issuing the configured database role and
revoking the required leases. Vault passwords, lease state, and Secret data
must not appear in logs, Events, metrics, CRD status, or Kubernetes state.

## Configuration inventory

| Value | Source/contract | Initial scaffold treatment |
| --- | --- | --- |
| `WATCH_NAMESPACE` | Required comma-separated approved namespaces; trim and validate; never wildcard/unrestricted | Required configuration; startup fails closed on invalid input and logs only the resulting namespace set. |
| System namespace | Fixed architecture namespace `cnpg-system` | Defaulted to `cnpg-system`; exposed as a configuration field for testability, with validation. |
| State Secret name/key | `vault-replica-controller-state` / `state.json` | Defaults from the design; configurable only as an explicit installation setting. |
| Target credential Secret name/key | Read from each Cluster's selected `externalClusters[].password.name` and `.key` reference; `password` is the expected key for this design | The future controller derives the reference from the current Cluster and blindly patches only that key. The target Role intentionally permits patching any Secret in each watched namespace because the name is dynamic. The scaffold does not read or watch target Secret data. |
| Vault address and authentication settings | TLS-validated Vault endpoint and unattended auth method are prerequisites; E2E may explicitly allow plain HTTP for the ephemeral dev Vault | Configuration placeholders/interfaces only; no auth or Vault side effect is implemented in this scaffold. |
| Vault database role/path | Derive the role from the selected source CNPG Cluster name, addressed as `/database/creds/<source-cluster-name>`; the source external-cluster entry uses that same stable name | No global role setting is used. The future Vault client derives the per-source path from the current Cluster reference; no issuance is implemented yet. |
| Minimum Vault lease duration | Configured lower bound for returned `lease_duration` | Duration configuration with a safe default matching the 32-day design target; future issuance code must enforce it. |
| Lease safety margin | Configurable margin before `expiresAt` | Duration configuration; future workflow uses it to decide replacement. |
| Password propagation delay | A few seconds; example is five seconds; persisted as `nextActionAt` | Duration default of five seconds; no timer/requeue behavior yet. |
| Verification delay | Typically two to five minutes after username patch | Duration default of two minutes; no verification behavior yet. |
| Stage deadlines | Suggested: one minute for issue/persist and password-to-username transition; five minutes for reconnect verification | Duration defaults; no deadline/retry behavior yet. |
| Orphan sweep interval | Example five minutes | Duration default of five minutes; no sweep behavior yet. |
| Required consecutive absences | Two confirmed absences before cleanup | Integer default of two; no cleanup behavior yet. |
| State size ceiling | 256 KiB decoded JSON application ceiling | Byte limit default of 256 KiB; future state writes must reject larger state. |
| Active workers / leader election | One worker initially; normal leader election if multiple replicas | Worker default one; leader election enabled by default for the `cnpg-system` Lease. |
| Metrics/probe endpoints | Health/readiness probes and metrics endpoint are operational scaffolding | Bind addresses are configurable; endpoints expose no credential data. |

The configuration package may expose these values for future implementation,
but the manager entrypoint must not issue Vault requests, patch Kubernetes
objects, or register a reconciliation controller in this scaffolding phase.

## State fields reserved by the design

The future state model may contain only the recovery information that cannot be
reconstructed from the Cluster: `version`, per-cluster `clusterUID`,
`currentLeaseID`, `currentExpiresAt`, `pending`, and `lastEvent`. Pending data
is limited to `leaseID`, `username`, `expiresAt`, `stage`, `stageDeadline`,
`triggerID`, and optional `nextActionAt`. Valid stages are `issued`,
`waiting-secret`, `password-patched`, `reconnect-pending`, `verified`, and
`replacement-backoff`. It must not store passwords, target namespace, external
cluster name, credential Secret name, issued timestamps, or current username.

## Explicitly out of scope

- Creating, updating, deleting, owning, or lifecycle-managing CNPG `Cluster`
  resources beyond the narrowly scoped future username patch described above.
- Creating or reading target credential Secret data. The future controller may
  blindly patch the CRD-referenced Secret name/key, but it must not read or
  watch the Secret.
- Watching target credential Secrets to initiate rotation.
- Watching or managing the CNPG controller Deployment.
- Direct etcd or PostgreSQL access, direct `primary_conninfo` edits, and
  database lease renewal.
- A Kubernetes finalizer, periodic password rotation loop, distributed
  per-cluster lock, unmanaged goroutine, or cluster-wide RBAC binding.

## E2E plan baseline

The detailed scenario, fixture, assertion, and teardown plan is in
[E2E_TEST_PLAN.md](E2E_TEST_PLAN.md). It is based on the clean upstream
`cloudnative-pg/cnpg-playground` checkout at
`1957b42b445532d284513964f53e3085b4f745f9`, not the local playground checkout.
The plan copies setup conventions only; this repository has no playground
dependency.