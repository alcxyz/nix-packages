# ADR-0016: nixfleet triage: run records and an agent playbook for failed deploys and warnings

**Status:** Accepted
**Date:** 2026-10-27, updated 2026-10-31 (phases 2 and 3 built in releases 0.21.0 to 0.26.0, per nix-packages#673)
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

Triage comes in phases (nix-packages#673). The repository's `README.md` and the
playbook it ships (`tools/nix-deploy/triage-playbook.md`) hold the details;
this record keeps the decisions and the limits.

### Phase 1: run records (release 0.19.0)

Every `deploy` run that gets past argument parsing, single-host, `--all`,
`--dry-run` and `--build-only` alike, writes a record under
`$XDG_STATE_HOME/nix-deploy/` (default `~/.local/state`):

- `runs/<id>/`, where `<id>` is the UTC start time to the microsecond and the
  process ID (`20261027T165425.123456Z-4242`), so names sort by time and
  concurrent runs do not collide. Directories are 0700, files 0600.
  - `run.json`: id, version, start and end time, the deploy arguments
    (after `nixfleet deploy` or `deploy`), the flake directory (the one Nix
    resolves `.` to: the nearest directory with a `flake.nix` from the
    working directory up, not past the Git root), its Git commit and
    whether tracked files differ from it (absent outside a Git checkout), the
    SHA-256 of the flake.lock the run deployed (`lockSha256`), the dry-run,
    build-only and force modes, and whether the run failed. End and result
    stay absent while the run goes on, after an interrupt and after a crash.
  - `flake.lock`: the lock the run deployed. It is read at the start and
    again after the evaluate phase, since Nix rewrites a lock that no longer
    matches `flake.nix` while it evaluates (deploy does not pass
    `--no-write-lock-file`, which would change what a run does). When it
    changed, the evaluated lock is `flake.lock` and `lockSha256`, and the one
    the run started with stays as `flake.start.lock` and `startLockSha256`.
    A flake without a lock after evaluation gets one warning.
  - `run.lock`: held locked by the run until its record is finished (end,
    result and report written), so another run's prune leaves the directory
    alone while it is in use. It is released then rather than at exit, so
    triage can read the run before deploy ends (0.26.0).
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

The record is evidence, not a safety mechanism. The first failure to create or
write it prints one `deploy: warning: run record: ...` and the run continues
with its usual behaviour and exit status. An interrupt leaves the partial
record in place.

### Phase 2: `nixfleet triage` (releases 0.21.0 to 0.25.0)

`nixfleet triage [--config FILE] [--no-launch] [--warnings] [<run>]` writes a
0600 `context.md` into a run directory, prints its path and then launches
the operator's agent on it. It reads only a finished run: one that no longer
holds its `run.lock` and has a `report.json`. Apart from `context.md` it
writes nothing and runs no Nix command; Git is read only, without optional
locks or lazy fetches, with a 10-second timeout per call.

#### Failed runs (releases 0.22.0 and 0.23.0)

The context came in 0.22.0 and its lock diff and store paths in 0.23.0.
Without `--warnings`, triage takes the newest run whose `run.json` says it
failed, or the one named (an ID or a path). Its `context.md` has the run's
facts, including the flake's recorded Git revision and whether tracked files
differed from it; each failed target with the step that failed (evaluate, build,
activate, an activation or its setup without a log, the serial ready gate,
or unknown), all its logs, and the first `error:` block of its most
relevant log, cut at 40 lines, with the files it names (a build failure
points to the build errors section instead); the first errors of the
derivation build logs, once for the whole run; the lock diff; the locked inputs; and the own inputs.

Phase 1 keeps no record of which lock each target last deployed well from,
so triage derives that baseline per failed target: the `flake.lock` of the
newest earlier kept run of the same flake directory whose report shows the
target `ok` or `unchanged`. A dry run's `unchanged` counts, since the host
runs it; a build-only run's `built` does not, since it proves no deploy.
That is evidence, not a record kept exact across concurrent runs. Triage
may cross-check it against what the host runs; that check is not built. A failure streak longer than
`DEPLOY_RUN_KEEP` can prune the last good run; triage then falls back to Git:
the newest of at most 50 commits at or before the run's revision, following
the plain history of `flake.lock` (merges that resolved it included), whose
lock differs from the run's. It is labelled an approximation, not a known
good deploy, and the search ends without a baseline at an object a partial
clone lacks. Targets with the same baseline share one diff of added, removed
and changed inputs, with their revisions, dates, sources and store paths.

Every locked input is listed with its store path, computed offline from its
`narHash` and its locked store name (`source` unless the lock names
another), and whether that path is in the local store, so bare
`/nix/store/<hash>-source` paths in errors and option definitions map to
inputs.

Failure context carries no error-pattern hints (a removed option, an unknown
argument): none were built. The playbook instead tells the agent which
messages removed and renamed options leave in the new source. Such hints
may still be added to `context.md` where a pattern proves stable.

#### Runtime configuration and launch (release 0.24.0)

The inventory of ADR-0006 (`--config FILE` or `NIX_DEPLOY_CONFIG`) may carry
an optional `triage` object, and triage validates only that object, so an
error elsewhere in the inventory does not stop it:

- `agentCommand`: the argv of the operator's agent CLI, passed through as
  given. It should resolve a role or a floating alias, not a pinned model;
  the tool does not check that.
- `inputCheckouts`: the local checkout of each of the operator's own inputs,
  by input name. A file belongs to the operator when it comes from the flake
  itself or from one of these inputs; every other input is upstream.
  `context.md` gives each checkout's branch, HEAD, whether tracked files
  differ, and whether HEAD is the revision the run's lock pins. A missing
  checkout only warns.
- `playbookAddendum`: a private Markdown file the agent also follows. It
  adds private details and cannot relax the playbook's limits.

The public playbook ships with the tool in `share/nixfleet/triage-playbook.md`
and is versioned with it. Triage launches the agent only with an
`agentCommand`, a readable inventory, standard input and output on a
terminal, and no `--no-launch`; otherwise it writes the context, prints why
it did not launch and exits 0. The agent runs in the run's flake directory
on the inherited terminal, with one more argument, a prompt naming
`context.md`, the playbook and the addendum, and with `NIXFLEET_TRIAGE=1`
in its environment. Triage exits with the agent's status and passes a
termination or hangup on to it.

#### Warnings (release 0.25.0)

`nixfleet triage --warnings [<run>]` takes the newest finished run whatever
its result, or the one named, and writes a `context.md` of the warnings in
its evaluation logs: each distinct `evaluation warning:` and `trace:
warning:` message, with the targets that printed it, the files it names
mapped to their locked input and marked own or upstream, and the names its
wording calls options, as hints. The run records already hold every
evaluation's output, so no new recording is needed; warnings printed only to
the terminal (the shared `nix build`, and the activations that keep the
terminal) are out of reach, as for errors. The context has no lock diff. It
compares the flake checkout now with the run's revision, dirty state and
lock, since the commands it lists evaluate the checkout as it is now, and
gives per target its flake attribute and a trace re-evaluation
(`nix eval --no-eval-cache --option abort-on-warn true --show-trace`),
which nixfleet does not run. The launch prompt says it is warning triage.

Each warning is attributed to the code that causes it before anything is
fixed, as the playbook's Warnings section describes:

- An option warning (renamed, obsolete or ignored option) names the option;
  the module system's `options.<path>.definitionsWithLocations` gives the
  files that define it. An option inside a submodule or an `attrsOf` entry
  has no path of its own, so the agent walks up to the nearest enclosing
  option whose lookup works, or searches the sources. Triage generates no
  lookup commands.
- Any other warning is located by its target's trace re-evaluation. Nix
  stops at the first `builtins.warn` it reaches and labels each trace frame
  with the flake reference of its file; the context maps such a reference
  to an input by its revision and source, and reports a revision several
  inputs share as ambiguous. Since every such evaluation stops at that same
  first warning, a target's warnings are attributed in evaluation order:
  the agent checks that the aborted message is the one it is after, then
  fixes that warning in its worktree, or there overrides it for the time
  being, and evaluates again to reach the next. A warning behind one that
  can be neither fixed nor overridden stays unattributed by this method, and
  is unknown unless an option lookup or a source search for its name
  locates it. The trace shows where a value was forced, not where it was
  named (a renamed package alias shows the list operation that forced it),
  so the agent then searches that input's source for the name. Warnings
  printed through `builtins.trace` are not stopped by `abort-on-warn`; they
  are usually option warnings and covered by the first method.

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

### Phase 3: `deploy` offers triage (release 0.26.0)

When a recorded run fails (`--dry-run` and `--build-only` included) with
standard input and output on a terminal, an `agentCommand` in a valid
`triage` object and `NIXFLEET_TRIAGE` unset, deploy finishes the record,
releases its build roots and asks `deploy: start nixfleet triage on this
run? [y/N]`. `y` or `yes`, in any case, runs triage on that run in the same
process, which writes `context.md` and launches the agent; anything else,
end of input, a read error or 64 bytes without a newline declines. deploy
then exits with its own failure status, not the agent's. `--json` does not
change the condition. Since triage sets `NIXFLEET_TRIAGE=1`, a deploy the
agent runs during a repair never offers triage again. Without the offer,
output and status are unchanged.

### Phase 4: headless repair in CI (deferred)

Running the same playbook without an operator, for example when a scheduled
flake update fails to evaluate, ending at a fix PR through the normal review,
is deferred to nix-packages#719.

### Limits in every mode

The playbook binds the agent: it edits only in worktrees, never reads
secrets, treats logs, nixpkgs source and forge content as untrusted data, and
never activates, pushes, merges or reboots without the operator's approval.

A repair is verified by evaluating and building every system and Home Manager
target it can affect, without activating any: after a flake update that is
the whole fleet. Evaluation alone is not enough, and one failure can hide the
next. On 2026-10-27 two system evaluation errors hid a certificate bundle
that failed only when built, while every Home Manager configuration evaluated.
Phase 2 therefore added `deploy --build-only [<host> | --all]` (0.21.0), which
evaluates, plans and builds both phases like a real run and stops before
activation. The playbook repeats it until the whole selection builds.

The terminal test that gates the launch and the offer accepts any character
device but the null device, so a device such as `/dev/zero` passes it. The
offer bounds its answer to 64 bytes, so an endless byte source cannot keep
it reading, but an idle character device that is not a terminal can still
block the read, and keystrokes typed during a long deploy stay buffered and
can answer it; the default is No, so a stray Enter declines.

## Alternatives Considered

- **A deterministic error-pattern fixer only**: too brittle; the messages and
  the fixes change with each nixpkgs release. Error-pattern hints in
  `context.md` remain possible but are not built.
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
- The JSON report is unchanged, and the run layout is the interface triage
  reads. The inventory gains one optional `triage` object (ADR-0006), read by
  `nixfleet triage`, and by deploy after a failed run in a terminal, which
  offers triage only when the object is valid and has an `agentCommand`.
- Locating a warning that names no file costs one more uncached evaluation
  of its target per warning, in order, with the earlier ones fixed or
  overridden first, so warning triage on a target with many warnings takes
  minutes. A warning behind one that cannot be fixed or overridden may stay
  unknown.
