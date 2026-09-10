# End-to-end acceptance suite

`make e2e` is the serial, clean-host acceptance gate defined in
[E2E_TEST_PLAN.md](../../E2E_TEST_PLAN.md). It owns the two Kind clusters
`k8s-us` and `k8s-eu`; it refuses to reuse either name and deletes only
clusters it created. The run creates a unique artifact directory below
`test/e2e/artifacts` unless `E2E_ARTIFACT_DIR` is supplied.

Before a first local run, install the pinned tools:

```sh
bash test/e2e/install-tools.sh
make e2e
```

The runner records tool versions, rendered CNPG/Vault/controller manifests,
watch-list transitions, redacted logs, state snapshots, pod-proxy status
responses, and phase results. It uses the explicit test-only Vault token only
inside Kubernetes Secrets and removes it from retained artifacts. Set
`KEEP_E2E_CLUSTERS=true` only to inspect a failed run; the exact two owned
clusters remain available until manually removed.

`make e2e` applies a 110-minute outer timeout by default. Override it only
when investigating a slow local container runtime, for example
`make e2e E2E_OUTER_TIMEOUT=140m`; the per-phase waits remain bounded and the
cleanup trap still removes only `k8s-us` and `k8s-eu`.

The setup begins with the `e2e-bootstrap` namespace and asserts that there are
no database `Cluster` resources. Scenarios then allocate monotonically named
Clusters (`db01`, `db02`, ...) and execute in the accepted order. No
CloudNativePG playground checkout is used at runtime.
