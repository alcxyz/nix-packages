# ADR-0003: Fail-Loud Automated Package Updates

**Status:** Accepted
**Date:** 2026-05-05
**Updated:** 2026-06-09
**Applies to:** `.forgejo/workflows/update-packages.yml`, `.forgejo/workflows/auto-merge-updates.yml`, `.forgejo/workflows/ci.yml`, `scripts/update-packages/`, `scripts/forgejo/`, `scripts/ci/`

## Context

Automated update workflows periodically check upstream releases, patch Nix package expressions, and open Forgejo pull requests against `dev`.

The update path must be conservative because a generated package expression can be syntactically valid enough to commit while still being semantically broken. One observed failure mode was an updater writing an empty SRI hash:

```nix
hash = "sha256-";
```

That value is invalid and should never reach a promotable branch. The prior CI shape also had two weaknesses:

- it built a stale, hard-coded package set that included retired tooling
- it did not reliably build the package changed by an update PR

This meant update PRs could fail for unrelated reasons or fail to prove that the updated derivation evaluated and built.

## Decision

Automated package updates must fail loud and stop before producing invalid package state.

Updater scripts must:

- compute complete SRI hashes before patching package files
- exit non-zero if a required hash is empty, malformed, or unavailable
- validate upstream package layout assumptions before patching package files
- build the changed derivation before opening or refreshing an update PR when the package has generated dependency state or native wrapper packaging
- avoid committing or refreshing a PR when validation fails
- use `lib.fakeHash` only as an explicit package-maintainer choice for an optional missing upstream asset, not as a fallback for failed required hashes

CI must:

- reject empty SRI hashes such as `hash = "sha256-";`
- test every Go tool module present under `tools/`
- build the changed `x86_64-linux` package when the runner can evaluate it
- validate Darwin-only package metadata when the Linux runner cannot build the package
- compare pull request heads against the target branch, and use a conservative promotion baseline when the runner does not expose a target branch variable
- avoid stale references to deleted or retired packages

Runner usage must:

- keep package automation scoped to `dev`; promotion from `dev` to `main` remains manual
- keep `dev` as a long-lived integration branch; manual promotion PRs must not delete it after merge
- use one stable `update/<package>` branch per package and refresh the existing open PR when a newer upstream version supersedes a stuck update
- treat closed `update/<package>` branches as disposable; stale remote update branches should be deleted because the updater can recreate them from current `dev`
- leave repository-level default branch deletion disabled so manual promotions do not delete `dev`
- cap scheduled package-update matrix parallelism so routine update checks do not saturate all shared runners at once
- avoid queueing stale auto-merge runs; a newer auto-merge event should replace an older waiting run
- trigger auto-merge from update pull request changes, with the scheduled auto-merge trigger kept as a nightly fallback
- rely on pull request validation for automated update merges instead of running duplicate validation on every resulting `dev` push

Auto-merge must:

- only operate on `update/*` pull requests targeting `dev`
- refetch pull request state immediately before acting so one merged update does not leave the rest of the run working from stale base information
- rebase stale update pull requests onto the current `dev` with Forgejo's pull request update API instead of skipping them indefinitely
- wait for the explicit required CI contexts, not the combined commit status, because the auto-merge workflow can post its own pull-request status while it is running
- use squash merge for update pull requests and include the checked head commit ID in the merge request
- delete the update branch after a successful squash merge

The scheduled updater may retry on the next scheduled run from a clean checkout. It must not silently carry invalid generated state forward.

## Alternatives Considered

- **Let CI catch bad package expressions after PR creation** — Rejected as the only guard. CI remains a backstop, but updater scripts should fail before creating bad commits.
- **Automatically retry inside the same updater run** — Rejected for malformed generated state. Retries are reasonable for network fetches, but once a script computes an invalid hash the safest behavior is to stop and make the failure visible.
- **Build a fixed package list in CI** — Rejected. Fixed lists drift as packages are added or retired, and they do not prove the changed package works.
- **Skip Darwin-only packages on Linux CI** — Rejected. Full Darwin builds are not available on the Linux runner, but metadata evaluation still catches missing attributes and obvious unsupported-system mistakes.
- **Plain merge update PRs** — Rejected. Squash merging keeps routine generated updates to one commit per package update and matches the manual recovery process used when stale update PRs failed to auto-merge.
- **Skip stale-but-clean update PRs until a later updater run refreshes them** — Rejected. A successful update PR merge advances `dev`, which can make every other open update PR stale. Auto-merge should rebase those PRs instead of relying on manual repair.

## Consequences

- Bad generated hashes fail earlier and more clearly.
- Upstream package layout changes fail before a generated update PR is refreshed.
- Update PR checks are more relevant because they include the changed package.
- Package update automation trades some latency for lower runner pressure and lower PR noise.
- Darwin-only packages still need occasional real Darwin builds for full confidence.
- Adding a new updater script requires maintaining the same fail-loud hash validation behavior.
- Auto-merge can spend extra time waiting after a rebase because pull-request checks rerun on the refreshed head.
- Failed rebases or failed required checks leave the PR open for manual inspection instead of merging a stale or unverified update.
- Manual `dev` to `main` promotion cannot rely on the repository's default branch cleanup; only disposable `update/*` branches are deleted by automation.
