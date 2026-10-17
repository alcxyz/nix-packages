# ADR-0010: Fleet reboots by policy and constraint group

**Status:** Accepted
**Date:** 2026-10-16
**Applies to:** `tools/nix-deploy/` (`nixfleet reboot`)

## Context

After a deploy that moves the kernel, every host needs a reboot, and each
kind of host needs different care. A Kubernetes node goes through the
database, drain, storage and self-fence gates of the node power port
(package `k8snode`). Members of a serial group, such as a quorum cluster or
a redundant DNS pair, must not be down together. A host running CI runners
should let its admitted jobs finish. Rebooting the fleet meant running the
node tool once per node, rebooting the other hosts by hand, and checking
each one before the next.

`nixfleet status` already tells which hosts need a reboot, and the
inventory (ADR-0006) already records the serial groups and their ready
commands for `deploy`. A Kubernetes node could take up to 90 minutes to
settle, so rebooting the whole fleet one host at a time is slow when most
hosts do not depend on each other.

## Decision

`nixfleet reboot [--all | host...]` reboots inventory hosts, each under the
policies its inventory entries give it:

- **Kubernetes node** (new optional inventory field `kubernetesNodes`, whose
  node names equal the host names): the `k8snode` reboot with every gate.
  The node options of the single-node command (`--check-only`, the
  timeouts, `--min-free-pod-slots` and the rest) apply to each node;
  `--resume-maintenance` needs exactly one host. A host the cluster lists
  as a node but the inventory does not is refused, so a missing inventory
  entry cannot turn a node reboot into a plain one. That cross-check needs
  the cluster's node list, so when it cannot be read no host is rebooted.
  Node reboots never overlap, even across groups, since two nodes drained
  at once would each pass gates that did not account for the other.
- **Serial group member:** before the host goes down, every other member
  must pass the group's ready command; after the host returns, it must pass
  the ready command itself.
- **Runner host:** the runner units that were active are stopped first,
  which lets the runner finish its admitted jobs within its own shutdown
  timeout and admit no new ones. If the host then is not rebooted, they are
  started again; after the reboot they must be active.
- **Every host:** the boot ID must change, and the host must then run the
  system it was meant to boot (`bootcheck` reports no reboot needed).
- **Not rebooted:** the operator host and the machine running `nixfleet`
  (its handoff is a later decision), and hosts that do not show a NixOS
  system, which are reported only.

Hosts are rebooted by **constraint group**: each serial group is one group
and every other host is a group of its own. Groups run concurrently;
members of a group reboot one at a time. Within a group, Kubernetes nodes
holding fewer database primaries go first, so a primary moves once, to a
node that already rebooted; otherwise the inventory order holds. A failure
stops its own group and leaves that host in its safe state (cordoned and
drained for a node); the other groups finish, and the run exits non-zero
with a summary of every host.

`--needed` skips hosts whose verdict is a clear "no" (and, for a node, that
is not cordoned); an unknown verdict reboots. Every run first prints the
plan: groups, order, policies and skips with their reasons. `--dry-run`
stops there; a run that changes anything needs a confirmation on a
terminal, or `--yes`.

## Alternatives Considered

- **One host at a time across the fleet:** simplest, but the slowest group
  member's settle time is paid for every independent host too.
- **Detect Kubernetes nodes from the cluster at run time:** needs no
  inventory field, but an unreachable cluster would make a node look like a
  plain host and reboot it without its gates.
- **Rely on the shutdown to stop runners:** the runner already waits for its
  jobs when stopped, but during a shutdown SSH goes away while jobs still
  run, so the operator cannot see the wait or tell it from a hung boot.
- **A separate command for the fleet, keeping `nixfleet reboot` for one
  node:** two commands with overlapping options; a fleet reboot of one node
  is the node reboot plus the host checks.

## Consequences

- One command reboots the fleet after a kernel bump, and the slowest group
  sets the total time instead of the sum of all hosts.
- The inventory gains `kubernetesNodes`; until the configuration emits it,
  a fleet reboot refuses the cluster's nodes.
- Ready commands now also gate reboots, so they must be safe to run before
  a member goes down as well as after it returns.
- Concurrent groups interleave their output, so each line names its host.
- `deploy --all --reboot` and the interactive-host policy are left for later
  changes.

## Amendment (2026-10-16): core singletons and reboot marks

A node reboot also moves the core services that run as a single Pod, such
as the forge and the identity provider, and each move is a short outage.
The order counted only database primaries, and a drained Pod could land on
a node still waiting for its reboot and move again in that node's turn.
The cluster already marks its core tier with a PriorityClass, so the
inventory names that class rather than listing services.

- **Core singletons.** The new optional inventory field
  `kubernetesCorePriorityClass` names the class. A Pod of that class that is
  the only live Pod of its workload is a core singleton. Pods of a
  DaemonSet, a Job or a node (static Pods) do not count, since a node
  reboot does not move them. Within a group, nodes that hold
  fewer database primaries and core singletons together reboot first, and
  the plan lists each node's core singletons. Without the field, only
  primaries count, as before.
