# ADR-0008: nix-deploy in Go, with evaluate, plan, build and activate phases

**Status:** Accepted; phase order amended 2026-10-13
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

Every run, single-host or fleet, has four phases (#522):

1. **Evaluate.** Each selected system and Home Manager configuration is
   evaluated on its own, up to `DEPLOY_EVAL_JOBS` (default 4) at a time, so an
   evaluation error is attributed to its host. Evaluation yields the
   derivation and the store path it builds, without building the target
   itself (an input read during evaluation, import from derivation, is still
   realised).
2. **Plan.** Each evaluated path is compared with what the host runs, read over
   SSH (or locally for this machine): a system is unchanged when both
   `/run/current-system` and the system profile point to it (only the profile
   for hosts staged with `boot`), Home Manager when both its profile and its
   `current-home` root do (the root moves only after every activation step
   succeeded). An unknown generation counts as changed, and so does a
   generation whose activation from this machine did not finish: a failed or
   interrupted switch can still leave both links on the new system, so the
   operator keeps a record (`nix-deploy/failed-activations.json` under the
   XDG state directory). Each activation is entered before it starts and does
   not start if the entry cannot be saved; every unfinished generation of a
   target is kept, and the target's entries are cleared once one of its
   activations succeeds. Updates re-read the record under a file lock, so
   concurrent runs on the operator keep each other's entries, and each target
   is locked across runs while it activates, so one run's success cannot
   clear another's activation in flight. A record that cannot be read counts
   as unfinished, which then fails the target instead of skipping it. A
   serial member stays unfinished until its ready gate passes, so one that
   switched but did not rejoin is switched and gated again.
3. **Build.** Only changed targets are built, in one `nix build --keep-going`
   on this machine, so Nix schedules and deduplicates the work across hosts,
   and `nix path-info` tells which targets built. Built paths stay rooted for
   the rest of the run, since activation can come minutes later behind ready
   gates: the shared build links every output when all succeed, and each
   built target is rooted again through its derivation, which rebuilds an
   output a collection removed in between.
4. **Activate.** Only changed targets are activated: remote systems through
   `nixos-rebuild <switch|boot> --store-path P --no-reexec --target-host ...
   --use-substitutes`, which copies the closure and keeps the
   switch-inhibitor fallback; `--no-reexec` stops it from rebuilding itself
   from a configuration it would have to evaluate. Before the first switch,
   the run checks once that the operator's `nixos-rebuild` offers both
   options (nixos-rebuild-ng from NixOS 25.11 does; the classic script and
   older ng releases do not) by passing them without an action, which a
   capable frontend rejects only for the missing action, and switches nothing
   otherwise.
   Remote scripts run under `sh -c`, whatever the login shell. The local system goes
   through `sudo nixos-rebuild switch --store-path P --no-reexec` (or `darwin-rebuild switch
   --flake` on Darwin, which cannot activate a store path); Home Manager the
   way the `home-manager` command does, setting its profile and then running
   the generation's `activate --driver-version 1`, after copying it to remote
   hosts. Serial groups switch only their changed members, but still check
   the whole group before the first switch.

`--dry-run` stops after the plan, before building, and never starts a host's garbage
collection (it assumes a collection a real run would start frees enough
space), and `--force` builds and activates unchanged targets too. A single-host
deploy builds all its changed targets before activating any, so a build
failure leaves the host untouched.

Serial groups (#504) are listed in `serialGroups`, each with its own hosts,
ready command and timeout; the older `serialSystemHosts` fields are a
one-group shorthand. Groups are independent: a member that fails preflight
or a pre-check blocks only its own group, groups run one after another, and
a single-host deploy checks only its own group.

The fleet summary adds the time each host spent in evaluation, activation
and ready gates, and the shared build time (#523). `--json FILE` writes the
same results and timings for comparison across runs.

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
- A run that changes nothing evaluates every target, reads one generation per
  target over SSH, and builds and activates nothing; quorum members are not
  gated.
- Both preflight phases run before the build, so `--fail-unreachable` stops
  the run before any host switches, instead of after the system phase.
- Hosts are compared by store path, so a configuration that embeds a
  changing input (for example a whole private flake source) still changes
  every time; consumers keep such inputs out of their closures.

## Amendment (2026-10-13)

The first implementation built every target before the plan, on the
assumption that unchanged targets would come from the local store or a
cache. Neither held: the operator's garbage collection removes outputs that
only remote hosts use, and nothing in a binary cache matches packages built
for another architecture, so a fleet run recompiled up-to-date Raspberry Pi
hosts under emulation on the operator. Evaluation already gives each
target's store path, which is all the plan compares, so the plan now runs
between evaluation and the build, and only changed targets are built
(release 0.7.0). Comparing against the host itself, not a previous
evaluation, is unchanged. A dry run now skips the build phase, so it no
longer shows that changed targets build.
