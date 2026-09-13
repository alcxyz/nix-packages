# Package build measurements — 14 September 2026

Measurements use the normal Linux CI runners and package inputs from the
Claude Code update head `7bcbf834747e3181881808960c5754d9ddecf23d`, including
OmniWM 0.6.10 and Zen Browser 1.22.1b already integrated into `dev`.
The separate T3 title-feature PR #358 is excluded.

## Method

- [Run 1722](https://git.alc.xyz/alcxyz/nix-packages/actions/runs/1722)
  validates Claude Code 2.1.270 and its two T3 consumers through ordinary CI.
- [Run 1723](https://git.alc.xyz/alcxyz/nix-packages/actions/runs/1723)
  enumerates every other `x86_64-linux` export and times each `nix build`.
  Failures are recorded individually so one failure cannot hide later exports.
- Together these cover all 25 Linux exports, including aliases. Go tests,
  automation tests, package hashes, all advertised platform evaluations and
  contract tests remain mandatory in ordinary CI.
- Each job starts with an ephemeral Nix store. Standard upstream substitutes
  remain enabled; later packages can reuse dependencies from earlier packages
  in the same job. These are realization times (downloads plus any local build),
  not isolated compilation benchmarks. The T3 dependency cache missed in run
  1722, so that run includes cold dependency preparation.
- Non-Linux exports are evaluated, not natively built. In particular OmniWM's
  update validation does not prove a native Darwin build or rendered behavior.
- Times are single observations, not latency guarantees. Job setup, runner
  assignment, upstream download speed and host contention affect elapsed CI.
  The earlier Zen update run experienced observed host-pressure pauses; those
  delays must not be confused with intrinsic package compilation cost.

## Results

All 25 Linux exports realized successfully. All required update PR checks passed.
OmniWM #360, Zen Browser #362 and Claude Code #359 were squash-merged into `dev`.

### End-to-end runs

Time from the first job log to final completion, including setup and reporting
but excluding any queue time before the first job starts.

| Run | Scope | Elapsed |
| --- | --- | ---: |
| [1720](https://git.alc.xyz/alcxyz/nix-packages/actions/runs/1720) | OmniWM update; no Linux export | 2m10s |
| [1721](https://git.alc.xyz/alcxyz/nix-packages/actions/runs/1721) | Zen Browser update; observed host-pressure pauses | 14m31s |
| [1722](https://git.alc.xyz/alcxyz/nix-packages/actions/runs/1722) | Claude Code update plus both T3 consumers; dependency cache miss | 25m26s |
| [1723](https://git.alc.xyz/alcxyz/nix-packages/actions/runs/1723) | Remaining 22 Linux exports, sequential in one job | 24m30s |

### Claude Code and T3 selected validation

These groups include platform evaluation and Linux realization. T3 includes
pnpm dependencies, the resource helper and the application. Fork timing is
**after upstream T3 in the same store**, not a standalone cold fork benchmark.

| Export | Selected validation |
| --- | ---: |
| `claude-code` | 1m08s |
| `t3code` | 14m06s |
| `t3code-fork` | 6m05s |

The entire selected-build step took 21m34s; provider checks added 28s.
The full dependency export reached 3,132,417,843 bytes and was skipped by the
2 GiB upload limit. This exposed the need to budget the selected outputs
before export, rather than merely reject the finished archive.

### Other Linux exports

Time inside each `nix build`, excluding setup and explicit home cleanup.
All rows passed. `default` aliases Helium; the later `helium` row reuses that
same output, so its 3s is not an independent cold Helium build.

| Export | Realization time |
| --- | ---: |
| `agent-sync-check` | 7s |
| `codex-app-server` | 10s |
| `codex-cli` | 26s |
| `default` | 21s |
| `devlog` | 37s |
| `forge-mirror` | 31s |
| `forgejo-runner-orphan-check` | 32s |
| `helium` | 3s |
| `herdr` | 8m18s |
| `k8s-node-reboot` | 3s |
| `kdash` | 2s |
| `ledger-live` | 16s |
| `ndrop` | 2s |
| `nix-deploy` | 2s |
| `nix-gc-maintenance` | 2s |
| `openzfs_7_1` | 2m35s |
| `stash` | 9m09s |
| `wcap` | 10s |
| `xonsh-direnv` | 3s |
| `xonsh-with-direnv` | 3s |
| `zen-browser` | 20s |
| `zfs-auto-unlock` | 7s |

## Interpretation

Most tools are inexpensive once shared dependencies are available. T3, Herdr
and Stash account for much of the observed work. A change to a provider CLI
still validates both T3 consumers and is materially more expensive than an
isolated tool update. The measurements do not establish standalone cold times
for every export or warm-cache end-to-end T3 latency.

Host-pressure pauses also affect ordinary checks and status reporting. This
pass changed no host admission, capacity or pressure policy. Removing unrelated
builds improves routine CI, but these runs do not establish a consistent
sub-five-minute service level for package updates.


## Small correction from these measurements

The cache helper now chooses a subset before exporting: 99% of the configured
size limit is a raw NAR budget, with large eligible outputs considered first
and smaller outputs filling available space. The actual archive-size check
remains. In the measured dependency mix, this keeps the 1.46 GB pnpm store
and skips the two roughly 769 MB npm archives that would overflow the budget.
This is deliberately conservative about compression and does not change
package coverage or required validation.

Regression tests cover selection, oversized outputs, actual archive limits
and failed-export preservation. A local export using real public dependency
metadata and NARs verifies that the pnpm output fits and is the only selected
output from that three-output fixture. The existing live restore qualification
remains recorded in ADR-0003.