- **Reboot marks.** When a run will reboot two or more nodes, each waits for
  its turn with the taint `nixfleet/reboot-pending:PreferNoSchedule`, which
  comes off when its turn ends, whatever the outcome; nodes whose turn never
  came lose it at the end of the run. Pods a drain evicts then prefer nodes
  that already rebooted, and a primary switchover takes a replica on an
  unmarked node first. A run that reboots a node also removes every mark
  it does not set, such as one an interrupted run left; a mark it cannot
  remove stays tracked so the end of the run tries again, and one already
  gone counts as removed. `--dry-run` and `--check-only`
  set no marks.

Alternatives considered:

- **Cordon the waiting nodes:** guarantees one move, but leaves a three-node
  cluster with a single schedulable node for the first reboot, below the
  capacity the drain checks require.
- **Place the core services explicitly (node affinity or a restart after
  each node):** changes workloads that GitOps owns, and fights their own
  spreading rules.
- **List the core services in the inventory:** duplicates the cluster's
  tier, which then has two owners.

The marks are preferences: a Pod whose own spreading preferences outweigh
the taint can still land on a waiting node, and the Pods of the first node
always do. A run that cannot set the marks warns and goes on without them,
since the gates, not the marks, keep the cluster safe.

## Amendment (2026-10-16): interactive hosts, this machine and the boot check

Hosts people use at the screen, and the machine running `nixfleet`, were
left out because a reboot there can end someone's work, or the run itself.
The operator agreed on the following (#548, phase 4).

- **Interactive hosts.** The inventory lists them in `interactiveHosts`
  and names a `rebootGuardCommand` that runs on them, read-only, over SSH
  (through `sudo -n` on hosts reached with sudo, so it can read every
  user's session; locally on this machine). It prints one finding per line: `busy
  <reason>` (someone uses the host: an unlocked desktop session, a game, a
  stream), `block <reason>` (it must not reboot now) and `note <text>`. The
  guard runs for the plan and again at the host's turn. A busy host is put
  to the operator on a terminal and skipped without one or with `--yes`; a
  blocked host is skipped; a guard that fails counts as busy. The host
  configuration owns the detectors, so the tool stays generic.
- **This machine.** It reboots only with `--include-self` and only from a
  terminal, never from a script or an agent session: a run with the flag
  and no terminal is refused before it reads anything. It goes after every
  other group, and only when none of their hosts failed, so a failure is
  still visible and fixable from it. Its guard runs first; a block (a
  running game) refuses, and the operator is asked even with `--yes`, after
  the summary of the rest of the run, since its reboot ends the run. Its own
  reboot has no node or serial gates around it, so a machine that is a
  Kubernetes node or in a gated serial group is refused and reboots from
  another machine. Questions discard input typed before them and read the
  terminal only while they wait, so a stray line cannot answer one and sudo
  gets the password.
- **Boot check (the handoff).** With the inventory's `bootCheck`, every
  NixOS host runs its own check after each boot (a unit of the host
  configuration): it booted its system profile, no unit failed and its
  runners are back. It leaves the result in
  `/run/nixfleet-boot-check/result`, readable by the hosts' system users and
  replaced whole (a temporary file renamed over it); a result read without a
  status is read again. `nixfleet status` shows it, and a fleet
  reboot waits for it after each reboot (up to 12 minutes; a host without
  the check is only warned about). For this machine the check is the only
  verification, since nothing runs from it across its reboot; on hosts
  with the operator's health checks, a failed check also alerts.

Alternatives considered:

- **Reboot this machine from another host, such as the operator's
  laptop:** that host would need the operator's credentials and the run's
  state, and would have to be up and reachable; a check on the rebooted
  host itself needs neither.
- **Detect sessions, games and streams in the tool:** ties a public tool to
  one fleet's desktop, game and streaming setup; the guard command keeps
  that in the host configuration, next to the runner guard that already
  detects games.
- **Record the expected generation before the reboot:** the system profile
  already names what the boot should load, so the check compares against
  it and needs no state from the run.

## Amendment (2026-10-17): every question before the start

The first fleet run with interactive hosts showed that a question asked at a
host's turn does not work: groups run concurrently, so a busy host asked at
the very start of the run, and the other groups' output buried the question
on the same line. The run waited on the operator without making that
visible. This replaces the parts of the previous amendment that ask at a
host's turn and at the end of the run.

- **Questions come first.** After the plan, a run puts every busy
  interactive host to the operator, then this machine with
  `--include-self`, then asks for the confirmation of the whole run, before
  anything changes. Once confirmed, the run needs no attention. A declined
  host, or a busy one with `--yes`, is skipped. A host the guard blocks is
  skipped when the plan is made (a `--check-only` run still checks it). An
  interrupt during the questions ends the run with nothing changed. This
  machine is not asked about when another group already fails before it
  starts, since it then cannot reboot.
- **A turn checks, never asks.** The guard still runs again at the host's
  turn, since the plan's reading may be stale. A block skips the host, and
  so does a busy reason the operator did not confirm before the start (a new
  session, say); the reasons the operator confirmed let it go on. Reasons
  are compared word for word, so a guard names them in stable words (no
  durations or counters in a busy reason).
- **This machine is confirmed before the start too**, even with `--yes`,
  and sudo takes the password then, renewed every minute until its reboot.
  At its turn its guard runs again under the same rule, so a game started
  meanwhile still refuses. When the renewal fails, sudo asks at the end,
  when no group prints any more.

Holding the other groups' output while a question waits was considered: it
keeps the question visible but still stalls the run on the operator at an
unpredictable moment, which is what the operator wants to avoid.
