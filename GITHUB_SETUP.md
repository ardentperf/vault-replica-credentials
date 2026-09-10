# GitHub administrator setup

This document is the handoff for enabling the repository features prepared by
the local implementation. These steps require GitHub administrator access and
are not part of the local implementation milestone.

## Before setup

Confirm that the repository contains and locally validates:

- the GitHub Actions workflow definitions;
- a stable aggregate `ci` workflow job that reports pass/fail after all
  required jobs;
- the local `act` event fixtures and commands;
- `renovate.json` and any Renovate custom managers;
- `CODEOWNERS`;
- the README test badge;
- `DESIGN.md`, `DESIGN_MAPPING.md`, `E2E_TEST_PLAN.md`,
  `IMPLEMENTATION_PLAN.md`, and `MONITORING.md`; and
- the Makefile targets used by CI.

No cloud credentials, GitHub OIDC provider, private registry credential, or
repository secret should be required for the build and test workflows.

Before enabling the workflow, validate both checked-in event fixtures locally:

```sh
act pull_request -e .github/workflows/events/pull-request.json \
  -W .github/workflows/ci.yml -j verify
act push -e .github/workflows/events/push.json \
  -W .github/workflows/ci.yml -j ci
```

Run the E2E job separately with the Docker network mode used by the local
runner. Exercise the aggregate `ci` job once with successful dependencies and
once with an intentionally failing dependency fixture; it must complete and
fail rather than be skipped.

## GitHub Actions

1. Enable GitHub Actions for the repository.
2. Permit the repository workflows and set the minimum required workflow
   permissions. The build and test jobs should not need write access to the
   repository, packages, deployments, or cloud resources.
3. Confirm that the workflow job names match the checks documented for branch
   protection.
4. Allow the E2E workflow to use the runner's Docker daemon and enough time and
   resources for two Kind clusters.
5. Confirm that failed E2E jobs upload only the redacted diagnostics described
   by the test plan.

The workflow must remain reproducible with `act`; GitHub-only authentication
must not be added merely to make the hosted workflow run.

## Teams and repository access

1. Create the project teams required by the organization's ownership model.
2. Grant each team the minimum repository permission it needs.
3. Map the responsible team or maintainers in `CODEOWNERS` if the repository's
   organization names differ from the committed file.
4. Confirm that at least one responsible team can review and maintain the
   controller, E2E infrastructure, and CI/dependency configuration.

Do not create teams or change organization membership as part of local
development.

## Branch protection

Configure protection for the default branch according to repository policy:

- require pull requests rather than direct pushes;
- require the stable aggregate `ci` workflow check listed in the CI
  definitions;
- optionally require individual checks in addition to `ci`, but do not make
  matrix-generated job names the only merge gate;
- require branches to be current before merge if appropriate;
- require code-owner review where the organization uses it;
- block force pushes and branch deletion; and
- enable the repository's approved merge queue or auto-merge settings if used.

The exact rule names and required-check names must be taken from the workflow
that is actually enabled. `act` can validate workflow behavior but cannot
enforce these repository settings.

## Renovate

1. Install or enable the Renovate GitHub App for this repository.
2. Grant only the repository permissions required to inspect dependencies and
   create/update Renovate branches and pull requests.
3. Confirm that Renovate reads the committed configuration and discovers Go,
   Docker, GitHub Actions, CNPG, Vault, PostgreSQL, Kind, kubectl, and custom
   shell-script dependency sources.
4. Enable the documented Renovate auto-merge policy only after required CI
   checks and branch protection are active.
5. Require the stable aggregate `ci` check, which includes all relevant checks
   including the full E2E suite, before merging a Renovate dependency pull
   request.
6. Keep major updates manual unless the project explicitly changes that policy.

The first Renovate pull request should be observed manually to confirm update
discovery, test execution, rebasing, and merge behavior. Renovate itself is not
required for local tests and must not be contacted by the controller or E2E
harness.

## README badge and final verification

After Actions is enabled, verify that the README badge points to the actual
workflow path and default branch. Open a small non-production pull request and
confirm:

1. all required checks start automatically;
2. the E2E job provisions and cleans up only its own Kind clusters;
3. redacted artifacts are available on failure;
4. branch protection blocks a failing or incomplete check; and
5. a Renovate dependency pull request follows the configured checks and
   auto-merge policy.

These are post-handoff verification steps, not prerequisites for local
Milestone 2 completion.
