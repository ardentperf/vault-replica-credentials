# Design acceptance matrix

This matrix is the executable inventory for `DESIGN.md`. Unit and focused API
tests use deterministic clocks and local HTTP servers; the ordered E2E phase
uses real Kubernetes, CloudNativePG, Vault, PostgreSQL, and Prometheus.

| Contract | Acceptance evidence |
| --- | --- |
| Parse a non-empty bounded `WATCH_NAMESPACE`; reject empty, malformed, duplicate, wildcard, and unrestricted values | `internal/config/config_test.go`; E2E phases 0, 5, and 6 inspect effective Deployment configuration |
| Require TLS for Vault except the explicit E2E HTTP opt-in; bound request timeout/retries | `internal/config/config_test.go`, `internal/vault/client_test.go` |
| Never log tokens, passwords, dynamic usernames, lease responses, or Secret data | Vault and API errors are body-free; `internal/vault/client_test.go`; `test/e2e/run.sh` redaction scan |
| Watch only CNPG Clusters and Pods in approved namespaces; never watch Nodes, Events, or target Secrets | `internal/controller/predicates.go`, manager wiring review, E2E RBAC/watch assertions |
| Ignore generic Cluster/Pod updates and the controller's own username patch | `internal/cnpg/cnpg_test.go`; ordered duplicate-observation assertion |
| Treat old Pod deletion as a hint and wait for an observable replacement primary | reconciler observation tests; E2E Pod deletion and node-drain scenarios |
| Blind-patch only the CR-referenced Secret key; never get/list/watch/create/update/delete target Secrets | exact verb/path/body test in `internal/kubernetes/api_test.go`; manifest and live `auth can-i` assertions |
| Patch the selected external-cluster username last without rewriting unrelated fields | exact JSON patch test; ordered reconciler test; E2E credential-pair assertion |
| Read WAL receiver state only via HTTPS `pods/proxy` `/pg/status` on port 8000 | exact proxy-path and response tests; E2E instance-status assertion |
| Issue a source-name-derived Vault role and reject malformed/short responses | `internal/vault/client_test.go`; E2E Vault role and lease inspection |
| Persist pending lease before Kubernetes mutation and never persist a password | `internal/controller/reconciler_test.go`, `internal/state/state_test.go`, E2E state/redaction assertions |
| Resume every durable stage; replace an unrecoverable pre-password credential after restart | restart boundary and ordered state-machine tests |
| Use persisted propagation/verification timestamps and timed requeues | ordered state-machine test with a controllable clock |
| Do not revoke the old lease until Pod Ready and WAL receiver activity are confirmed | verification failure regression tests and E2E invalid-password scenario |
| Deduplicate one topology fingerprint and distinguish Cluster UID reuse | trigger/predicate tests; ordered duplicate and stale-incarnation E2E assertions |
| Stop mutation on promotion and clean current/pending leases | promotion unit test; E2E standalone-promotion phase |
| Confirm deletion in two fresh sweeps, retry Vault cleanup, and ignore namespaces outside this controller's watch list | sweeper unit tests; E2E namespace removal/restoration and deletion phases |
| Expose only bounded domain metrics; compute lease expiry at scrape time and remove cleaned series | `internal/metrics/metrics_test.go`; regional Prometheus assertions |
| Reuse standard controller-runtime/client-go/Go metrics | manager integration and metrics exposition checks |
| Keep all retries and waits bounded and emit redacted failure diagnostics | Vault timeout/retry tests; E2E polling helpers and cleanup trap |
| Preserve the responsibility boundary: no direct PostgreSQL from the controller and no CNPG Cluster lifecycle ownership | package dependency/static review; SQL exists only in `test/e2e` |

The two completion gates are `make e2e` for Milestone 1 and `make ci && make
act-check` for Milestone 2. The GitHub workflow calls the same targets.
