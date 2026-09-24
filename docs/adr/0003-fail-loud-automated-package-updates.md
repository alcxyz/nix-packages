# ADR-0003: Fail-Loud Automated Package Updates

**Status:** Accepted
**Date:** 2026-05-05
**Updated:** 2026-09-24
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
- determine platform membership from exported attribute names before evaluating a derivation
- fail with the original diagnostic when any selected exported derivation cannot evaluate, including on non-native systems
- build every selected `x86_64-linux` export; evaluating another platform is never a fallback for a failed Linux evaluation or build
- evaluate selected exports on all other systems, reporting this as derivation evaluation rather than a successful native build
- expand selection to all exports when root flake files or shared package inputs change; select package inputs together with their wrapper consumers, keeping those reverse dependencies aligned with `flake.nix`
- regression-test package selection and failure classification with mocked Nix commands in CI
- evaluate every advertised package derivation on all four systems on every validation run, including default aliases
- build only affected exports and their wrapper consumers by default; retain
  explicit baseline, combined baseline-and-selected, and deterministic sharding
  controls for manual use
- expose a pre-Nix selection plan so workflow setup and cache work can be
  skipped when a change has no package validation candidates; normal validation
  must recompute and verify the selection rather than trusting the plan
- reuse standalone fixed-output dependencies through a directory-format Nix binary cache carried by
  the existing runner Actions cache; do not archive a live Nix store/database
- cache only newly built, derivation-backed content-addressed outputs without
  references, with fast zstd compression and a 2 GiB archive-directory limit;
  select outputs before export using 99% of that bound as a raw NAR budget,
  largest first with smaller outputs filling the remaining space; retain the
  actual archive-size check before upload
- restore dependencies only when T3 or the full package matrix is selected;
  the existing T3 updater also exports its validated dependencies before
  publishing an update PR, without adding another build
- preserve runner-local, pull-request-isolated cache writes; this primarily
  benefits repeated validation of the same PR on the same worker. Successful
  uploads alone do not establish reuse or warm-build performance, and cross-PR
  misses are expected without a trusted cache producer
- key cache snapshots by platform, flake lock, and package source inputs, with
  same-lock fallback; cache misses and cache-service failures must not bypass
  builds or fail an otherwise successful validation
- preserve the required package-validation context through an aggregate job
  that succeeds only when Go tests, validation prerequisites, package builds,
  and provider verification succeed
- keep unavailable optional assets out of named and default exports; required Helium asset download failures stop its updater before package edits
- compare pull request heads against the target branch, and use a conservative promotion baseline when the runner does not expose a target branch variable
- avoid stale references to deleted or retired packages

Runner usage must:

- keep package automation scoped to `dev`; promotion from `dev` to `main` remains manual
- keep `dev` as a long-lived integration branch; manual promotion PRs must not delete it after merge
- use one stable `update/<package>` branch per package and refresh the existing open PR when a newer upstream version supersedes a stuck update
- treat closed `update/<package>` branches as disposable; stale remote update branches should be deleted because the updater can recreate them from current `dev`
- leave repository-level default branch deletion disabled so manual promotions do not delete `dev`
- cap scheduled package-update matrix parallelism so routine update checks do not saturate all shared runners at once
- serialize auto-merge passes so concurrent runs do not race to rebase or merge the same update
- stop expensive package/provider commands when their pull-request head is
  superseded, using a bounded Git-ref watcher; do not depend solely on server
  concurrency support, and do not apply PR cancellation to `main` pushes
- run short auto-merge passes hourly throughout the day, with manual dispatch for recovery; ordinary pull request events trigger validation
- rely on pull request validation for automated update merges instead of running duplicate validation on every resulting `dev` push

Workflow-authored changes must trigger ordinary pull-request validation. Use an
explicit automation credential under a non-reserved Actions secret name for
publishing update branches/PRs and rebasing stale updates. Bind this credential
in the consuming step: the runner overwrites job-level `FORGEJO_TOKEN` with
its automatic token before applying step-level environment values. Forgejo's automatic
workflow token suppresses downstream workflow events; it is suitable for
workflow-local operations, but not these writes. Missing automation credentials
must fail rather than fall back to the automatic token. Credential provisioning
belongs to the hosting configuration, outside this public package repository.

Auto-merge must:

- only operate on `update/*` pull requests targeting `dev`
- refetch pull request state immediately before acting so one merged update does not leave the rest of the run working from stale base information
- finish eligible merges before rebasing stale update pull requests onto the resulting `dev` with Forgejo's pull request update API; refetch each deferred candidate and leave refreshed heads for a later pass instead of invalidating their checks with another merge in the same pass
- require the explicit CI contexts rather than the combined commit status;
  scheduled passes leave missing/pending checks for a later pass instead of
  occupying a runner while builds finish
- treat absent, null, and empty status lists as missing required checks;
  keep the affected PR blocked, continue checking other candidates, and report
  remaining blocked updates with a non-zero exit
