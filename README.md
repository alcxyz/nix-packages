# nix-packages

Personal Nix flake with packages and tools not in nixpkgs.

## Usage

```nix
# flake.nix
inputs.nix-packages.url = "git+https://git.alc.xyz/alcxyz/nix-packages.git";
```

Then reference packages as `inputs.nix-packages.packages.${system}.<name>`.

## Packages

| Package | Description | Platforms |
|---------|-------------|-----------|
| [ghostty](https://ghostty.org) | Ghostty terminal emulator | `aarch64-darwin` `x86_64-darwin` |
| [helium](https://github.com/imputnet/helium) | Helium browser | `x86_64-linux` `aarch64-linux` `aarch64-darwin` `x86_64-darwin` |
| [kdash](https://github.com/kdash-rs/kdash) | Simple and fast dashboard for Kubernetes | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [paneru](https://github.com/karinushka/paneru) | Sliding, tiling window manager for macOS | `aarch64-darwin` `x86_64-darwin` |
| [t3code](https://github.com/pingdotgg/t3code) | T3 Code — AI coding assistant desktop app | `x86_64-linux` `aarch64-darwin` |
| [claude-code](https://github.com/anthropics/claude-code) | Agentic coding tool that lives in your terminal | all |
| [ndrop](https://github.com/schweber/ndrop) | Scratchpad toggle helper for Wayland compositors | `x86_64-linux` |

## Tools

Internal tools built from source, tracked in this repo.

| Tool | Description | Platforms |
|------|-------------|-----------|
| zfs-auto-unlock | Automatic ZFS dataset unlocking | all |

## Automated updates

Packages are kept up to date by a daily Forgejo Actions workflow
(`.forgejo/workflows/update-packages.yml`). When a new upstream release is
detected the workflow computes fresh Nix SRI hashes and opens a Forgejo pull
request against `dev`.

Each package uses one stable `update/<package>` branch. If an update PR is
already open, the next updater run refreshes that same branch and PR so a newer
upstream release supersedes the stuck update instead of creating more PR noise.

The update matrix is capped at two concurrent package jobs to avoid flooding
the shared runners. Green `update/*` pull requests are merged into `dev` by
`.forgejo/workflows/auto-merge-updates.yml`; PR events trigger the normal path,
with a nightly scheduled run as a fallback. Promotion from `dev` to `main` is
manual.

Updater scripts must fail before committing invalid generated state. Empty SRI
hashes such as `hash = "sha256-";` are rejected by both the updater scripts and
CI; see [ADR-0003](docs/adr/0003-fail-loud-automated-package-updates.md).
