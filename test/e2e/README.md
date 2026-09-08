# Ordered end-to-end suite

Run `make tools`, add `$PWD/.tools` to PATH, and run `make e2e` from
the repository root. The full scenario contract is in
[../../E2E_TEST_PLAN.md](../../E2E_TEST_PLAN.md).

The runner uses public, pinned images and local builds. It creates k8s-us and
k8s-eu with a control plane and three tainted PostgreSQL workers each. It
installs CNPG 1.30.0, one US dev Vault, regional controllers, and regional
Prometheus observers before creating any databases. No playground checkout or
GitHub/cloud credentials are used.

The SQL actor allocates db01 through db08 from a single counter and exercises
initial issuance, failover, Pod replacement, drain, distributed switchover,
standalone promotion, rebuilt replicas, a second namespace, controller-only
watch removal/restoration, restart and invalid-password recovery, and deletion.
It verifies pg_stat_replication, pg_stat_wal_receiver, recovery mode and
independent WAL markers. Controller timings remain at their design defaults.

Artifacts are redacted before writing to artifacts/e2e.*. Private kubeconfigs
are outside that tree. The EXIT/INT/TERM cleanup path deletes only clusters
created by this run and verifies removal. Existing clusters with either fixed
name cause preflight to fail. An explicit KEEP_E2E_CLUSTERS=true debug run may
retain the environment; CI never sets that option.

`make e2e-setup` checks infrastructure without creating databases. The
110-minute outer timeout includes normal two-sweep cleanup. The test preserves
the source until Vault has revoked the deleted replica's lease, since deleting
the source first prevents its PostgreSQL revocation statements from running.
Afterward it deletes the source and removes its Vault configuration.

CNPG's bounded watch list is set through its supported operator ConfigMap and
verified after rollout. During drain, the suite verifies the promoted primary
and its stream before uncordoning, then requires both instances Ready; Kind's
local-volume standby cannot move its existing PVC to another worker.

`make act-check` runs both complete event fixtures with a bind-mounted checkout
so redacted diagnostics survive the job container. The harness temporarily
attaches only that act job container to the Kind network and uses Kind's
internal kubeconfig; the workflow still uses Docker bridge mode. That network
attachment and private kubeconfigs are removed during ordinary teardown.

Pinned versions live in versions.sh and are discovered by Renovate. The actor
is a standalone Go program in runner; its subprocess and HTTP failures are
redacted, and its SQL privileges are never assigned to the controller.
