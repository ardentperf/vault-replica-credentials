# Design implementation mapping

The behavior contract is [DESIGN.md](DESIGN.md). The executable acceptance
mapping is [ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md), and actual gate evidence
belongs in [IMPLEMENTATION_STATUS.md](IMPLEMENTATION_STATUS.md).

| Responsibility | Implementation | Verification |
| --- | --- | --- |
| Fail-closed namespaces, Vault token/TLS, timings | internal/config | configuration tables |
| Namespace-scoped cache and manager, single worker, leader election | internal/kubernetes/cache.go; internal/operator | cache tests and live RBAC |
| CNPG replica/distributed topology, predicates, event identity | internal/cnpg | observation tests and ordered failovers |
| Blind password-only Secret PATCH | internal/kubernetes/api.go | exact HTTP verb/path/body test |
| Username PATCH selected by external name, atomic version/name guards | internal/kubernetes/api.go | JSON Patch protocol test |
| Designated-primary status through pods/proxy | internal/kubernetes/api.go | HTTP fixtures and live invalid-password check |
| Dynamic issue and lease revoke, validated duration | internal/vault | httptest protocol/failure/timeout/redirect cases |
| Compact v1 journal and optimistic entry updates | internal/state | schema, size, stale-write and no-op tests |
| Ordered rotation, durable delays, restart recovery | internal/controller | fake-client reconciliation tests |
| Confirmed absence, promotion, stale UID, namespace scope | internal/controller | sweep/safety tests and ordered cleanup |
| Bounded custom telemetry, scrape-time expiry | internal/telemetry | exposition tests and regional Prometheus |
| PostgreSQL SQL provisioning and independent WAL assertions | test/e2e/runner only | ordered live suite |
| Local checks and CI orchestration | Makefile; scripts; .github/workflows | same targets outside and under act |

## Watch and permission boundaries

CNPG Cluster and instance Pod events are watched only in WATCH_NAMESPACE.
Generic status updates and the controller's own username changes are filtered.
Pod deletion is a hint: an existing relationship waits for its settled, Ready
designated primary. Initial issuance can precede pg_basebackup and a primary
Pod, because bootstrap cannot succeed using the deliberately dummy credentials.

No target Secret, Node, or Kubernetes Event informer is registered. The cache
refuses unregistered informer reads. System Secret and Lease cache scopes are
limited to cnpg-system. Fresh API reads protect mutation boundaries and orphan
absence confirmation.

The target namespace Role grants Cluster get/list/watch/patch, Pod
get/list/watch, pods/proxy get, Event create/patch, and Secret patch only.
The system Role grants Secret get/list/watch/update/patch and namespaced Lease
coordination. The controller creates no ClusterRoleBinding and never creates,
owns, or lifecycle-manages CNPG Cluster resources.

## Journal and recovery

The version 1 schema permits only Cluster UID, current lease ID and expiration,
pending lease metadata, and last committed trigger under namespace/name keys.
Pending metadata is limited to leaseID, username, expiresAt, stage,
stageDeadline, triggerID and optional nextActionAt. There is no current
username, password, resource snapshot, retry counter, or heartbeat in state.
Unknown versions and fields fail closed; installation provides an empty v1
journal. The decoded application ceiling defaults to 256 KiB.

The controller persists a pending lease before mutation, patches the password,
persists its propagation delay, patches the username, and persists its
verification delay. Successful status verification precedes old-lease
revocation and final commit. Retries retain the journal and use bounded timed
requeues. Restart before durable password-patch completion revokes/replaces
the unrecoverable credential; later stages resume without reading the Secret.
Cross-system issuance/persistence cannot be atomic, and untracked leases are
bounded by their Vault TTL.

Promotion cleanup waits for the primary to stop using its WAL receiver.
Deletion cleanup requires two consecutive fresh sweep absences. State for
removed namespaces is untouched. A stale Cluster UID is cleaned before a
new incarnation can inherit that name.

## Explicit exclusions

The controller has no PostgreSQL client, database lease renewal, periodic
rotation, finalizer, cloud dependency, direct etcd access, CNPG Deployment
watch, or target Secret read. PostgreSQL connections, gateways, database
provisioning, source role setup and watch-list changes belong to the E2E actor
or installation owner.

The repository prepares CI, dependency automation and administrator documents.
It does not publish images, create releases, configure GitHub branch rules,
install Renovate, or grant repository permissions.
