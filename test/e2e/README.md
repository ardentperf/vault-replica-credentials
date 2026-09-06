# E2E test scaffold

The complete environment and scenario sequence is in
[`E2E_TEST_PLAN.md`](../../E2E_TEST_PLAN.md). The future runner provisions its
own `k8s-us` and `k8s-eu` Kind clusters, CNPG, one ephemeral HTTP Vault in the
US cluster, and this operator. It copies setup patterns from the upstream
CloudNativePG playground but does not use that repository as a runtime
dependency.

The operator currently has no reconciliation behavior. `setup.sh` provisions
the full infrastructure and asserts that no database Cluster resources exist;
the ordered scenario runner remains a later step because it depends on the
controller behavior in `DESIGN.md`.