# ADR-0005: T3 Code Source Builds

**Status:** Accepted
**Date:** 2026-07-11
**Amended:** 2026-08-27
**Amended:** 2026-09-09
**Amended:** 2026-09-12
**Amended:** 2026-09-16
**Amended:** 2026-09-21
**Amended:** 2026-09-30
**Amended:** 2026-10-01
**Amended:** 2026-10-08
**Applies to:** `pkgs/t3code/`, `pkgs/codex-cli/`, package update automation

## Context

T3 Code was originally built from source so that changes maintained in a fork
could be packaged alongside upstream. Packaging an upstream AppImage on Linux
and DMG on Darwin can produce matching display versions while silently
omitting such changes. The desktop and its bundled server also require exact
protocol compatibility, so mixing a patched server with an upstream desktop is
unsafe.

The fork is no longer used. Its sync workflow is disabled and its promotion
branches are frozen. Because the updater pinned upstream and fork channels
together, the stalled fork and a quota patch that no longer applied blocked
upstream updates.

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

Expose only upstream `t3code` on `x86_64-linux`, built from source and
tracking a pinned published upstream nightly. `source.json` records its exact
release tag commit, source hash, and dependency hashes. The fork packaging
(`t3code-fork*` packages, `ai-stack-fork-*` bundles, the quota recovery patch
and fork channel automation) is retired because the fork is no longer used.
Restoring it means reverting that retirement change and re-enabling the
fork's sync workflow.

Linux keeps source builds: the recipe wires the packaged Claude Code and Codex
CLIs into T3's runtime and has been validated against the constraints above.
Replacing it with official Linux artifacts is a separate decision.

On macOS, use the upstream developer-signed nightly app through the
`t3-code@nightly` Homebrew cask, declared by nix-darwin, with existing profile
wiring retained in Home Manager. Do not export Darwin T3 packages: Mac rebuilds
no longer consume them, and CI should not evaluate unused Darwin variants.
The native desktop uses its matching upstream server. This platform exception
follows nix-config ADR-0073.

The package must:

- pin the selected revision, source hash, Cargo dependency hash, and pnpm
  dependency hash;
- build the web client, server, and desktop application explicitly;
- disable pnpm's `verifyDepsBeforeRun` nested-install behavior in the build;
- preserve Linux installation logic from the nixpkgs source package;
- be built and smoke-tested on Linux before deployment;
- verify the runtime-reported T3 and provider CLI versions, not only Nix
  derivation names.

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

- **Track raw upstream `main`** — Rejected because those commits are not
  published releases and can change product behavior between scans.
- **Keep the fork channels pinned but frozen** — Rejected because the fork is
  unused, its patch no longer applies to newer sources, and coupled channel
  updates blocked upstream releases.
- **Allow pnpm or Vite to install dependencies during the build** — Rejected
  because it bypasses Nix hashes and fails in sandboxed Darwin builds.
- **Always track prerelease Codex CLI** — Rejected now that stable supports the
  required T3 models. Prerelease channels remain available as an explicit,
  time-bounded compatibility exception.
- **Run the graphical desktop as a user service** — Rejected as the default
  interactive launch path because it can own the single-instance process
  without producing a usable compositor window.

## Consequences

- Linux exposes one upstream T3 package and the `ai-stack-upstream` bundle.
  macOS uses upstream nightly releases. Consumers of the retired fork outputs
  must switch to `t3code` or `ai-stack-upstream`.
- Builds are slower than repackaging release binaries, especially on Darwin.
- The Codex package follows stable releases by default and still needs explicit
  build and runtime smoke tests.
- T3's runtime wrapper must explicitly receive the selected Codex and Claude
  derivations. Installing newer CLIs in the user profile is insufficient
  because the desktop launcher prepends its build-time runtime package set.
- Switching desktop distribution identities can require one-time regeneration
  of encrypted connection metadata, while project data remains independent.
- The six-hourly updater selects the latest published upstream nightly. A
  lightweight check compares its identity with `dev` and any open update PR
  before Nix setup. A changed candidate triggers source, dependency,
  full-build, and embedded-provider validation before opening an update PR. A
  failed build leaves the last package pin in place.
- Nightly selection applies to T3 Code only. It does not opt Codex into a
  prerelease channel. Linux retains source builds; macOS app versions follow
  the cask or upstream updater rather than the Nix lock file.
