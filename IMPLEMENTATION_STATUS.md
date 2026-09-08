# Implementation status

Both completion gates in `IMPLEMENTATION_PLAN.md` passed locally on 2026-09-08.
The controller, ordered E2E suite, CI workflows, dependency automation and
administrator handoff are implemented. No required local gate remains open.

The acceptance matrix is in `ACCEPTANCE_MATRIX.md`. Gate results must record
actual commands and outcomes; a skipped or unavailable check is not a pass.

## Completion evidence (2026-09-08, Linux ARM64, Go 1.26.8)

- Final local `make ci` passed (`/tmp/vrc-ci-final-7.log`), including build,
  format/vet, unit/integration/race tests, coverage, manifests, shell/cleanup
  checks, image builds, vulnerability scanning, Renovate and monitoring rules.
- Clean `make e2e` passed all 12 ordered phases with default timings and no
  manual intervention (`/tmp/vrc-e2e-clean-3.log`, `artifacts/e2e.n5c2v6`).
  This includes RBAC, real SQL/WAL replication, regional Prometheus accuracy,
  invalid-password/restart safety, redaction and two-sweep deletion cleanup.
  Both run-owned Kind clusters and private kubeconfigs were automatically
  removed, and the command exited successfully at 12:23 UTC.
- The complete PR event workflow passed under `make act-check`: `fast`, all
  12 E2E phases, automatic owned-cluster cleanup and the stable `ci` aggregate
  succeeded (`artifacts/act/pull_request.log`, `artifacts/e2e.D7zjTM`, 13:23 UTC).
  Redacted artifacts are readable on the host after teardown, and the job's
  temporary Kind network attachment was removed.
- The complete push workflow also passed all three jobs and all 12 E2E phases
  (`artifacts/act/push.log`, `artifacts/e2e.5RXho1`, 14:23 UTC). Both run-owned
  clusters and the job's network attachment were removed. `make act-check`
  exited successfully after repeating the aggregate success/failure fixtures.
- Final artifact scans of all three successful full-suite runs found none of
  the known credential sentinels or generated Vault usernames. No Kind or act
  containers remain from those runs.
- `make aggregate-check` passed success and intentional-failure fixtures under
  act: the aggregate completed with failure rather than being skipped when a
  required dependency failed (`artifacts/act/aggregate-{success,failure}.log`).
- Failure and interruption tests execute the real harness cleanup trap at
  isolated command boundaries, covering TERM, partial creation, existing
  clusters, failed inventory and failed post-delete inventory checks.
- Seven rendered manifests validate with no skipped schemas. Renovate 44.69.8
  validates the configuration and dependency inventory without authentication.
  The Go vulnerability scan reports no vulnerabilities.
- Final tracked/new-file whitespace checks passed. Host `make tools`,
  `make ci` and `make aggregate-check` all passed consecutively after the final
  act regression runs (`/tmp/vrc-tools-final-reuse.log`,
  `/tmp/vrc-ci-final-7.log`, `/tmp/vrc-aggregate-final-reuse.log`).

## Development regressions retained

Regression tests cover defects found during live validation: pinned CNPG tool
selection, tainted-node probe placement, HTTPS instance-status proxy routing,
explicit CNPG namespace configuration, local-volume drain readiness,
post-promotion Vault connection retries, leader-restored metric readiness and
password-only Secret reloads. Recovery tests also cover journal-write failures,
unsettled target-primary transitions and redirect-safe write-only PATCH calls.

Earlier failed clean runs retained redacted diagnostics in
`artifacts/e2e.CJEyOr` and `artifacts/e2e.SWxl8P` and removed their owned clusters.
The password-reload regression was additionally verified in a focused live
run (`/tmp/vrc-negative-live-2.log`, 845.71s); its temporary wrapper and clusters
were removed before the successful clean completion run.

A host check after act exposed a root-owned, mode-700 generated runner cache.
The harness now builds privately and installs the binary with mode 755;
cleanup removes the private build output. A new stale/unreadable-cache fixture
failed before this fix and passes afterward, as does local `make ci`
(`/tmp/vrc-ci-final-6.log`). Controller code, workflow definitions and live E2E
scenarios are unchanged. Both event fast jobs then passed on the final harness
to exercise the changed real-harness build and cleanup boundaries under act
(`artifacts/act/pull_request-cache-fix.log`,
`artifacts/act/push-cache-fix.log`). Host tool installation, complete CI and
aggregate fixtures passed afterward, confirming the cache is reusable across
the root container and host user. The final runner is host-owned and mode 755.

## Scope boundary

GitHub administrator-owned enablement remains outside this implementation and
is documented in `GITHUB_SETUP.md`. No branch protection, repository settings,
Renovate installation, release automation or registry publishing was performed.
