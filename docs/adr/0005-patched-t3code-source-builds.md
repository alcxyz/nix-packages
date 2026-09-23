# ADR-0005: Patched T3 Code Source Builds

**Status:** Accepted
**Date:** 2026-07-11
**Amended:** 2026-08-27
**Amended:** 2026-09-09
**Amended:** 2026-09-12
**Amended:** 2026-09-16
**Amended:** 2026-09-21
**Applies to:** `pkgs/t3code/`, `pkgs/codex-cli/`, package update automation

## Context

T3 Code needs changes that are maintained in a fork and are not present in the
official release artifacts. Packaging an upstream AppImage on Linux and DMG on
Darwin can produce matching display versions while silently omitting those
changes. The desktop and its bundled server also require exact protocol
compatibility, so mixing a patched server with an upstream desktop is unsafe.

Building the Electron monorepo from source exposed several cross-platform
constraints:

- pnpm's dependency verification may launch a nested networked install, which
  is incompatible with a deterministic Nix sandbox;
- the generic Vite task runner may repeat the same dependency check, so the
  package uses an explicit web, server, and desktop build sequence;
- release-version stamping must be idempotent and occur after native dependency
  rebuilds;
- generated Darwin application bundles have a different signing identity from
  official signed artifacts, so Electron Safe Storage data encrypted by one
  identity may not be decryptable by the other;
- interactive Wayland applications must be launched by the active compositor
  when a visible window is required; a background service is not a general
  substitute for compositor-native application launch;
- the model selected by T3 Code previously required a newer Codex CLI than
  npm's stable `latest` tag.

## Decision

Expose two source-built variants on `x86_64-linux`. `t3code` tracks a
pinned published upstream nightly. `t3code-fork` tracks the tested
`alcxyz/t3code` feature branch and applies a separate, narrowly scoped quota
recovery patch. A shared `source.json` records each flavor's exact commit,
source hash, and dependency hashes, together with the fork's tested upstream
baseline. Both variants use one shared build recipe so their provider wiring
and platform-specific installation stay identical.

On macOS, use the upstream developer-signed nightly app through the
`t3-code@nightly` Homebrew cask, declared by nix-darwin, with existing profile
wiring retained in Home Manager. Do not export Darwin T3 packages: Mac rebuilds
no longer consume them, and CI should not evaluate unused Darwin variants.
The native desktop uses its matching upstream server; it is not a client for a
locally patched server. This platform exception follows nix-config ADR-0073.

The fork's `feat/automatic-thread-titles` branch is a promotion ref rather than
an untested development head. Its sync workflow merges a candidate upstream
main revision, records that revision and the source version in
`.github/fork-source.json`, validates the candidate, and only then advances the
branch. Package automation resolves the branch once and reads its metadata and
archive by that immutable commit. It verifies that the declared baseline is an
official upstream commit and an ancestor of the promoted fork commit.

Consumers select the variant explicitly while retaining the same service,
application data, port, and user-facing endpoint. Switching variants must not
create a second application identity or a separate conversation store. Small
patches must remain backward-compatible with the upstream data format so a
consumer can switch back to `t3code` without migrating or discarding user data.

The package must:

- pin each selected revision, source hash, Cargo dependency hash, and pnpm
  dependency hash;
- build the web client, server, and desktop application explicitly;
- disable pnpm's `verifyDepsBeforeRun` nested-install behavior in the build;
- preserve Linux installation logic from the nixpkgs source package;
- be built and smoke-tested on Linux before deployment;
- verify the runtime-reported T3 and provider CLI versions, not only Nix
  derivation names.

An unmerged schema prerequisite must not consume a numbered upstream migration
identifier. Bootstrap its schema idempotently outside the numbered ledger, then
explicitly reconcile that bootstrap when the upstream migration lands.

The overnight updater must build the fork source with its separate quota patch
immediately after pinning a candidate source and before computing generated
dependency hashes. It records that narrow quota-patch preflight separately from
the later full build and provider validation, so a passing patch check is not
presented as a validated package update.

Codex CLI follows npm's stable `latest` dist-tag. Prerelease channels create
substantial update churn and may move between release lines, so the updater
must reject a prerelease resolved from the default channel. `CODEX_NPM_TAG` may
explicitly select `alpha` or another dist-tag for a time-bounded compatibility
exception after the stable CLI has failed the relevant T3 Code runtime check.

Treat Electron Safe Storage files as application-identity-bound. Before
switching between official and source-built desktop identities, preserve those
files. If decryption fails, retain backups and regenerate only the encrypted
connection metadata; do not replace the separate project database.

For remote launch into an existing Hyprland session, dispatch the executable
through that compositor. Background user services remain appropriate for
headless servers, not interactive desktop windows.

On Linux, launch Electron through XWayland until its native Wayland startup is
reliable. A process existing without a mapped window or a listening backend is
not a successful GUI launch and must fail runtime verification.

## Alternatives Considered

- **Use official artifacts for the upstream channel on Linux** — Rejected
  because source builds keep the two selectable variants structurally identical.
  macOS uses official artifacts to retain upstream application identity.
- **Run upstream and fork as separate services** — Rejected for small,
  data-compatible patches because it fragments conversation history and changes
  the user-facing endpoint.
- **Allow pnpm or Vite to install dependencies during the build** — Rejected
  because it bypasses Nix hashes and fails in sandboxed Darwin builds.
- **Always track prerelease Codex CLI** — Rejected now that stable supports the
  required T3 models. Prerelease channels remain available as an explicit,
  time-bounded compatibility exception.
- **Run the graphical desktop as a user service** — Rejected as the default
  interactive launch path because it can own the single-instance process
  without producing a usable compositor window.

## Consequences

- Linux exposes upstream and fork variants built by the same recipe from
  independently pinned sources. macOS uses upstream nightly releases and does
  not receive fork-only patches through this package set.
- The fork's declared upstream baseline remains distinguishable from its
  feature commits and from the separately selected published nightly.
- Selecting a variant changes only the package used by the existing service;
  application state and network identity are retained.
- Builds are slower than repackaging release binaries, especially on Darwin.
- The Codex package follows stable releases by default and still needs explicit
  build and runtime smoke tests.
- T3's runtime wrapper must explicitly receive the selected Codex and Claude
  derivations. Installing newer CLIs in the user profile is insufficient
  because the desktop launcher prepends its build-time runtime package set.
- Switching desktop distribution identities can require one-time regeneration
  of encrypted connection metadata, while project data remains independent.
- The overnight updater selects the latest published nightly and the latest
  tested fork promotion independently. A change to either source triggers both
  builds and embedded-provider validation before opening an update PR. A quota
  patch conflict or failed build leaves the last package pin in place. Runtime
  activation retains its idle-turn guard.
- Fork package versions use the tested source version and a monotonically
  increasing fork revision. This makes a new fork commit visible even when its
  source version and the selected upstream nightly have not changed.
- Nightly builds are intentionally newer than stable releases. Pinning one
  validated nightly per daily scan limits upstream churn, while the fork sync
  workflow qualifies changes from upstream main before package automation can
  select them.
- Nightly selection applies to T3 Code only. It does not opt Codex into a
  prerelease channel. Linux retains source builds; macOS app versions follow
  the cask or upstream updater rather than the Nix lock file.
