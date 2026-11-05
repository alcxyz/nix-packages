**History of** [ADR-0019](../0019-shared-deployment-reservations.md): text replaced on 2026-11-05.

## Recovery identity clarification, 2026-11-05

Review of the supported recovery amendment replaced this release requirement,
because wall-clock changes can alter a live process's `ps` start time:

> The recorded controller process is absent, or its PID has a different UTC
> start identity. PID namespace must match the inspection endpoint, so a
> container PID cannot be mistaken for a host PID. An endpoint change or missing
> identity is uncertain.

# ADR-0019: Shared deployment reservations

**Status:** Accepted
**Date:** 2026-11-05
**Applies to:** `tools/nix-deploy/`

## Context

A controller-local target lock cannot serialize different group members or
controllers. A connection-scoped lock can vanish while a disconnected
controller's remote activation continues. The owner requires shared protection
from group precheck through readiness and refusal when coordination fails
([issue #592](https://git.alc.xyz/alcxyz/nix-packages/issues/592#issuecomment-50015)).

## Decision

Every serial system deployment, single-host and fleet, reserves all members
on the inventory's operator host over noninteractive SSH, using the inventory's
system user. A remote sudo entry uses `sudo -n`, requiring noninteractive
permission on the authority, rather than activation's interactive sudo prompt.
All supported controllers of the same fleet must use the same operator endpoint. There is no local fallback.

Reservations are atomic directories under `/var/lib/nix-deploy/coordination`,
keyed by the canonical member name's SHA-256 and acquired in sorted order.
Overlapping member sets conflict even when their order differs. Each reservation
has a random ownership token and metadata identifying its member, controller,
process ID and creation time; release checks ownership first. Acquisition fails
on contention or any command failure. The authority rolls back partial acquisition
when it can confirm contention before admitting work. No deployment starts
without all members.

The reservation covers every group precheck, activation and readiness gate.
Successful completion releases it, as does a known admission failure before
any activation starts. A failed or interrupted activation, or an ambiguous SSH
result during acquisition, retains reservations. Release is attempted only when
protected work completed or none started; an acknowledgement lost after deletion
can report a failure even though the reservation is already gone. Reservations
have no timeout and are not removed at startup. Operators must reconcile a
retained reservation and any remote activation before permitting another run.
The metadata and ownership token identify the affected member and invocation;
reconciliation must establish that its controller and remote work have stopped
and check the group's readiness before removing that invocation's reservations.
This is a deliberate availability cost to prevent expired protection admitting
concurrent work. Controller-local failed-activation records still track retries.

The authority needs a POSIX shell, core utilities, directory creation permissions
and noninteractive SSH access. If remote sudo is configured, it must permit these
coordination commands without a password. Acquisition and release commands are bounded.
An unavailable authority blocks work even when every target is reachable.

## Alternatives considered

- Controller-local group locks: do not protect other controllers.
- SSH-held flock or expiring leases: disconnect or expiry can release protection
  before remote work stopped, violating the required failure behavior.
- An external coordination service: adds a service and consumer configuration;
  durable reservations on the existing operator endpoint meet this scope.

## Consequences

No inventory version change or database migration is needed. Existing inventories
remain readable, but serial activation now requires authority access. Consumers
adopt this behavior at their own package update and deployment boundary. A failed
rollout can block subsequent attempts until reconciled. Removing or renaming the
operator or member identifiers also needs coordination with outstanding work.

## Text replaced on 2026-11-05: controller inspection

Automatic cleanup now inspects its own controller locally with root visibility,
without requiring inbound SSH. The previous runtime wording was too broad
because deployment preflight skips the controller:

> Existing deployment preflight already uses `BatchMode=yes` with the inventory's
> system endpoints. Recovery and coordination use that noninteractive contract,
> with no password fallback. A static contract check cannot establish that a live
> agent is unlocked or that a host is reachable.
