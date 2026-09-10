# E2E test runner

The complete environment and scenario sequence is in
[`E2E_TEST_PLAN.md`](../../E2E_TEST_PLAN.md). The runner provisions its
own `k8s-us` and `k8s-eu` Kind clusters, CNPG, one ephemeral HTTP Vault in the
US cluster, and this operator. It copies setup patterns from the upstream
CloudNativePG playground but does not use that repository as a runtime
dependency.

`setup.sh` provisions and validates the clean no-database infrastructure.
`run.sh` retains that run-owned environment long enough to execute the ordered
scenario actor in `scenarios.sh`, then collects redacted diagnostics and
deletes only the two named Kind clusters. SQL catalog queries are made only by
the actor, never by the controller.

The actor creates source and replica fixtures from the templates in
`fixtures/`, installs the test-only host-network gateways from
`manifests/gateway.yaml`, configures the ephemeral Vault database engine, and
executes the ordered failover, replacement, drain, switchover, rebuild,
namespace-scope, invalid-password, restart, Prometheus, and orphan-cleanup
assertions. Every wait is bounded and failures leave scrubbed evidence in the
artifact directory.

Required host tools are Docker, Kind, kubectl, the released `kubectl cnpg`
plugin, Go, curl, and jq. Run `make e2e` for the complete suite, or
`KEEP_E2E_CLUSTERS=true make e2e-setup` to inspect only the infrastructure
phase. The setup script refuses to reuse either run-owned cluster name.
