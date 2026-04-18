# nix-packages

Personal Nix flake with packages and tools not in nixpkgs.

## Usage

```nix
# flake.nix
inputs.nix-packages.url = "github:alcxyz/nix-packages";
```

Then reference packages as `inputs.nix-packages.packages.${system}.<name>`.

## Packages

| Package | Description | Platforms |
|---------|-------------|-----------|
| [helium](https://github.com/imputnet/helium) | Helium browser | `x86_64-linux` `aarch64-linux` `aarch64-darwin` `x86_64-darwin` |
| [t3code](https://github.com/pingdotgg/t3code) | T3 Code — AI coding assistant desktop app | `x86_64-linux` `aarch64-darwin` |
| [claude-code](https://github.com/anthropics/claude-code) | Agentic coding tool that lives in your terminal | all |
| [carapace](https://github.com/carapace-sh/carapace-bin) | Cross-shell completion engine | `x86_64-linux` `aarch64-linux` `aarch64-darwin` `x86_64-darwin` |
| [carapace-bridge](https://github.com/carapace-sh/carapace-bin) | Shell integration bridges for carapace | `x86_64-linux` `aarch64-linux` `aarch64-darwin` `x86_64-darwin` |
| [ndrop](https://github.com/schweber/ndrop) | Scratchpad toggle helper for Wayland compositors | `x86_64-linux` |

## Tools

Internal tools built from source, tracked in this repo.

| Tool | Description | Platforms |
|------|-------------|-----------|
| pihole-sync | Sync Pi-hole config between instances | all |
| zfs-auto-unlock | Automatic ZFS dataset unlocking | all |
| leantime-tidy | Leantime ticket cleanup with AI suggestions | `x86_64-linux` |
| paperless-review | AI review tool for Paperless-NGX inbox | `x86_64-linux` |
| paperless-filetype-index | Symlink index of non-PDF Paperless files by type | `x86_64-linux` |

## Automated updates

`helium` and `t3code` are kept up to date by a daily GitHub Actions workflow
(`.github/workflows/update-packages.yml`). When a new upstream release is
detected the workflow computes fresh Nix SRI hashes and opens a pull request.
