# Implementation acceptance matrix

`DESIGN.md` is the behavior contract. Tests below exercise real pure logic,
the controller-runtime fake client, and HTTP servers at external boundaries.
The ordered E2E suite exercises released CNPG, Vault, PostgreSQL and Kubernetes.

| Requirement | Local acceptance test | Live acceptance |
| --- | --- | --- |
| Bounded namespaces, token, TLS, timeouts, defaults | `internal/config` validation tables | startup and watch-list transitions |
| Cluster and instance Pod watches only; own username ignored | `internal/cnpg` observation and predicate tests; cache tests | failover, deletion, drain, duplicate observations |
| Replica and distributed topology, stable names | observation tests | switchover, promotion, rebuild |
| Password-only blind Secret PATCH; named external username PATCH | `internal/kubernetes` HTTP protocol tests | impersonated RBAC assertions, initial issuance |
| Status through pods/proxy, strict active WAL receiver | HTTP status fixtures | invalid password, independent SQL/WAL markers |
| Vault response validation, TTL, redacted failures | `internal/vault` HTTP tests | real dynamic database engine |
| Compact v1 journal, size limit, conflicts, unknown versions | `internal/state` tests | state inspection and restarts |
| Persist before patch, password then username, durable delays | `internal/controller` reconciliation tests | initial issuance and restart checkpoints |
| Verification before revoke, retry revocation after crash | reconciliation failure tests | invalid password and lease lookups |
| Missing Secret, lost in-memory password, expiry replacement | reconciliation recovery tests | late Secret creation and restart |
| Topology changes and promotion during pending work | reconciliation safety tests | switchover and standalone promotion |
| Fresh reads, two sweep absences, stale UID cleanup | reconciliation sweep tests | deletion and namespace restoration |
| No cleanup outside watched namespaces | reconciliation scope tests | controller-only namespace removal |
| Bounded retries, no password/state/log leakage | reconciliation and HTTP failure tests | redacted diagnostics scan |
| Four custom metric families, scrape-time expiry, cleanup | `internal/telemetry` exposition tests | both regional Prometheus observers |
| Local build, format, vet, test, race, manifests, shell, security | Makefile targets | same targets in Actions/act |
| Complete serial clean-environment E2E and owned cleanup | E2E runner and cleanup checks | repeated clean runs |
| Stable CI aggregate succeeds/fails, never skipped | aggregate fixtures under act | administrator later requires `ci` |
| Dependency discovery and guarded update policy | local Renovate validation/discovery | administrator later enables app |

## Resolved implementation details

- CNPG distributed topology is a replica when `spec.replica.primary` differs
  from `spec.replica.self` (default: Cluster name). Standalone replica mode uses
  `enabled`. The source entry name selects `database/creds/<source>`.
- CNPG 1.30 serves instance status over HTTPS on port 8000. The Kubernetes
  Pod-proxy target is `https:<pod>:8000`; the controller still communicates
  only with the Kubernetes API server using its normal TLS authentication.
- The E2E harness explicitly installs CNPG's documented
  [`cnpg-controller-manager-config` ConfigMap](https://cloudnative-pg.io/docs/1.27/operator_conf/)
  with `WATCH_NAMESPACE` and rolls out CNPG. The released install renderer's
  flag alone was observed to omit the setting; live checks verify the ConfigMap.
- Under act's Docker bridge mode, only the current job container is attached
  to the Kind network and uses Kind's internal kubeconfig. The attachment is
  removed during teardown; no host networking or NodePort fallback is used.
- Initial issuance can precede the primary Pod: pg_basebackup cannot bootstrap
  using the dummy credential. Status is observed when available; successful
  status is required for verification, not for issuing bootstrap credentials.
  Existing relationships wait for a settled, Ready replacement primary.
- A trigger contains a hash of the selected relationship and a hash of the
  primary episode. Credentials and the controller-owned username are excluded.
  Initialization commits the final observed primary episode so bootstrap Pod
  creation does not cause an extra rotation.
- Before a password patch has a durable completion record, a restart replaces
  the pending credential after revocation. A crash between the password PATCH
  and its journal write has the same recovery. Vault issuance and its journal
  write cannot be atomic; an untracked lease expires at its Vault TTL.
- A reconnect deadline records failure. If status confirms the pending
  credential has no active WAL receiver, replace that failed attempt with
  bounded backoff. An unavailable status endpoint never authorizes revoking a
  possibly active credential. Other expired stages retry their required action.
- Unknown state versions fail closed; there is no historical schema to migrate.
  Installation seeds an explicit empty version 1 journal.
- When both source and replica are deleted, database-side revocation may fail
  because the database is gone. Cleanup retains lease IDs until Vault confirms
  revocation or the recorded lease expires; the test must preserve the source
  until revocation completes before deleting it. This ordering preserves the
  design's requirement not to discard live leases during a Vault outage.

Evidence and remaining gates are tracked in `IMPLEMENTATION_STATUS.md`.

## Live fixture regression notes

Password-only negative tests label credential Secrets `cnpg.io/reload: "true"`
to reload the external passfile, as specified by the
[CNPG plugin documentation](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.0/docs/src/kubectl-plugin.md).
`TestCredentialSecretReloadsPasswordOnlyChanges` prevents a silently stale
passfile; the live negative scenario requires sustained inactive WAL reception.
