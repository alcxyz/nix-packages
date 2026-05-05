# ADR-0003: Fail-Loud Automated Package Updates

**Status:** Accepted
**Date:** 2026-05-05
**Applies to:** `.forgejo/workflows/update-packages.yml`, `.forgejo/workflows/ci.yml`, `scripts/update-packages/`, `scripts/ci/`

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

The scheduled updater may retry on the next scheduled run from a clean checkout. It must not silently carry invalid generated state forward.

## Alternatives Considered

- **Let CI catch bad package expressions after PR creation** — Rejected as the only guard. CI remains a backstop, but updater scripts should fail before creating bad commits.
- **Automatically retry inside the same updater run** — Rejected for malformed generated state. Retries are reasonable for network fetches, but once a script computes an invalid hash the safest behavior is to stop and make the failure visible.
- **Build a fixed package list in CI** — Rejected. Fixed lists drift as packages are added or retired, and they do not prove the changed package works.
- **Skip Darwin-only packages on Linux CI** — Rejected. Full Darwin builds are not available on the Linux runner, but metadata evaluation still catches missing attributes and obvious unsupported-system mistakes.

## Consequences

- Bad generated hashes fail earlier and more clearly.
- Upstream package layout changes fail before a generated update PR is refreshed.
- Update PR checks are more relevant because they include the changed package.
- Darwin-only packages still need occasional real Darwin builds for full confidence.
- Adding a new updater script requires maintaining the same fail-loud hash validation behavior.
