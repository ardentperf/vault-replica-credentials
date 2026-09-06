# Vault replica credentials controller

This repository is the scaffold for an independent Kubernetes operator that
runs in `cnpg-system` beside CloudNativePG. The design-derived implementation
inventory is in [DESIGN_MAPPING.md](DESIGN_MAPPING.md); the full requirements
are in [DESIGN.md](DESIGN.md).

The current binary is deliberately non-reconciling. It validates a bounded
`WATCH_NAMESPACE` list, configures namespace-scoped manager cache and leader
election, and exposes health/metrics infrastructure. It does not create or
manage CloudNativePG `Cluster` resources, call Vault, watch target credential
Secrets, or mutate Kubernetes objects.

The detailed E2E design is in [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md). It uses
patterns from the upstream CloudNativePG playground but provisions its own two
Kind clusters and does not depend on a playground checkout.

The initial monitoring contract is in [MONITORING.md](MONITORING.md). It uses
standard controller-runtime/client-go metrics where available and adds only a
small set of domain metrics for rotations, Vault operations, pending workflows,
and per-Cluster lease time to expiration.

The implementation roadmap is in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md).
GitHub administrators should follow [GITHUB_SETUP.md](GITHUB_SETUP.md) after
the local CI and repository artifacts are ready.

## Local checks

```sh
make test
make build
```

For local E2E work, follow [E2E_TEST_PLAN.md](E2E_TEST_PLAN.md). The
`make e2e-setup` target provisions the complete two-region, no-database
environment and cleans it up when the setup process exits. Set
`KEEP_E2E_CLUSTERS=true` to retain run-owned clusters for inspection. The
ordered scenario runner will be enabled with reconciliation behavior.

The example manifests use `reporting` as one approved namespace. Copy the
target Role and RoleBinding for each namespace in the same namespace list used
by the CloudNativePG controller. The target credential Secret and the
`cnpg-system` state Secret are provisioned separately; this operator has no
permission to create the target Secret.
