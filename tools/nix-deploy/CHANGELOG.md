# nix-deploy changelog

`deploy --version` and the `version` field of the `--json` report name the
running release. Bump `version` in `default.nix` with every change to the tool
and add an entry here:

- major: an incompatible change to the command line, the inventory schema or
  the JSON report;
- minor: a new option or a change in what a run does;
- patch: a fix that keeps the intended behaviour.

## 0.9.0 (2026-10-16)

- `k8s-node-power reboot|off|on` (#548, phase 1): a Go port of the
  k8s-node-reboot Bash tool (`kreboot`, `koff`, `kon`) with the same options,
  gates and messages (nix-config ADR-0068 and ADR-0082). It ships beside
  `deploy` for qualification runs; the Bash tool stays the supported command
  until then. kubectl JSON is decoded in Go instead of jq, and output that
  does not decode fails the gate rather than taking a fallback. Durations are
  validated before any cluster change and bare seconds reach kubectl as `Ns`.
  An interrupt stops new commands, restores a raised Longhorn rebuild limit
  and reports a stopped self-fence agent.

## 0.8.0 (2026-10-15)

- Build profiles (#548): before the shared build, count the derivations it
  would build (`nix build --dry-run`). From `DEPLOY_HEAVY_BUILDS` (default 50)
  the run is heavy and builds with `max-jobs` `DEPLOY_HEAVY_MAX_JOBS` (default
  6) and `cores` `DEPLOY_HEAVY_CORES` (default 8), each capped at this
  machine's own setting (one that cannot be read is left alone); below it, light keeps this machine's Nix settings. A
  count that fails or cannot be read picks heavy.
- `--heavy` / `--light`, or `DEPLOY_BUILD_PROFILE=auto|light|heavy`, choose
  the profile without counting.
- `--dry-run` prints the profile a run would use, and the JSON report gains
  `buildProfile` and `pendingBuilds`.

## 0.7.1 (2026-10-14)

- Drop Nix's "waiting for another Nix process to finish fetching input" lines
  from the evaluate phase. Nix takes a lock to fetch the flake for every
  evaluation, even one already in the store, so parallel evaluations always
  queue briefly and report it; the wait is expected and short.

## 0.7.0 (2026-10-13)

- Plan before building: each target's store path is compared with what its
  host runs straight after evaluation, and only changed targets are built. An
  unchanged host is no longer rebuilt on this machine, which recompiled its
  packages (under emulation for other architectures) whenever they had left
  the local store (ADR-0008).
- `--dry-run` stops after the plan and skips the build phase.

## 0.6.0 (2026-10-13)

- `deploy --version` prints the release, and the JSON report gains a
  `version` field.

## 0.5.0 (2026-10-12)

- Build every selected target once, compare it with what each host runs, and
  activate only changed targets (#526).
- `--dry-run`, `--force` and `--json FILE`; per-host timings in the summary.
- `serialGroups`: independent groups of hosts that switch one at a time behind
  a ready command.
- Failed or interrupted activations are remembered and retried.

## 0.4.0 (2026-10-12)

- Rewritten in Go at parity with the Bash tool (ADR-0008, #524).

## 0.3.0 (2026-10-01)

- Switch quorum hosts serially behind a ready gate (#456).

## 0.2.0 (2026-09-09)

- Configured at runtime by a versioned JSON inventory (ADR-0006).

## 0.1.0 (2026-04-26)

- First release: one command for NixOS, nix-darwin and Home Manager hosts.
