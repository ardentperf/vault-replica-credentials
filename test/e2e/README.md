# End-to-end tests

The complete environment and scenario sequence is in
[`E2E_TEST_PLAN.md`](../../E2E_TEST_PLAN.md). `run.sh` provisions its own
`k8s-us` and `k8s-eu` Kind clusters, CNPG, PostgreSQL 18, one ephemeral HTTP
Vault in the US cluster, regional controllers, gateways, and Prometheus
observers. It then runs every phase in the documented order.

Install the pinned local tools and run the complete gate with:

```sh
bash test/e2e/install-tools.sh
make e2e
```

`setup.sh` is also available through `make e2e-setup` when only the validated
no-database environment is needed. Both entrypoints refuse to reuse their two
reserved Kind names and clean up only clusters created by that invocation.
Set `KEEP_E2E_CLUSTERS=true` for local debugging. Redacted diagnostics are
stored in `.artifacts/e2e`, and ephemeral kubeconfigs are removed during normal
cleanup.
