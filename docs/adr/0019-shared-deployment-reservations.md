# ADR-0019: Shared deployment reservations

**Status:** Accepted
**Date:** 2026-11-05, updated 2026-11-05 (supported reservation and reboot runner recovery)
**History:** [replaced text](archive/0019-shared-deployment-reservations-history.md)
**Applies to:** `tools/nix-deploy/`

## Context

A controller-local target lock cannot serialize different group members or
controllers. A connection-scoped lock can vanish while a disconnected
controller's remote activation continues. The owner requires shared protection
from group precheck through readiness and refusal when coordination fails
([issue #592](https://git.alc.xyz/alcxyz/nix-packages/issues/592#issuecomment-50015)).
[The recovery follow-up](https://git.alc.xyz/alcxyz/nix-packages/pulls/762#issuecomment-54283)
requires a supported way to inspect and release stopped work, and automatic
release after definite failures, rather than manual authority-file maintenance.

## Decision

Every serial system deployment, single-host and fleet, reserves all members
on the inventory's operator host over noninteractive SSH, using the inventory's
system user. A remote sudo entry uses passwordless `sudo -n`, rather than
activation's interactive sudo prompt. All controllers of the same fleet must
use the same operator endpoint. There is no local fallback.

Reservations are atomic directories under `/var/lib/nix-deploy/coordination`,
keyed by the canonical member name's SHA-256 and acquired in sorted order.
Overlapping member sets conflict even when their order differs. Each invocation
has a random ownership token. Metadata records the member, group, controller,
PID, UTC process-start and PID-namespace identity, creation time, inventory endpoints, last
active member and reason for holding protection. Optional added fields are
read compatibly; legacy or incomplete metadata does not establish safety.

Every authority operation uses the same kernel `flock`, with a bounded wait.
This serializes acquisition, metadata updates, listing and release, including
ownership rechecks. It is a short operation lock, not the deployment reservation:
its release or a connection loss never expires the durable reservation. Known
partial acquisition failures roll back on the authority. An acquisition whose
success acknowledgement is lost may leave reservations, but activates nothing.
Every successful operation must return a fresh acknowledgement bound to its
exact script and ownership token, so SSH discarding stdin cannot admit work.

Protection covers group prechecks, activation and readiness. Successful
completion releases it. Admission failures before any activation also release
it, including cancellation before activation starts. A definitely completed
non-zero activation result releases automatically only after every member and
the controller report no remaining activation work. The failure remains in the
run report and failed-activation record, allowing a fixing deployment to retry.
The running controller inspects itself locally, as root or through `sudo -n`,
including when it is outside the inventory. This requires no inbound SSH to
the controller. Unavailable local privileged inspection still retains protection.
A failed ready gate after a completed activation can release under that same
idle-work inspection. Every later rollout still has to pass its normal precheck.

Lost connections, timeouts, interruptions during activation, signal exits and
unavailable work inspection retain protection, with the last known reason and
an exact recovery command. Reservations have no lease, timeout or startup
removal. This preserves the original invariant: uncertain work must not admit
an overlapping activation. A failed rollout whose work definitely stopped does
not need protection solely because it failed.

## Supported recovery

`nixfleet reservations [list] --config FILE` lists held IDs, members and groups,
controller host, process, age and last known reason. A blocked deployment names
the exact ID and `nixfleet reservations release ID --config FILE` command.

Release applies to the invocation owning the selected ID. By default it requires:

- The recorded controller PID is absent. A matching UTC start identity counts
  as running; a mismatch is uncertain because wall-clock changes can alter
  `ps` start times even without PID reuse. PID namespace must match the
  inspection endpoint, so a container PID cannot be mistaken for a host PID.
  An endpoint change or missing identity is uncertain.
- Every group member and controller is reachable through its recorded inventory
  endpoint and reports no known activation process. The controller is inspected
  too, because an orphaned rebuild frontend could resume work on idle targets.
- Activation-target inspection has root visibility. On Linux, pending system
  jobs also count as potentially running work. Failed or unavailable checks
  never count as idleness.

Positive process or work activity always refuses release. When safety cannot be
established, `--confirm-stopped` is an explicit operator attestation that the
controller and remote work have stopped. It permits unavailable or legacy
inspection, never a positively busy result. There is no silent removal.

Under the authority operation lock, release rechecks ownership against the
listing, logs a release request, renames the owned directory, logs the release,
and removes only the renamed invocation's files. A stale release cannot delete
a new owner. `releases.jsonl` records IDs, invocation, member, controller, PID,
time, reason and whether confirmation was required; the command also reports
what it released. An acknowledgement lost after deletion can report failure
even though protection is already gone, so listing is authoritative for a retry.

The process inspection covers supported rebuild and activation commands and
pending system jobs; it cannot prove the absence of arbitrary custom detached
work or future external scheduling. Unavailable inspection requires explicit
confirmation. Changing authority, endpoint or member identifiers requires
coordination with outstanding work.

## Runner recovery after an unknown reboot outcome

Before stopping runners, a reboot run requires a working version 1 host report
and readable runner hold, refusing unsupported hosts before recording or
draining anything. It publishes a root-owned record under
`/run/nixfleet-reboot-runners`, using an atomic directory rename. It contains
a random owner token, the controller's PID, UTC start and PID namespace
identity, its inventory endpoint, and the units actually active at the host's
stop-time check. The record is readable by status without root. A stale plan
cannot record an already inactive, hand-stopped or held unit. Publication
precedes stopping, so a lost acknowledgement cannot lose the recovery set.
A bounded host `flock` serializes record publication, stopping and resuming;
a later reboot run cannot overwrite an outstanding record, even when all
its runner units are already inactive.

This record is durable across controller exits, interruptions and SSH loss,
but boot-scoped: `/run` clears on a real reboot. Enabled units start with boot
as before, and no stale reboot record or startup migration blocks later work.
An uncertain reboot still leaves runners stopped. Neither a matching old boot
ID nor a missing SSH acknowledgement triggers a start.

`nixfleet runners status` identifies this record as "stopped by a reboot run
with an unknown outcome" and prints `nixfleet runners resume HOST --config FILE`
with the exact inventory and host. Resume:

- Requires the recorded reboot controller to have exited, using the same
  identity rules as reservation release. When it is this machine, inspection
  is local with root visibility. Otherwise its recorded inventory endpoint
  must still match and be reachable. Busy, unknown or unavailable inspection
  refuses, with no confirmation override. Root-visible work inspection also
  excludes surviving SSH children that could still dispatch a reboot, even
  after the controller PID has exited.
- Acquires a reservation for that host on the shared authority. This conflicts
  atomically with any deployment reservation containing the host. An unavailable
  authority refuses. Protection lasts through a fresh acknowledgement of the start result; an uncertain
  resume retains it for inspection through `reservations release`. Work
  inspection also recognizes host reboot-runner operations still holding their
  `flock`, so a lost connection cannot release an outstanding start.
- Under the host lock, rechecks record ownership, refuses scheduled shutdown,
  pending system jobs, an active shutdown target or a shutdown command still
  running, and requires complete successful inspection.
- Requires the version 1 host runner report, an active hold reporting `none`,
  no active restart, and recorded units either already active or inactive with
  the host's ordinary stopped reason. Held, gated, disabled, reset, failed,
  transitional or unknown states refuse before any start. The ordinary stopped
  reason is part of this additive recovery contract; unknown wording refuses.
- Starts only recorded units still inactive, through ordinary `systemctl start`
  and the units' existing start gates. It never resets failures, overrides a
  gate or starts an unrecorded unit. A hold that begins after inspection still
  has its start gate. A skipped or failed start retains the record for retry;
  units already active are left alone. Unit definitions removed by a later
  deployment are skipped rather than started. Success atomically retires the
  owned directory before deleting its contents; interrupted cleanup cannot
  leave a partial blocking record.

Resume is an explicit action and needs no `--yes`, like `reservations release`.
A terminal validates local sudo before any local controller or target probe.
Noninteractive sessions require preauthorized local sudo. Inventory and
endpoint changes still require coordination with outstanding work; an
unreachable or removed controller cannot be assumed idle, but recovery on
the original controller uses local inspection.
It differs from `runners restart`, which only replaces running units with older
definitions. No record means no work, including after a real reboot. Historical
runs from before this recording support cannot be reconstructed from inactive
units and are never guessed from logs. The same host safety checks apply to a
reboot run restoring runners after a definite pre-reboot failure.

## Runtime contract

The authority needs a POSIX shell, core utilities and `flock`, directory creation
permission and noninteractive SSH access. Linux NixOS supplies `flock` through
its base system utilities; controllers need not run the authority's OS. Remote
sudo, when configured, must allow coordination without a password. Commands and
work inspections are bounded. An unavailable authority blocks serial work even
when every target is reachable. Linux controllers package `ps`; Darwin uses its
system `ps`.

Remote-target deployment preflight already uses `BatchMode=yes` with the
inventory's system endpoints; it skips the controller. Coordination requires
noninteractive authority access. Automatic failure cleanup checks the controller
locally with root visibility, and configured remote members through SSH.
Manual recovery checks the recorded controller through SSH; an outside-inventory
or unavailable controller requires explicit stopped-work confirmation. None of
these checks permits a password fallback. A static contract check cannot
establish that a live agent is unlocked or that a host is reachable.

## Alternatives considered

- Controller-local group locks: do not protect other controllers.
- SSH-held deployment flock or expiring leases: disconnect or expiry can
  release protection before remote work stopped.
- Retain every failed activation indefinitely: unnecessary once work definitely
  stopped, and can prevent the deployment that fixes the failure.
- Hand removal of authority files: lacks a supported safety check, ownership
  recheck and audit trail, and creates avoidable operator work.
- An external coordination service: adds a service and consumer configuration;
  durable reservations and serialized operations on the existing authority meet
  this scope without another service.

## Consequences

No inventory version change, database migration, host unit or startup migration
is needed. Existing inventories remain readable. Consumers adopt the behavior
at their own package update and deployment boundary. Ordinary definite failures
release automatically when inspection succeeds; ambiguous cases have a supported,
audited recovery command. Controller-local failed-activation records still track
retries. No consumer input pin, host deployment or release changes as part of
this implementation.
