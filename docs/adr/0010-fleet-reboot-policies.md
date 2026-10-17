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

## Amendment (2026-10-17): only streaming, caught-up replicas take over

The first fleet run promoted a replica that CloudNativePG reported healthy
but that had stopped streaming hours earlier: it had been away longer than
its replication slot could hold WAL, and nothing rebuilt it. The switchover
rolled the database back to where that replica stopped. Readiness alone does
not show this; only the primary's `pg_stat_replication` does.

- **The primary is asked.** Before a switchover, after maintenance and in
  the one-node-down gate, a run asks each cluster's primary which replicas
  stream (`kubectl exec` and `psql` in the primary's `postgres` container;
  a replica cluster's designated primary measures from the later of the
  WAL it received and replayed). A primary that cannot be asked, or is not
  Ready, is reported with the reason, never as replicas that stopped
  streaming, and fails the checks; the operator needs `pods/exec` there.
- **Switchover candidates must stream and be caught up**, at most one WAL
  segment (16 MiB) behind the primary's WAL; an unmarked node is still
  preferred among them. A replica that streams but trails under load is
  waited for up to the settle timeout; without one, the run stops before
  the cordon, at once when no replica could come within reach.
- **Full health means every replica streams.** A replica that is Ready but
  does not stream stops the run at the preflight or after the node returns,
  with the hint that only a rebuild (`kubectl cnpg destroy`) brings back a
  replica whose WAL is gone. The run never rebuilds a replica itself: that
  discards its copy, which the operator may still need.
- **One node down keeps a streaming replica** in clusters of three or more
  instances: the gate counts only streaming replicas away from the node, so
  a drain never leaves their synchronous primary without a standby to
  confirm its commits. A two-instance cluster keeps no replica with one
  node down; requiring one would make it impossible to maintain.

## Amendment (2026-10-17): room before the drain

The first fleet run drained a node whose Pods the other nodes could not
hold: the scheduler places Pods by their CPU requests, and the remaining
nodes' free requests fell short. The availability gate then waited the full
settle timeout (90 minutes) for replicas that could never start, and the
failure left the node cordoned, which kept its room from those same Pods.
The Pod-slot check before the cordon counts Pods, not CPU or memory.

- **The needed Pods must fit before anything changes.** Before the
  database switchover and the cordon, a run places the Pods the
  availability gate will need elsewhere on the remaining Ready, schedulable
  nodes. `--check-only` reports the same.
  - *Which Pods:* per controller, as many as its Ready replicas on other
    nodes fall short of the gate's minimum: those some remaining node may
    run, smallest first, or for an ordered StatefulSet the lowest ordinals,
    which it recreates first; not those of a controller pinned to the node
    or tolerating its cordon, a Job or a DaemonSet.
  - *Their size:* an upper bound on the replacement, from the union of the
    Pod, which carries what admission added (LimitRange defaults, injected
    sidecars, RuntimeClass overhead), and the template that recreates it
    (its own ReplicaSet's, or for a StatefulSet the revision its ordinal
    gets), which an in-place resize does not change: every container at the
    larger of its two requests (as on admission, a limit standing in for a
    missing request, per container and at Pod level, and then the largest
    of the namespace's LimitRange defaults; a template that relies on them
    stops the check when they cannot be read, as does a recreating
    ReplicaSet, StatefulSet or revision that cannot be read), each init container beside the sidecars that
    start before it on either side, and the larger Pod-level request. It is
    exact when the Pod matches the template. Pods already on a node hold
    what the node allocated them during a resize, or only that, for the
    resources it reports, when the resize is infeasible.
  - *Where:* the largest first on the node with the largest free share of
    CPU and memory, like the default scheduler's spreading; when that
    leaves one out, a search for any placement within a bounded number of
    steps, the Pods with the fewest nodes first. A total beyond the nodes'
    free room settles it at once, and interchangeable Pods (same namespace,
    labels and requests, no anti-affinity of their own) are tried in each
    arrangement once. It honours node selectors, required node affinity,
    taints and required hostname anti-affinity both ways (the Pod's and the
    running and placed Pods').
  - *Outcome:* a needed Pod no node may run, or that the search settles
    fits nowhere, stops the run; one the search cannot settle within its
    steps is a warning. The drain's other Pods are placed after them, and
    those some node may run but none has room for are only warned about,
    since the scheduler places Pods in no such order. (One no node may run,
    such as a one-per-node replica with too few nodes left, waits for the
    node by design.)
  - *Limits:* placement uses the running Pod's constraints, not a changed
    template's, and leaves out spread constraints, preferences, required Pod
    affinity, host ports, volume node affinity and extended resources; the needed replicas are chosen by whether some node may
    run them and by size (CPU, then memory), not by room, so a refusal can
    name a replica while another would have fit; and the size may exceed
    the replacement's. The check therefore catches a shortage early but
    guarantees nothing; the gate after the drain still decides.
- **An unplaceable replica fails fast.** After the drain, nothing frees room
  on its own. Once a controller's live Pods (a rollout's surge Pods
  included) that have not each been reported Unschedulable for five
  minutes are fewer than its gate's minimum, the availability gate fails
  with the scheduler's reasons. A Pod of another owner and a preemptor
  waiting for its victims do not count; a Pod whose ReplicaSet cannot be
  read counts as a replica but never as stuck.
