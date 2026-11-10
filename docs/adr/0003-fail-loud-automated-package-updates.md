# ADR-0003: Fail-Loud Automated Package Updates

**Status:** Accepted
**Date:** 2026-05-05, updated 2026-11-04 (`credential-approval` exported on all four systems, per ADR-0018); updated 2026-11-06 (`hyprscratch` exported on both Linux systems, per [nix-config #1004](https://git.alc.xyz/alcxyz/nix-config/issues/1004))
**Amended:** 2026-09-30; 2026-10-28 (the September 2026 follow-through folded into the decision; the platform table updated to the flake's current exports); 2026-11-03 (CI runs cheap checks only; native builds and provider verification run locally); 2026-11-10 (the fetch rule names what it covers, the fetch-only helper disables import from derivation, the provider contract states its limit)
**History:** [replaced text](archive/0003-fail-loud-automated-package-updates-history.md)
**Applies to:** `.forgejo/workflows/`, `scripts/update-packages/`, `scripts/forgejo/`, `scripts/ci/`

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

Native builds on the shared Forgejo CI runners were never reused. Job stores
are discarded, the Actions build cache was runner-local and isolated per pull
request, and hosts substitute only from public binary caches, so every CI
build was repeated at deployment. A complete export build also reached the
runners' one-hour job limit. Meanwhile the consumer's local package promoter
on the development host builds each new `dev` revision against the
configurations before moving the `promoted` branch that hosts and the
unattended T3 Code updater follow
([nix-config ADR-0067](https://git.alc.xyz/alcxyz/nix-config/src/branch/dev/docs/adr/0067-explicit-consumer-and-platform-validation.md),
[nix-config ADR-0080](https://git.alc.xyz/alcxyz/nix-config/src/branch/dev/docs/adr/0080-promoted-package-ref.md)).
The owner's fleet-wide CI decision of 2026-11-03 therefore limits the shared
runners to cheap checks and keeps native builds on the development host, where
deployments reuse their outputs.

## Decision

Automated package updates must fail loud and stop before producing invalid package state.

Updater scripts must:

- compute complete SRI hashes before patching package files
- exit non-zero if a required hash is empty, malformed, or unavailable
- validate upstream package layout assumptions before patching package files
- leave the changed derivation evaluable: every update PR's CI evaluates all
  exports on every platform, and the T3 Code, Stash and AI provider updaters
  also evaluate their package themselves; they never build a package natively
- realise flake installables only through `scripts/update-packages/fod-hash.sh`,
  which refuses, before building, any installable whose plan needs a
  derivation that is not fixed-output: `collect_fod_hash` collects a
  fixed-output dependency hash (such as a Cargo, pnpm or Go module hash) by
  building the fetch with a placeholder hash, and `realise_fetch_only` fetches
  a pinned source so a wrong or stale source hash fails before the update is
  committed, as the Claude Code and Codex updaters do. The helper evaluates
  with import from derivation disabled, because Nix realises such inputs
  during evaluation, before the plan it checks exists; none of the fetch
  targets needs it. Updaters may still hash upstream files directly with
  `nix store prefetch-file` or `nix-prefetch-url`, and run a hashing tool with
  `nix shell --option max-jobs 0`, which substitutes it and never builds
- avoid committing or refreshing a PR when validation fails
- use `lib.fakeHash` only as an explicit package-maintainer choice for an optional missing upstream asset, not as a fallback for failed required hashes

CI runs cheap checks only, on the shared runners. CI must:

- reject empty SRI hashes such as `hash = "sha256-";`
- test every Go tool module present under `tools/`
- run the script contracts directly, including the ones the flake also
  exposes as checks (`nix-gc-maintenance-contract`,
  `k8s-node-network-audit-contract`)
- evaluate every advertised package derivation on all four systems on every
  validation run, failing with the original diagnostic when any exported
  derivation cannot evaluate, including on non-native systems
- evaluate the flake's outputs without building (`nix flake check --no-build
  --all-systems`)
- evaluate the T3 Code provider contract
  (`scripts/ci/check-t3code-provider-versions.sh`): every provider derivation
  evaluates, and T3 Code's metadata names the packaged provider versions.
  The metadata reads those versions from the packages the flake passes in, so
  the comparison only catches a change to that wiring. Whether the built
  application really embeds those providers is checked only by a build,
  locally or by the promoter
- regression-test package selection, failure classification, the updaters and
  the merge queue with mocked Nix commands
- never run `nix build`, `nix-build`, a building `nix flake check` or a NixOS
  VM test, except the fixed-output fetches through `fod-hash.sh` above.
  Evaluating T3 Code imports its fetched source (import from derivation), so
  validation fetches that source through `realise_fetch_only` first and then
  evaluates with local builds disabled (`max-jobs = 0`): an evaluation that
  would build anything through import from derivation fails instead.
  `scripts/ci/check-no-ci-builds.sh` scans the workflows and every non-test
  script under `scripts/`, including helpers reached only through `source`,
  and rejects any mention of a local-only build script by file name, so a
  call through a variable counts too;
  the `tools/` contracts the workflows run are tests that put mocked Nix
  commands first on `PATH` and are not scanned
- preserve the required package-validation context through an aggregate job
  that succeeds only when the Go tests and the evaluation and script checks
  succeed
- keep unavailable optional assets out of package exports; required asset download failures stop an updater before package edits
- avoid stale references to deleted or retired packages

Native builds and provider verification run locally on the development
host: by the author before pushing a package change, and by the consumer's
local promoter for unattended updates. The promoter builds the AI package
stack and the consuming configurations against each new `dev` revision and
never advances `promoted` past one that fails, so automated update merges rely
on the cheap checks and the promoter is the build gate. The local commands are
in the [README](../../README.md#local-builds-before-pushing):

- `scripts/ci/build-changed-packages.sh` determines platform membership from
  exported attribute names before evaluating a derivation, builds every
  selected `x86_64-linux` export and evaluates the other systems, reporting
  them as derivation evaluation rather than a successful native build;
  evaluating another platform is never a fallback for a failed Linux
  evaluation or build
- it compares the branch against the target branch named in
  `GITHUB_BASE_REF` (`dev` for package work), or a conservative baseline when
  none is given; it expands
  the selection to all exports when root flake files or shared package inputs
  change, and selects package inputs together with their wrapper consumers,
  keeping those reverse dependencies aligned with `flake.nix`
- it builds only affected exports and their wrapper consumers by default,
  unsharded, and retains explicit baseline, combined baseline-and-selected,
  deterministic sharding and plan-only controls
- `scripts/ci/verify-t3code-providers.sh` builds T3 Code and the providers it
  embeds and checks their runtime version and closure references

Runner usage must:

- keep package automation scoped to `dev`; promotion from `dev` to `main` remains manual
- keep `dev` as a long-lived integration branch; manual promotion PRs must not delete it after merge
- use one stable `update/<package>` branch per package, except the grouped
  AI providers on `update/ai-tools` (ADR-0007), and refresh the existing open PR
  when a newer upstream version supersedes a stuck update
- treat closed `update/<package>` branches as disposable; stale remote update branches should be deleted because the updater can recreate them from current `dev`
- leave repository-level default branch deletion disabled so manual promotions do not delete `dev`
- cap scheduled package-update matrix parallelism so routine update checks do not saturate all shared runners at once
- serialize auto-merge passes so concurrent runs do not race to rebase or merge the same update
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
  occupying a runner while checks finish
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
| `claude-code`, `codex-app-server`, `codex-cli`, `credential-approval`, `deck`, `desk`, `forge-mirror`, `forgejo-deploy-key`, `gh-app`, `gh-app-gh`, `git-identity-guard`, `k8s-node-reboot`, `kdash`, `nix-deploy`, `nix-gc-maintenance`, `nixfleet`, `revkit`, `workmux` | All four |
| `deck-image`, `devlog`, `forgejo-runner-cancellation`, `forgejo-runner-orphan-check`, `hyprscratch`, `ndrop`, `openzfs_7_1`, `openzfs_7_3`, `stash`, `zfs-auto-unlock` | Both Linux systems |
| `herdr` | Both Linux systems, `aarch64-darwin` |
| `ghostty` | Both Darwin systems |
| `t3code`, `ai-stack-upstream` | `x86_64-linux` |
| `omniwm` | `aarch64-darwin` |
| `workspace-sync` | `x86_64-linux`, `aarch64-darwin` |
| `ledger-live`, `wcap`, `zen-browser` | `x86_64-linux` |

Herdr's pinned unstable SDK dependency rejects Intel Darwin; that export is
omitted until a supported dependency baseline is established.

Helium, previously the `default` export, was removed on 2026-10-12 after its
last consumer replaced it with Brave Origin (nix-config#628). The flake no
longer has a default package.

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
- **Keep native builds on the shared runners**: Rejected on 2026-11-03.
  Nothing reused their outputs, so each build was repeated at deployment, and
  a complete export build reached the runners' one-hour job limit even when
  split into two serialized shard jobs. The shared runner capacity buys no
  assurance the local promoter does not already provide.
- **Add a trusted binary cache fed by CI**: Not pursued. It would make CI
  builds reusable, but adds a service to operate and secure, while the
  development host already builds what deployments use.
- **Automatically retry inside the same updater run** — Rejected for malformed generated state. Retries are reasonable for network fetches, but once a script computes an invalid hash the safest behavior is to stop and make the failure visible.
- **Build a fixed package list in CI** — Rejected. Fixed lists drift as packages are added or retired, and they do not prove the changed package works.
- **Skip Darwin-only packages on Linux CI** — Rejected. Full Darwin builds are not available on the Linux runner, but metadata evaluation still catches missing attributes and obvious unsupported-system mistakes.
- **Advertise placeholder or dependency-incompatible exports** — Rejected. Exporting a name promises a usable derivation; missing optional assets remain absent until verified. Choosing a replacement ARM Linux default would change product intent without solving the missing artifact.
- **Build a fixed baseline on every pull request** — Rejected. It adds unrelated
  native builds to documentation, automation, and targeted package changes.
  Baseline mode remains available for explicit smoke testing, while ordinary
  validation follows the changed package graph.
- **Plain merge update PRs** — Rejected. Squash merging keeps routine generated updates to one commit per package update and matches the manual recovery process used when stale update PRs failed to auto-merge.
- **Skip stale-but-clean update PRs until a later updater run refreshes them** — Rejected. A successful update PR merge advances `dev`, which can make every other open update PR stale. Auto-merge should rebase those PRs instead of relying on manual repair.

## Consequences

- Bad generated hashes fail earlier and more clearly.
- Upstream package layout changes fail before a generated update PR is refreshed.
- CI catches what evaluation can: missing attributes, evaluation errors on any
  exported system, invalid hashes and broken automation. A package that
  evaluates but fails to build or run passes CI; for an automated update it is
  merged into `dev` and stopped by the local promoter, which leaves `promoted`
  on the last good revision until a fix lands.
- Package update automation trades some latency for lower runner pressure and lower PR noise.
- CI jobs no longer approach the runners' job-duration limit, and shared input
  changes no longer schedule a full native matrix there.
- Updaters still fetch fixed-output dependencies to learn their hashes. A
  package whose dependency fetch needs a local build (for example a fetcher
  that depends on a compiled part of the package) is refused and must be
  restructured or updated locally; Stash's Go module fetch skips the UI copy
  for this reason.
- Authors carry the native build: a package change pushed without the local
  build is caught only by the promoter after it merges.
- Darwin-only packages still need occasional real Darwin builds for full confidence.
- Evaluation covers every advertised system, but native builds on ARM Linux
  and Darwin still require their own builders or manual validation. Exports
  that no configuration consumes are built only by authors, since the
  promoter builds what the configurations use.
- Validating packages in the consuming flake's dependency context is the
  consumer's contract, recorded in nix-config ADR-0067.
- Adding a new updater script requires maintaining the same fail-loud hash validation behavior.
- Auto-merge can spend extra time waiting after a rebase because pull-request checks rerun on the refreshed head.
- Failed rebases or failed required checks leave the PR open for manual inspection instead of merging a stale or unverified update.
- Manual `dev` to `main` promotion cannot rely on the repository's default branch cleanup; only disposable `update/*` branches are deleted by automation.
