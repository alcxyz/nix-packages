# ADR-0005: Patched T3 Code Source Builds

**Status:** Accepted
**Date:** 2026-07-11
**Amended:** 2026-08-27
**Amended:** 2026-09-09
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

Expose two source-built variants on every supported platform. `t3code` tracks a
pinned stable upstream release, while `t3code-fork` applies narrowly scoped,
checked-in patches to a pinned upstream revision. Both variants use one shared
build recipe so their provider wiring and platform-specific installation stay
identical.

Consumers select the variant explicitly while retaining the same service,
application data, port, and user-facing endpoint. Switching variants must not
create a second application identity or a separate conversation store. Small
patches must remain backward-compatible with the upstream data format so a
consumer can switch back to `t3code` without migrating or discarding user data.

The package must:

- pin the selected revision, source hash, Cargo dependency hash, and pnpm
  dependency hash;
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

- **Use official artifacts for the upstream channel** — Rejected because source
  builds keep the two selectable variants structurally identical.
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

- Both supported platforms expose matching upstream and patched variants.
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
- Update automation updates `t3code` without modifying the pinned revision or
  patch set of `t3code-fork`. It must not replace either source build with
  official artifacts or opt into a Codex prerelease channel without a
  documented compatibility reason.
