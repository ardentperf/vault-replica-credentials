# Monitoring

This document describes the initial operational monitoring contract for the
Vault replica-credentials controller. It intentionally starts small. Generic
controller, queue, Kubernetes API, and process/runtime behavior comes from
controller-runtime, client-go, and the standard Go collectors. The exact
built-in metric names must be verified against the pinned library versions;
the controller must not duplicate them.

## Custom metrics

The controller exposes these domain-specific metric families:

| Metric | Type | Labels | Purpose |
| --- | --- | --- | --- |
| `vault_replica_rotations_total` | Counter | `result`, `trigger` | Counts completed rotation outcomes. |
| `vault_replica_vault_operations_total` | Counter | `operation`, `result` | Counts Vault `issue` and `revoke` outcomes. |
| `vault_replica_pending_workflows` | Gauge | `phase` | Counts non-terminal workflows by bounded phase. |
| `vault_replica_current_lease_time_to_expiration_seconds` | Gauge | `namespace`, `cluster` | Reports remaining lifetime of each active replica Cluster's current Vault lease. |

Allowed values must be finite and documented. Suggested values are:

- rotation `result`: `success`, `failure`;
- rotation `trigger`: `initialization`, `topology_change`,
  `pod_replacement`;
- Vault `operation`: `issue`, `revoke`;
- Vault `result`: `success`, `failure`; and
- workflow `phase`: the finite workflow phases already defined by the design.

The lease metric is the identifying metric. It has one series per active
managed replica Cluster and uses `namespace` and `cluster` so an alert can
identify the affected database. Its value is calculated at collection time
from the persisted expiration timestamp:

- positive: lease remains valid;
- zero or negative: lease has expired;
- no series: there is no current lease for that replica relationship.

Do not add lease IDs, usernames, passwords, Secret names, Pod UIDs, event
fingerprints, Vault paths, error strings, or arbitrary resource fields as
labels. This prevents both credential leakage and unbounded cardinality.

## Alert starting points

These are initial alert expressions, not additional controller behavior.
Thresholds and `for` durations should be tuned after observing normal
controller timing.

```promql
increase(vault_replica_rotations_total{result="failure"}[10m]) > 0
```

This detects any failed rotation in the last ten minutes.

```promql
increase(vault_replica_vault_operations_total{result="failure"}[10m]) > 0
```

This detects any failed Vault operation in the last ten minutes.

```promql
vault_replica_pending_workflows > 0
```

Use a Prometheus alert `for` duration longer than the normal phase deadline
for the pending-workflow rule.

```promql
vault_replica_current_lease_time_to_expiration_seconds < 86400
```

This identifies each replica Cluster with less than 24 hours remaining.

```promql
vault_replica_current_lease_time_to_expiration_seconds <= 0
```

This identifies each replica Cluster whose current lease has expired.

Alert annotations should include `namespace` and `cluster` for lease alerts.
They should not include credential values or Vault responses.

An expiring lease identifies a replica Cluster that needs credential
remediation. It does not, by itself, require or trigger a database restart.
The operator remains event-driven; lease renewal and scheduled rotation are
not implemented by this design.

## Logs and Kubernetes Events

Metrics are for aggregate health and alerting. Redacted structured logs or
permitted Kubernetes Events may carry the detailed context needed to diagnose
one workflow, including:

- namespace and Cluster identity;
- Cluster UID;
- bounded workflow phase;
- bounded failure stage;
- topology/event fingerprint where permitted; and
- lease expiration timing without the lease ID.

Never emit passwords, target Secret data, Vault tokens, dynamic usernames,
unredacted Vault responses, or lease IDs. Kubernetes Events are not controller
watch inputs and must never be used as a correctness dependency.

## Testing

Metric behavior is tested at three levels:

1. Unit tests verify metric increments, bounded label values, gauge cleanup,
   time-to-expiration calculation, and sensitive-value exclusion.
2. Integration tests scrape the controller endpoint after representative
   rotations, Vault failures, pending workflows, and cleanup.
3. The E2E actor queries both regional Prometheus instances and may scrape the
   controllers directly when diagnosing a shipping mismatch. The initial E2E
   environment does not need dashboards or Alertmanager. Alert expressions can
   be tested locally against fixture metric samples.

The same metric checks must run locally and in GitHub Actions. No cloud
monitoring service, GitHub OIDC, or GitHub-only credential is required.

## E2E Prometheus observer

The E2E environment installs one minimal, ephemeral Prometheus instance per
Kind cluster. Each instance scrapes only the local controller through a
namespaced ClusterIP metrics Service. The Prometheus image and scrape interval
are pinned and Renovate-managed. Storage is short-retention and disposable.

The E2E environment does not install Grafana, Alertmanager, Prometheus
Operator, dashboards, recording rules, or a cross-cluster monitoring stack.
Prometheus is an observer only; the controller must not call the Prometheus API
or depend on Prometheus for reconciliation correctness.

Before any database Cluster is created, the E2E harness verifies:

- Prometheus readiness;
- a healthy local controller target through `/api/v1/targets`; and
- successful queries for the controller's metrics through `/api/v1/query`.

For known test actions, the harness compares Prometheus query results with the
expected behavior:

- one expected counter increment for one successful rotation;
- Vault issue and revoke operation counts matching the action;
- pending workflow gauges returning to zero after completion;
- the lease time-to-expiration series carrying the correct namespace and
  Cluster labels;
- lease time-to-expiration decreasing across scrapes without a new event; and
- removal of the lease series after Cluster cleanup.

The harness uses a fresh baseline or Prometheus counter semantics across
deliberate controller restarts. It records scrape-target status, rendered
configuration, query responses, and redacted metric samples as artifacts. It
does not test dashboards.
