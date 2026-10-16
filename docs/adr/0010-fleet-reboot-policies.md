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