- use squash merge for update pull requests and include the checked head commit ID in the merge request
- delete the update branch after a successful squash merge

The scheduled updater may retry on the next scheduled run from a clean checkout. It must not silently carry invalid generated state forward.

### Exported platform matrix

The flake explicitly exports the following combinations. "All four" means
`x86_64-linux`, `aarch64-linux`, `x86_64-darwin`, and `aarch64-darwin`.

| Packages | Exported systems |
|----------|------------------|
| `agent-sync-check`, `claude-code`, `codex-app-server`, `codex-cli`, `forge-mirror`, `k8s-node-reboot`, `kdash`, `nix-deploy`, `nix-gc-maintenance`, `xonsh-direnv`, `xonsh-with-direnv` | All four |
| `devlog`, `ndrop`, `openzfs_7_1`, `stash`, `zfs-auto-unlock` | Both Linux systems |
| `herdr` | Both Linux systems, `aarch64-darwin` |
| `helium`, `default` | `x86_64-linux`, both Darwin systems |
| `ghostty` | Both Darwin systems |
| `t3code`, `t3code-fork` | `x86_64-linux`, `aarch64-darwin` |
| `omniwm` | `aarch64-darwin` |
| `ledger-live`, `wcap`, `zen-browser` | `x86_64-linux` |

Herdr's pinned unstable SDK dependency rejects Intel Darwin; that export is
omitted until a supported dependency baseline is established. Helium's ARM
Linux asset remains an explicit, unexported placeholder. Enabling it requires
a verified artifact and an intentional matrix/test update. Every other Helium
asset is required by the updater, and the package expression rejects placeholder
hashes. The flake exposes Helium as `packages.<system>.default` only where it is
supported; ARM Linux has no default, rather than an unrelated replacement.

## Alternatives Considered

- **Use the automatic workflow token for update publication and rebases** —
  Rejected because its recursion protection suppresses the validation events
  needed by the merge gate. Explicitly dispatching a second validation path
  would add coordination and status-context complexity without improving the
  ordinary pull-request validation contract.
- **Retry only during the morning window or rebase before later merges**:
  Rejected after a green update repeatedly became stale as other updates merged.
  One accepted rebase did not advance its merge base within the bounded wait;
  a later successful rebase finished validation after the final morning pass.
  Hourly passes permit recovery without holding a runner for builds. Merging
  before rebasing prevents avoidable checks against intermediate bases. The
  consumer should poll after these passes, retaining its queue-drained gate.
- **Treat a missing status list as success or abort the entire queue** —
  Rejected. Missing checks provide no validation evidence, but one blocked
  candidate should not prevent independently green updates from progressing.

- **Let CI catch bad package expressions after PR creation** — Rejected as the only guard. CI remains a backstop, but updater scripts should fail before creating bad commits.
- **Automatically retry inside the same updater run** — Rejected for malformed generated state. Retries are reasonable for network fetches, but once a script computes an invalid hash the safest behavior is to stop and make the failure visible.
- **Build a fixed package list in CI** — Rejected. Fixed lists drift as packages are added or retired, and they do not prove the changed package works.
- **Skip Darwin-only packages on Linux CI** — Rejected. Full Darwin builds are not available on the Linux runner, but metadata evaluation still catches missing attributes and obvious unsupported-system mistakes.
- **Advertise placeholder or dependency-incompatible exports** — Rejected. Exporting a name promises a usable derivation; missing optional assets remain absent until verified. Choosing a replacement ARM Linux default would change product intent without solving the missing artifact.
- **Always shard selected package builds** — Rejected as the default. The
  September 2026 slowdown was dominated by a runner file-descriptor limit that
  made Nix process startup abnormally expensive. Sharding multiplied Nix
  installation and checkout overhead and prevented shared closures from being
  reused within one job. The selector retains manual sharding for future
  measured capacity needs.
- **Build a fixed baseline on every pull request** — Rejected. It adds unrelated
  native builds to documentation, automation, and targeted package changes.
  Baseline mode remains available for explicit smoke testing, while ordinary
  validation follows the changed package graph.
- **Restore a raw Nix store/database archive** — Rejected for this workflow.
  Directory-format binary caches use Nix's normal substitution and reference
  validation while keeping each job's installed store and database independent.
- **Archive complete application closures** — Rejected after a T3 pair produced
  a 5.1 GB cache and spent 3m20s exporting it before hitting the size bound.
  Standalone fixed-output dependencies avoid archiving application runtimes.
- **Add a separate binary-cache service immediately** — Deferred. Existing
  runner-local Actions caches provide reuse without another service to operate.
  Their host locality and transfer cost must be measured before expanding this.
