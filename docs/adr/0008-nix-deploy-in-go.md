# ADR-0008: nix-deploy in Go, with build, plan and activate phases

**Status:** Accepted
**Date:** 2026-10-12
**Applies to:** `tools/nix-deploy/`

## Context

`deploy --all` ran one `nixos-rebuild switch --flake .#host --target-host ...`
per host and then a Home Manager switch per host. Every selected host was
evaluated, contacted and re-activated even when its generation was identical
to the one it already ran, and quorum members waited on the serial ready gate
one by one regardless. The run kept no timings, so a slow fleet run could not
say which host or step took the time.

The Bash implementation had grown to about 1,160 lines of per-host phase
state, parallel batches through GNU parallel, a serial group with deadlines
and preflight garbage collection. Splitting a run into build, plan and
activate steps, timing them and running them concurrently adds more of that
state, which Bash expresses poorly and tests only from the outside.

## Decision

nix-deploy is a Go program (`tools/nix-deploy`, binary `deploy`). It keeps
ADR-0006's interface: the versioned JSON inventory and its validation, the
command line, environment tunables, messages and exit statuses. The
black-box contract (`test-deploy.py`) runs against the unwrapped binary in
the package build, next to Go unit tests. The code follows the repository's
Go conventions: concrete types, explicit control flow, standard library
only.

Fleet runs will move to three phases. The Go rewrite (#521) keeps today's
per-host `nixos-rebuild --flake` runs; #522 implements the phases and #523
the timings, and this record is updated as they land:

1. **Build** every selected system and Home Manager configuration on the
   builder in one step, so Nix schedules and deduplicates the work across
   hosts, and attribute a failed evaluation or build to its host.
2. **Plan** by comparing each built path with what the host currently runs.
   An identical host is reported as unchanged and is neither copied to,
   activated nor gated.
3. **Activate** only changed hosts: systems through
   `nixos-rebuild --store-path` (keeping the switch-inhibitor fallback and
   activation modes), Home Manager through its activation script, and serial
   groups one member at a time behind their ready gates.

The fleet summary will report per-host phase timings (#523).

## Alternatives Considered

- **Keep Bash and add the phases there** — rejected; the planning state,
  concurrency and timings would sit in associative arrays and background
  jobs that the contract can only observe through mocked commands.
- **deploy-rs or colmena** — rejected for now; both replace the inventory,
  serial ready gates, preflight capacity checks and Home Manager handling
  that consumers rely on, so adopting one is a migration rather than an
  improvement of this tool.
- **Skip unchanged hosts by comparing evaluation results only** — rejected;
  the running system can differ from the profile (for example after a
  `boot` fallback), so the plan compares against the host itself.

## Consequences

- The runtime dependencies shrink to Bash (for the consumer's ready
  command), core utilities, hostname and OpenSSH; jq, grep, sed and GNU
  parallel are no longer needed. Deployment front ends (`nix`,
  `nixos-rebuild`, `home-manager`, `sudo`, nix-darwin's rebuild command)
  remain runtime requirements.
- Parallel jobs run in-process. Their output lines carry the phase and host
  as before, and the fleet's Home Manager phase no longer calls `deploy`
  recursively.
- The switch-inhibitor fallback to `boot` now applies to fleet runs too, not
  only to single-host deploys.
- Ready-gate pauses are real waits in the contract test, which therefore
  takes about half a minute.