- **A failure before the storage detach uncordons.** This replaces "cordoned
  and drained" as the safe state for a node whose drain or availability
  gate failed: it still runs untouched, and staying cordoned only takes its
  room away. From the storage detach on, which may change the host's
  storage sessions, the node keeps its cordon, and so does a node that
  `--resume-maintenance` found cordoned and a run with `--leave-cordoned`,
  whose error then says it remains cordoned, and a run that was
  interrupted, which says the node may need inspection.
- **The drain outlasts the slowest Pod.** Without `--drain-timeout`, the
  drain timeout grows to the longest grace period of the node's evictable
  Pods plus two minutes, since the drain waits for each to stop; a shorter
  chosen timeout is kept with a warning, and zero is passed on unchanged.

Requiring room for every displaced Pod was considered: it would refuse
maintenance whenever a replica the gate may lose does not fit.
Requiring headroom for a whole node at all times was considered; it is a
capacity policy for the cluster's manifests, which the check makes visible
without enforcing. Lowering requests or scaling workloads down from the
tool was rejected: it would change workloads the tool only moves.

## Amendment (2026-10-17): a hook around planned reboots

A planned fleet reboot trips the cluster and host health alerts of every
node it reboots, and each fires again on recovery, so the operator learns
nothing the run does not already know and real alerts drown among them.
nixfleet knows when each host's maintenance starts and ends, but how the
operator's monitoring is quieted and where progress is announced is the
operator's setup, not this tool's.

- **The inventory names a hook.** `rebootHookCommand` runs on this machine
  through `sh`, at `run-start` (the hosts to reboot), `host-start`,
  `host-finish` (outcome, reason, duration) and `run-finish` (a line per
  host and whether one failed). The event and its data are environment
  variables (`NIXFLEET_EVENT`, `NIXFLEET_HOST`, `NIXFLEET_KIND`, and so on);
  `nixfleet reboot --help` lists them.
- **Events pair up and never overlap.** `host-finish` follows every
  `host-start`, after a failure or an interrupt too, so whatever a hook
  paused is resumed. A node's events fall inside its node turn, so the next
  node starts only after the previous one finished, after its cluster
  checks settled. This machine's turn ends with its reboot, which comes
  after `run-finish`, so it gets `host-finish` only when the reboot does
  not happen; SIGTERM while its reboot command runs may be the shutdown
  itself and sends none, while the operator's interrupt leaves it up and
  finishes its turn. Calls are serialized, run
  on their own clock (so an interrupted run still reports) and are bounded
  at 60 seconds, plus up to 5 seconds for a hook that ignores the stop;
  each runs in its own process group, which the deadline stops and which
  is killed when the hook ends, so nothing left in that group acts after
  its event (a helper that leaves the group, say with `setsid`, is the
  hook's to stop).
- **A hook never stops a reboot.** A failing hook is a warning: quiet
  monitoring is a convenience, and a reboot that waited on it would make
  the monitoring a gate. `--check-only` and `--dry-run` change nothing and
  call none.

Silencing monitoring from nixfleet itself was considered: it would tie a
public tool to one monitoring stack and its credentials, which belong to
the operator's private configuration.