- **Plain merge update PRs** — Rejected. Squash merging keeps routine generated updates to one commit per package update and matches the manual recovery process used when stale update PRs failed to auto-merge.
- **Skip stale-but-clean update PRs until a later updater run refreshes them** — Rejected. A successful update PR merge advances `dev`, which can make every other open update PR stale. Auto-merge should rebase those PRs instead of relying on manual repair.

## Consequences

- Bad generated hashes fail earlier and more clearly.
- Upstream package layout changes fail before a generated update PR is refreshed.
- Update PR checks are more relevant because they include the changed package.
- Package update automation trades some latency for lower runner pressure and lower PR noise.
- Darwin-only packages still need occasional real Darwin builds for full confidence.
- Evaluation covers every advertised system, but native builds on ARM Linux and Darwin still require their own runners or manual validation.
- Changes to shared package inputs build the full native matrix and can take longer than isolated package updates.
- Unsharded validation reuses one Nix store for affected packages and T3 Code
  provider checks and avoids repeated job setup. Documentation and general
  automation changes do not cause native package builds; provider-verification
  inputs still select both T3 Code exports, and applicable regression checks
  remain mandatory. A full shared-input change can still approach the runner
  limit, so explicit baseline and manual sharding support remain available.
- Each runner warms independently. Pull-request cache writes remain isolated
  to that PR; later revisions may reuse them, while other PRs may only reuse
  snapshots written by trusted update jobs or branch-push validation. Restored
  archives supply only requested store paths through a local substituter;
  normal upstream substitution and content verification remain enabled. A
  cache hit never replaces package or provider validation.
- Source-keyed snapshots avoid new copies for documentation/workflow changes
  and repeated runs of unchanged inputs. The existing runner cache retention
  still governs total disk use; the upload limit bounds each snapshot, not the
  entire cache. Fresh exports replace the restored snapshot rather than
  accumulating obsolete dependency versions. A complete T3/provider dependency
  set measured 3.13 GB, so output selection now happens before export: it
  prioritizes large eligible dependencies such as pnpm and skips candidates
  that do not fit. Raw NAR sizes are conservative and can exclude highly
  compressible outputs. Oversized actual snapshots are still skipped and
  builds remain correct.
- Adding a new updater script requires maintaining the same fail-loud hash validation behavior.
- Auto-merge can spend extra time waiting after a rebase because pull-request checks rerun on the refreshed head.
- Failed rebases or failed required checks leave the PR open for manual inspection instead of merging a stale or unverified update.
- Manual `dev` to `main` promotion cannot rely on the repository's default branch cleanup; only disposable `update/*` branches are deleted by automation.

## September 2026 follow-through

The [repository audit milestone](https://git.alc.xyz/alcxyz/nix-packages/milestone/284)
tracks gaps found while reviewing the producer and its configuration consumer:

- [Do not classify supported-platform evaluation errors as platform absence](https://git.alc.xyz/alcxyz/nix-packages/issues/320)
- [Make advertised package platforms and defaults usable](https://git.alc.xyz/alcxyz/nix-packages/issues/321)
- [Shard complete package validation within the runner limit](https://git.alc.xyz/alcxyz/nix-packages/issues/347)
- [Validate packages in the actual consumer dependency context](https://git.alc.xyz/alcxyz/nix-config/issues/275)

The proposed
[consumer-validation ADR](https://git.alc.xyz/alcxyz/nix-config/pulls/283)
records the additional producer/consumer contract. Existing fail-loud policy
remains accepted; the listed implementation gaps remain open until their
Forgejo issues are completed.

### Superseded build supervision

The package and provider commands run under a small supervisor on PR events.
It compares the checked-out event head to the current PR head ref every 30
seconds and terminates only its command process group when a newer head exists.
A stale command returns a nonzero status so it cannot publish a cache or make
the old validation aggregate green. The latest commit has its own checks.

Git lookup failures leave the build running, and ordinary command failures
retain their original status. Non-PR invocations execute directly. This keeps
the behavior independent of differences in server-side cancellation support
and needs no additional credential or API permission.

### Dependency-cache qualification

The live dependency-only probe restored T3's pnpm output with local builds
disabled and normal content verification enabled. Cold realization took 1m48s;
cache restore plus Nix realization took 19s. Export and upload cost 47s on the
first write. These are measurements from one runner, not a guaranteed build
latency; application compilation and runner contention remain separate costs.

### Bounded complete-export validation

A complete export build reached the runner's job-duration limit before provider
verification and cache export finished. Keep ordinary affected-package builds
unsharded. When the pre-Nix plan selects the complete matrix, split its sorted
exports across two deterministic shards. The second job depends on the first,
so the workflow graph enforces serialization even when Forgejo does not honor
matrix parallelism limits. Each shard evaluates every platform of its selected
exports, builds their native Linux outputs, and verifies the T3 Code flavor it
built. The all-export platform check and every other prerequisite still run, and the
aggregate validation context requires both shards. Each job retains the same
2 GiB dependency-cache limit and a distinct cache key.
