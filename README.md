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
| [herdr](https://github.com/ogulcancelik/herdr) | Agent multiplexer that lives in your terminal | `x86_64-linux` `aarch64-linux` `aarch64-darwin` |
| [helium](https://github.com/imputnet/helium) | Helium browser | `x86_64-linux` `aarch64-darwin` `x86_64-darwin` |
| [kdash](https://github.com/kdash-rs/kdash) | Simple and fast dashboard for Kubernetes | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [t3code](https://github.com/pingdotgg/t3code) | T3 Code upstream published nightly | `x86_64-linux` |
| `t3code-fork-nightly` (`t3code-fork`) | Promoted nightly fork with quota recovery patch | `x86_64-linux` |
| `t3code-fork-stable` | Promoted stable fork with quota recovery patch | `x86_64-linux` |
| `ai-stack-{upstream,fork-nightly,fork-stable}` | Profile bundle of one T3 channel with `claude-code`, `codex-cli` and `codex-app-server` | `x86_64-linux` |
| [claude-code](https://github.com/anthropics/claude-code) | Agentic coding tool that lives in your terminal | all |
| [codex-cli](https://github.com/openai/codex) | Lightweight coding agent that runs in your terminal | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [codex-app-server](https://github.com/openai/codex) | Codex app server for GUI integrations | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [ndrop](https://github.com/schweber/ndrop) | Scratchpad toggle helper for Wayland compositors | `x86_64-linux` |
| [OpenZFS 7.1 snapshot](https://github.com/openzfs/zfs) | Pinned unreleased OpenZFS snapshot with Linux 7.1 support | `x86_64-linux` `aarch64-linux` |

The default package is Helium on its supported systems, exposed through
`packages.<system>.default`. ARM Linux has no default package until a verified
Helium artifact is available; choose a named package there. The complete export
matrix is documented in [ADR-0003](docs/adr/0003-fail-loud-automated-package-updates.md).

## Tools

Internal tools built from source, tracked in this repo.

| Tool | Description | Platforms |
|------|-------------|-----------|
| k8s-node-reboot | Guarded Kubernetes node reboot, power, and network-path lifecycle helpers | all |
| nix-gc-maintenance | Guarded Nix profile retention and capped garbage collection | all |
| zfs-auto-unlock | Automatic ZFS dataset unlocking | all |

## Automated updates

Stable Claude Code, Codex CLI, and Codex app-server releases are checked every
hour by `.forgejo/workflows/update-ai-tools.yml`. A lightweight probe
compares upstream releases with `dev` and any pending `update/ai-tools` PR before
installing Nix. Unchanged candidates skip builds; new provider releases update
one combined PR so T3 validates their versions together. Failed unchanged
candidates remain visible for diagnosis or explicit CI retry.

Other packages, including T3 nightlies and tested fork promotions, retain the
daily `.forgejo/workflows/update-packages.yml` scan and one stable
`update/<package>` branch. New releases refresh the existing PR. The daily
matrix is capped at two concurrent package jobs; provider scans are serialized.

The merge queue checks hourly at half past, following provider scans at the
top of each hour. Green `update/*` pull requests are
rebased onto `dev` when needed and squash-merged by
`.forgejo/workflows/auto-merge-updates.yml`; ordinary PR events trigger validation.
Consumer promotion and idle-session activation gates remain in place, so scan
cadence is not a deployment-time guarantee. See
[ADR-0007](docs/adr/0007-continuous-provider-updates.md).
Successful update PRs are validated before merge, not again on the resulting
`dev` push; `main` is still validated when changes are promoted manually.
Promotion from `dev` to `main` is manual. `dev` is a long-lived integration
branch and must be retained after promotion; repository-level default branch
deletion is disabled, while update branch cleanup is handled explicitly by the
auto-merge workflow.

Updater scripts must fail before committing invalid generated state. Empty SRI
hashes such as `hash = "sha256-";` are rejected by both the updater scripts and
CI; see [ADR-0003](docs/adr/0003-fail-loud-automated-package-updates.md).

### Runtime-configured deployment

`nix run .#nix-deploy -- --config ./inventory.json --help` selects deployment
inventory explicitly. `NIX_DEPLOY_CONFIG` supplies the same default for direct
CLI use; a consumer may generate the JSON and provide a configured wrapper.
The package supplies no built-in fleet. See [ADR-0006](docs/adr/0006-runtime-configured-nix-deploy.md)
for the versioned JSON fields and runtime command requirements.
