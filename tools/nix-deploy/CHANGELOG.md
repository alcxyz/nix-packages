# nix-deploy changelog

`deploy --version` and the `version` field of the `--json` report name the
running release. Bump `version` in `default.nix` with every change to the tool
and add an entry here:

- major: an incompatible change to the command line, the inventory schema or
  the JSON report;
- minor: a new option or a change in what a run does;
- patch: a fix that keeps the intended behaviour.

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
