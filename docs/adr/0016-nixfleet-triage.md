# ADR-0016: nixfleet triage: run records and an agent playbook for failed deploys and warnings

**Status:** Accepted
**Date:** 2026-10-27, updated 2026-10-31 (warning triage, per nix-packages#673; `--build-only` in 0.21.0, the triage context in 0.22.0, its lock diff in 0.23.0, the agent launch, own inputs and playbook in 0.24.0)
**Applies to:** `tools/nix-deploy/`
**History:** [replaced text](archive/0016-nixfleet-triage-history.md)

## Context

A failed `deploy` leaves its evidence only in the terminal. The `--json`
report (ADR-0008) has each target's status and timings, not its errors, so
once the scrollback is gone nobody can say why a host failed, and handing the
failure to someone else means rerunning it.

Most failures follow a `flake update`: a nixpkgs update removes an option a
host still sets, renames a package argument an override passes, or changes a
build. The fix is ordinary code work in the flake or in one of the operator's
own inputs, and the question that finds it fastest is what changed in the lock
since the last deploy that worked.

The operator already uses a pattern for this kind of work in T3 Code: `t3
triage` is a thin command that writes the machine facts and a versioned
playbook, then starts the operator's own `claude` or `codex` CLI interactively,
whose own permission prompts gate every action. The tool gathers evidence; the
agent and the operator decide.

Runs that succeed can still print evaluation warnings: renamed or obsolete
options, renamed package aliases, deprecated attributes such as
`stdenv.isLinux`. Each is a failure a later nixpkgs update will turn into an
error when it removes the old name. Some come from the operator's own code and
can be fixed now; others come from an input the operator does not own, and the
fix is to update that input or to wait for its upstream.

## Decision

Triage comes in phases (nix-packages#673).

### Phase 1: run records (release 0.19.0)

Every `deploy` run that gets past argument parsing, single-host, `--all` and
`--dry-run` alike, writes a record under
`$XDG_STATE_HOME/nix-deploy/` (default `~/.local/state`):

- `runs/<id>/`, where `<id>` is the UTC start time to the microsecond and the
  process ID (`20261027T165425.123456Z-4242`), so names sort by time and
  concurrent runs do not collide. Directories are 0700, files 0600.
  - `run.json`: id, version, start and end time, the deploy arguments
    (after `nixfleet deploy` or `deploy`), the flake directory (the one Nix
    resolves `.` to: the nearest directory with a `flake.nix` from the
    working directory up, not past the Git root), its Git commit and
    whether tracked files differ from it (absent outside a Git checkout), the
    SHA-256 of the flake.lock the run deployed (`lockSha256`), the dry-run
    and force modes, and whether the run failed. End and result stay absent
    while the run goes on, after an interrupt and after a crash.
  - `flake.lock`: the lock the run deployed. It is read at the start and
    again after the evaluate phase, since Nix rewrites a lock that no longer
    matches `flake.nix` while it evaluates (deploy does not pass
    `--no-write-lock-file`, which would change what a run does). When it
    changed, the evaluated lock is `flake.lock` and `lockSha256`, and the one
    the run started with stays as `flake.start.lock` and `startLockSha256`.
    A flake without a lock after evaluation gets one warning.
  - `run.lock`: held locked by the run for its lifetime, so another run's
    prune leaves the directory alone while it is in use.
  - `report.json`: the `--json` document, written whether or not `--json` was
    given.
  - `logs/`: `<phase>-<host>-<step>.log`, with phase `system` or `hm` and
    step `evaluate` or `activate`, holds a step's raw output only where
    deploy already read that output through a pipe or buffer: every
    evaluation, the parallel fleet jobs (independent system switches and all
    fleet Home Manager activations), and system switches staged with
    `switch`, whose output deploy reads for a switch inhibitor. There the log
    is one more writer beside the existing ones and its writes never fail, so
    every command behaves as before. Commands that write to the terminal keep
    it and are not logged: single-host Home Manager activations (including
    the `nix copy` to a remote host), activations staged with `boot` (and the
    `boot` retry after an inhibited switch), the local nix-darwin switch,
    serial ready gates and pre-checks. The shared `nix build` keeps the
    terminal too, since Nix draws its progress bar only there: `build.log`
    summarises it (command, exit status, failed targets), and after a failure
    the run lists what the failed targets still need with `nix build
    --dry-run`: the derivations under "will be built", those that failed and
    those that waited on them, and the paths under "will be fetched". With
    `--keep-going` the build attempted each derivation whose inputs were all
    there, so `nix derivation show` of that list (and, when downloads were
    pending, of their other inputs) picks those none of whose input
    derivations is left to build or to fetch, and only their logs are
    written, as `build-<derivation name>.log`; a waiting one could have an
    old log from an earlier build. Lookups run `nix log --option substituters
    ''`, so they read only this machine's store and never the network, and
    stop at 20
    logs found or 200 lookups; the listing, the selection and the lookups
    together stop after 60 seconds. Nix's log store has a failed build's
    whole log, where the terminal showed only its tail.

At the end of a run the newest `DEPLOY_RUN_KEEP` (default 30) run directories,
the current one and any whose run still holds its `run.lock` are kept; older
ones are removed. A failed run prints `deploy: run record: <dir>` after its
summary.

Phase 1 keeps no record of which lock each target last deployed well from.
Phase 2 derives that baseline for a failed target when it diagnoses: the
`flake.lock` of the newest kept run whose report shows the target `ok` or
`unchanged`, which it may cross-check against what the host runs. That is
evidence, not a record kept exact across concurrent runs. A failure streak
longer than `DEPLOY_RUN_KEEP` can prune the last good run; phase 2 then falls
back to the Git history of `flake.lock`.

The record is evidence, not a safety mechanism. The first failure to create or
write it prints one `deploy: warning: run record: ...` and the run continues
with its usual behaviour and exit status. An interrupt leaves the partial
record in place.

### Phase 2: `nixfleet triage [run]` (decided design)

`nixfleet triage` takes the newest failed run, or the one named, and writes a
`context.md` next to it: the failed targets with their first error and the
files it names, the paths of their logs, the lock diff since each failed
target's last good lock with the store paths of the old and new version of
every changed input, and the Git state of the flake and of the operator's own
inputs. Cheap deterministic hints (for example a removed option or an unknown
argument) may be added to it. A public playbook ships with the tool and is
versioned with it. The command then launches the agent CLI interactively in
the flake checkout, with the context and the playbook.

The runtime configuration of ADR-0006 supplies the agent command (resolved
from a role, not a pinned model), local checkouts of the operator's own
inputs, and an optional private playbook addendum.

#### Warnings

`nixfleet triage --warnings [run]` takes the newest run whatever its result,
or the one named, and writes the same kind of `context.md` for the warnings in
its logs: each distinct `evaluation warning:` and `trace: warning:` message,
with the targets that printed it. The run records already hold every
evaluation's output, so no new recording is needed; warnings printed only to
the terminal (the shared `nix build`, and the activations that keep the
terminal) are out of reach, as for errors.

Each warning is attributed to the code that causes it before anything is
fixed:

- An option warning (renamed, obsolete or ignored option) names the option;
  the module system's `options.<path>.definitionsWithLocations` gives the
  files that define it.
- Any other warning is located by evaluating its target again with
  `--no-eval-cache --option abort-on-warn true --show-trace`. Nix stops at
  the first `builtins.warn` it reaches and labels each trace frame with the
  flake reference of its file, which names the input directly. Since every
  such evaluation stops at that same first warning, a target's warnings are
  attributed in evaluation order: the agent checks that the aborted message
  is the one it is after, then fixes that warning in its worktree, or there
  overrides it for the time being, and evaluates again to reach the next. A
  warning behind one that can be neither fixed nor overridden stays
  unattributed by this method, and is unknown unless the option method or a
  source search for its name locates it. The trace shows where a value was
  forced, not where it was named (a renamed package alias shows the list
  operation that forced it), so the agent then searches that input's source
  for the name.
  Warnings printed through `builtins.trace` are not stopped by
  `abort-on-warn`; they are usually option warnings and covered by the first
  method.
- A file belongs to the operator when it comes from the flake itself or from
  one of the operator's own inputs as the runtime configuration lists them;
  every other input is upstream. Option definitions and some messages name
  bare `/nix/store/<hash>-source` paths, so `context.md` maps every locked
  input to its store path (as `nix flake archive --json --dry-run` lists
  them), not only the inputs whose lock changed.

The playbook then sorts each warning into one outcome:

- **Own code:** fix it in a worktree.
- **Upstream, fixed in a newer revision:** when updating only that input in a
  worktree removes the warning, propose the update, or, where the operator
  pins the input on purpose, a local override that the next update removes
  (for example setting a module's package option to the renamed package).
- **Upstream, not yet fixed:** report it with its evidence and wait; an
  upstream issue or a watch on its release needs the operator's approval.
- **Unknown:** report the evidence and leave it.

A fix is verified like a repair: the affected targets build with
`deploy --build-only`, and evaluating them with `--no-eval-cache` no longer
prints the warning (an evaluation answered from the cache can skip it).

### Phase 3: `deploy` offers triage (decided design)

When `deploy` runs in a terminal and fails, it offers to start triage on the
run it just recorded.

### Phase 4: headless repair in CI (deferred)

Running the same playbook without an operator, for example when a scheduled
flake update fails to evaluate, ending at a fix PR through the normal review,
is deferred to its own issue.

### Limits in every mode

The playbook binds the agent: it edits only in worktrees, never reads
secrets, treats logs, nixpkgs source and forge content as untrusted data, and
never activates, pushes, merges or reboots without the operator's approval.

A repair is verified by evaluating and building every system and Home Manager
target it can affect, without activating any: after a flake update that is
the whole fleet. Evaluation alone is not enough, and one failure can hide the
next. On 2026-10-27 two system evaluation errors hid a certificate bundle
that failed only when built, while every Home Manager configuration evaluated.
Phase 2 therefore adds `deploy --build-only [<host> | --all]` (0.21.0), which
evaluates, plans and builds both phases like a real run and stops before
activation. The playbook repeats it until the whole selection builds.

## Alternatives Considered

- **A deterministic error-pattern fixer only**: too brittle; the messages and
  the fixes change with each nixpkgs release. Cheap pattern hints still feed
  `context.md`.
- **An always-headless repair bot**: rejected for the first version; the fleet
  is live and the private inputs need the operator in the loop.
- **Fixing every warning where it appears**: rejected; a warning raised in
  an input the operator does not own cannot be fixed in the flake without
  forking or overriding that input, so attribution comes first.
- **Launching inside a T3 Code thread**: deferred; a terminal works over SSH
  from any machine, and the record and context are the same either way.

## Consequences

- Each run writes a few files more to the state directory, bounded by
  `DEPLOY_RUN_KEEP`; a failed build adds at most 20 derivation logs and
  spends about a minute at most listing and finding them.
- Every command keeps the terminal or pipe it had before run records, so
  what deploy and the commands it runs print is unchanged. The price is
  that single-host Home Manager, boot-mode and nix-darwin activations, which
  keep the terminal, leave no activation log; their result is still in
  `report.json`, and a fleet run logs Home Manager activations.
- The JSON report is unchanged, and the run layout is the interface phase 2
  reads. The inventory gains one optional `triage` object (ADR-0006), read only
  by `nixfleet triage`; deploy ignores it.
- Locating a warning that names no file costs one more uncached evaluation
  of its target per warning, in order, with the earlier ones fixed or
  overridden first, so warning triage on a target with many warnings takes
  minutes. A warning behind one that cannot be fixed or overridden may stay
  unknown.
