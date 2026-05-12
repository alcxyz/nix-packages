# ADR-0004: Patched Paneru Package

**Status:** Accepted
**Date:** 2026-05-11
**Applies to:** `pkgs/paneru/`, `flake.nix`

## Context

Paneru is used on macOS as the sliding tiling window manager. The Homebrew
formula provides a working binary, but it cannot carry local layout behavior
changes and leaves the macOS setup split between Nix and Homebrew.

The desired layout behavior matches the Hyprland scrolling workflow: if the
visible column strip is narrower than the viewport, the columns should be
anchored as a centered group. Paneru `0.4.1` does not expose a configuration
option for this. Its `auto_center` option centers the focused column when focus
changes, which is a different interaction and should remain available as an
explicit command through the normal `window_center` binding.

The upstream `v0.4.1` tag also has a stale `Cargo.lock` entry for `libc` and
the package requires Rust `1.89.0`, which is newer than the default Rust in the
repo's stable nixpkgs input.

## Decision

Package Paneru in this flake and expose it as
`packages.${system}.paneru` on Darwin systems.

The package:

- builds upstream `karinushka/paneru` at `v0.4.1`
- applies a small source patch that centers narrow layout strips as a group
- applies a `cargoPatches` lock-file fix before vendoring dependencies
- uses the `nixpkgs-unstable` package set only for this package, so Paneru can
  build with a Rust toolchain new enough for upstream `0.4.1`

## Alternatives Considered

- **Keep using Homebrew** — Rejected. It cannot include the local strip-centering
  patch and keeps Paneru outside the declarative package set.
- **Set Paneru `auto_center = true`** — Rejected. It centers focus movement, not
  the narrow strip as a group, and changes the expected `Alt-C` workflow.
- **Move the whole flake to nixos-unstable** — Rejected. Only Paneru needs the
  newer Rust toolchain, so importing unstable narrowly keeps the rest of the
  package set stable.
- **Fork Paneru as a separate repository** — Deferred. A package-local patch is
  smaller and easier to retire if upstream adds equivalent behavior.

## Consequences

- macOS hosts can consume the same Paneru package through Nix on Apple Silicon.
- The local layout behavior is explicit and reviewable in `pkgs/paneru/patches`.
- Linux CI can evaluate Darwin package metadata, but real Darwin builds remain
  the confidence check for this package.
- The local patch should be revisited when Paneru releases a native option for
  centered narrow strips.
