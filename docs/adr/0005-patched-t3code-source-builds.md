# ADR-0005: Patched T3 Code Source Builds

**Status:** Accepted
**Date:** 2026-07-11
**Amended:** 2026-08-27
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

Package T3 Code from the pinned fork source on every supported platform. Linux
and Darwin must use the same fork revision and package version. Do not restore
official release artifacts for one platform merely to simplify its build.

The package must:

- pin the fork revision, source hash, and pnpm dependency hash;
- build the web client, server, and desktop application explicitly;
- disable pnpm's `verifyDepsBeforeRun` nested-install behavior in the build;
- preserve platform-specific installation logic from the nixpkgs source
  package, including the Darwin `.app` bundle;
- be built and smoke-tested on both Linux and Darwin before deployment;
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

- **Use official artifacts on all platforms** — Rejected because they omit the
  fork changes.
- **Use the patched source build only on Linux** — Rejected because desktop and
  server versions can diverge and become protocol-incompatible.
- **Allow pnpm or Vite to install dependencies during the build** — Rejected
  because it bypasses Nix hashes and fails in sandboxed Darwin builds.
- **Always track prerelease Codex CLI** — Rejected now that stable supports the
  required T3 models. Prerelease channels remain available as an explicit,
  time-bounded compatibility exception.
- **Run the graphical desktop as a user service** — Rejected as the default
  interactive launch path because it can own the single-instance process
  without producing a usable compositor window.

## Consequences

- Both supported platforms run the same patched T3 implementation.
- Builds are slower than repackaging release binaries, especially on Darwin.
- The Codex package follows stable releases by default and still needs explicit
  build and runtime smoke tests.
- T3's runtime wrapper must explicitly receive the selected Codex and Claude
  derivations. Installing newer CLIs in the user profile is insufficient
  because the desktop launcher prepends its build-time runtime package set.
- Switching desktop distribution identities can require one-time regeneration
  of encrypted connection metadata, while project data remains independent.
- Update automation must not replace the source build with official artifacts
  or opt into a Codex prerelease channel without a documented compatibility
  reason.
