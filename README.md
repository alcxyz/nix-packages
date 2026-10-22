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
| [kdash](https://github.com/kdash-rs/kdash) | Simple and fast dashboard for Kubernetes | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [t3code](https://github.com/pingdotgg/t3code) | T3 Code upstream published nightly | `x86_64-linux` |
| `ai-stack-upstream` | Profile bundle of `t3code` with `claude-code`, `codex-cli` and `codex-app-server` | `x86_64-linux` |
| [claude-code](https://github.com/anthropics/claude-code) | Agentic coding tool that lives in your terminal | all |
| [codex-cli](https://github.com/openai/codex) | Lightweight coding agent that runs in your terminal | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [codex-app-server](https://github.com/openai/codex) | Codex app server for GUI integrations | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [workmux](https://github.com/raine/workmux) | Git worktrees and tmux windows for parallel development | `x86_64-linux` `aarch64-linux` `x86_64-darwin` `aarch64-darwin` |
| [ndrop](https://github.com/schweber/ndrop) | Scratchpad toggle helper for Wayland compositors | `x86_64-linux` |
| [OpenZFS 7.1 snapshot](https://github.com/openzfs/zfs) | Pinned unreleased OpenZFS snapshot with Linux 7.1 support | `x86_64-linux` `aarch64-linux` |

There is no default package; choose a named package. The complete export
matrix is documented in [ADR-0003](docs/adr/0003-fail-loud-automated-package-updates.md).

## Tools

Internal tools built from source, tracked in this repo.

| Tool | Description | Platforms |
|------|-------------|-----------|
| [deck](tools/deck/README.md) | T3 thread dashboard and operator-authored interactive page collections | all |
| gh-app | Routes gh and Git through a GitHub App's installation token for the App's installed repositories, and registers new Apps from a manifest (`gh-app register`) | all |
| gh-app-gh | A `gh` that runs through gh-app, and is plain gh until the App is configured; used by t3code | all |
| k8s-node-reboot | Guarded Kubernetes node reboot, power, and network-path lifecycle helpers | all |
| nix-gc-maintenance | Guarded Nix profile retention and garbage collection | all |
| revkit | Agent reviews of whole repositories with sandboxed reviewers of both AI clients | all |
| zfs-auto-unlock | Automatic ZFS dataset unlocking | all |

## Automated updates

Stable Claude Code, Codex CLI, and Codex app-server releases are checked every
six hours by `.forgejo/workflows/update-ai-tools.yml`. A lightweight probe
compares upstream releases with `dev` and any pending `update/ai-tools` PR before
installing Nix. Unchanged candidates skip builds; new provider releases update
one combined PR so T3 validates their versions together. Failed unchanged
candidates remain visible for diagnosis or explicit CI retry.

Published upstream T3 nightlies are scanned every six hours by `.forgejo/workflows/update-t3code.yml`. Other packages retain the
daily `.forgejo/workflows/update-packages.yml` scan. Each uses one stable
`update/<package>` branch. New releases refresh the existing PR. The daily
matrix is capped at two concurrent package jobs; provider scans are serialized.

The merge queue checks hourly at half past the hour. Green `update/*` pull requests are
rebased onto `dev` when needed and squash-merged by
`.forgejo/workflows/auto-merge-updates.yml`; ordinary PR events trigger validation.
Consumer promotion and idle-session activation gates remain in place, so scan
cadence is not a deployment-time guarantee. See
[ADR-0007](docs/adr/0007-continuous-provider-updates.md).

The `promoted` branch marks the newest `dev` revision that passed the consumer's
full configuration validation. The nix-config local package promoter moves it
(nix-config ADR-0080); unattended AI-stack updates follow it, and nix-config's
`just lock-packages` locks it. Do not commit to it or base work on it.

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
`nixfleet --version` names the release; its
[changelog](tools/nix-deploy/CHANGELOG.md) lists what each one changed.

The package (`.#nixfleet`, also `.#nix-deploy`) installs one program,
`nixfleet`, with these subcommands:

- `nixfleet deploy`: the deploy command. It is also installed as `deploy`,
  which takes the same options without the subcommand, and `nix run
  .#nix-deploy` still runs it.
- `nixfleet reboot [--all | <host>...]`: reboots hosts of the fleet, each
  under the policies the inventory gives it ([ADR-0010](docs/adr/0010-fleet-reboot-policies.md)):
  Kubernetes nodes (`kubernetesNodes`, in the cluster of `kubernetesContext`)
  behind the node gates, serial group
  members behind their ready command, runner hosts after their admitted jobs.
  Serial groups and other hosts run as concurrent constraint groups. Nodes
  holding fewer database primaries and core singletons (single-Pod workloads
  of `kubernetesCorePriorityClass`) reboot first, and nodes waiting for their
  turn carry a `PreferNoSchedule` taint so evicted Pods prefer nodes that
  already rebooted. Interactive hosts run a guard first and are asked about
  before the start or skipped when busy; this machine reboots last, only with
  `--include-self` on a terminal; every question comes before anything
  starts. With `bootCheck` each host's own check after the boot must pass. It prints
  the plan first; `--dry-run` stops there, and a run needs a confirmation on a
  terminal or `--yes`. With `--needed`, hosts whose booted kernel, initrd,
  kernel modules and kernel parameters match their current and next-boot
  system are skipped (unless the node is cordoned); when that cannot be told,
  they reboot.
- `nixfleet off|on <host>` and `k8s-node-power reboot|off|on <host>`: the Go
  port of the `k8s-node-reboot` helpers under qualification for one node;
  `kreboot`, `koff` and `kon` remain the supported commands until it replaces
  them. `k8s-node-power reboot --needed <host>` skips a reboot the host does
  not need, as above.
- `nixfleet status [--all | <host>...] [--json FILE]`: reads, without changing
  anything, each host's booted and current system, whether it needs a reboot,
  its Kubernetes cordon state and its Forgejo or Gitea runner units, and prints
  them as a table. Unreachable hosts and an unreachable cluster are reported,
  not errors. `--json` writes a `version`, a `cluster` object
  (`reachable`, `error`, `nodes`) and one entry per host with `host`,
  `reachable`, `error`, `os`, `bootedSystem`, `currentSystem`,
  `nextBootSystem`, `rebootNeeded` (`true`, `false` or `null` when it cannot be
  told), `rebootReason`, `rebootDiffers`, `node` (`name`, `cordoned`, `ready`,
  or `null`) and `runners` (`active`, `total`, `units`, or `null`).

The program chooses its command by the name it is invoked as, so each
installed name is a wrapper that sets it.

### Agent reviews

`revkit repo-review [--config FILE] [PATH]` reviews a whole repository at
rest: each configured lens runs once with every configured reviewer (Claude
Code and Codex), each reviewer's findings are checked by another, and the
result goes to `report.md` and `results.json` in a run directory under
`$XDG_STATE_HOME/revkit`; nothing is filed. Reviewers are read-only and load
none of the user's instructions, skills or settings: Claude in restricted and
safe mode, Codex in a temporary home that holds only its login and under a
file system profile that denies everything outside the tree. A reviewer that
fails, replies out of format or makes no successful tool call, and a
verification that leaves findings without a verdict, are reported as missing
reviews. The package ships no reviewers or
lenses: the consumer generates the JSON configuration (`--config` or
`REVKIT_CONFIG`). Distinct problems are kept in a findings store per
repository, matched by meaning with earlier runs, so a repeat run shows what
is new; `revkit findings` lists, shows, dismisses and reopens them, files
the confirmed ones you pick as issues in the repository's configured tracker
(never security-relevant ones), and `repo-review --resume` reuses the jobs
that completed in the last run. See [ADR-0011](docs/adr/0011-revkit-agent-review-toolkit.md)
and the [changelog](tools/revkit/CHANGELOG.md).
