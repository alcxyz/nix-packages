# ADR-0017: nixfleet runner status and restarts

**Status:** Accepted
**Date:** 2026-11-02, updated 2026-11-05
**History:** [replaced text](archive/0017-nixfleet-runner-status-and-restarts-history.md)
**Applies to:** `tools/nix-deploy/` (`nixfleet status`, `nixfleet runners`), and the host side in nix-config's runner module

## Context

The CI runner hosts keep their runner units, their engine APIs and their
resource policy running across a switch (`restartIfChanged = false`), so a
deploy never cuts a running job short. The cost is that a changed runner
label, name, CPU limit or engine package only applies at the unit's next
start, and nothing said which units still ran an older definition. On
2026-11-02 a new label and a raised CPU ceiling needed hand-run restarts on
all four runner hosts, and later label and name changes need the same.

`nixfleet status` counted runner units by name patterns that missed the
extra Podman runners (`forgejo-podman-runner-fast`), so it showed `2/2` on
hosts running three, `nixfleet reboot` did not stop those before a reboot,
and nothing said why a runner was down. The host's runner hold (nix-config
ADR-0106) stops runners for a game or a low disk, a start gate refuses
starts for the same reasons, and the Podman store reset stops them while it
resets; the hold's markers are root-only, while `nixfleet status` reads hosts
as their system user.

systemd keeps no "running an older definition" flag: after the switch's
daemon reload, a running unit reports the new unit file.

## Decision

**Host contract.** A runner host built with nix-config's runner module
provides, readable without root:

- **Start records.** Every unit a switch does not restart (the runner units,
  `forgejo-runner-docker`, `forgejo-runner-podman` and
  `forgejo-runner-resource-policy`) runs a root `ExecStartPost` that writes
  its invocation ID and the resolved store paths of its unit file and
  drop-ins to `/run/nixfleet-runners/started/<unit>`. A unit needs a restart
  when it runs and its record is missing, belongs to another invocation, or
  names other paths than the deployed unit file. A unit that does not run
  starts on the deployed definition, so it never needs one. A failed record
  never fails the start; it leaves the unit flagged.
- **`/run/current-system/sw/bin/nixfleet-runner-status`** prints one record
  per line, tab-separated, `-` for an empty field: `version 1`; `hold` with
  the runner hold's state and reason (`none`, `game`, `disk_space`, ...);
  `restart-unit` with the restart unit and its wait limit in seconds;
  `restart` with the restart unit's state and its last run's status, time,
  units and reason, once it has run; and per unit `unit` with its kind
  (`runner` or `service`), unit, ActiveState, SubState, whether it needs a
  restart and why, its label names, and why a runner does not run. The host
  writes the reasons, from systemd's state and result, the hold's status
  text and the store reset's state, so nixfleet carries no knowledge of the
  hold. nixfleet reads only version 1, and records the command's exit
  status after its output: another version, a failed command or a report
  without that exit record is an error, never healthy runners or none.
- **`forgejo-runner-restart.service`**, a root oneshot, restarts the flagged
  units once their running jobs finish. It waits until no Podman store reset
  runs and no runner is starting or stopping, then pauses the runners the
  restart touches: the flagged ones, or all of them when an API or the resource
  policy is flagged, since restarting those restarts the runners that require
  them. Other runners keep working. A paused runner takes no new job, lets its
  running ones finish with no time limit, and reports `paused tasks=<running>`
  as its systemd status text. The pause is one file per runner,
  `/run/nixfleet-runners/paused/<unit>`, that the runner watches through
  `FORGEJO_RUNNER_PAUSE_FILE`, a patch in this repository's
  `forgejo-runner-cancellation`. Once every paused runner reports no tasks, it
  restarts the flagged units in one `systemctl restart`, checks that they run
  the deployed definition, and removes the pauses; a pause also ends with the
  unit, however it ends. It gives up and restarts nothing when a wait outlasts
  its limit, by default the job timeout plus 5 minutes, and fails when a
  running runner has not reported the pause within 60 seconds, which means it
  runs a build without the patch. Its progress and result (`waiting`, `restarting`, `done`,
  `nothing`, `busy`, `failed`) go to the world-readable
  `/run/nixfleet-runners/restart`, which the status command reports. It
  leaves the runner hold as it is: a restarted runner whose gate refuses the
  start is the hold's to start later.

**nixfleet.**

- `nixfleet status` runs the status command where it exists. The runner
  column counts the `runner` units; notes explain each runner that does not
  run, say how many units need a restart, and report a restart under way,
  busy or failed. `--json` carries every field. Without the command it
  lists units named `gitea-runner-*`, `gitea-*runner`, `forgejo-*runner` and
  `forgejo-*-runner-*`, with their states only. `nixfleet reboot` stops and
  starts the runner units status lists, and fails a host whose runner report
  is an error rather than reboot runners it cannot see.
- `nixfleet runners [status]` lists every runner and tracked unit with its
  state, restart flag and reason, then the hold's reason and the last
  restart run.
- `nixfleet runners restart [--all | <host>...] [--dry-run] [--yes]` plans
  first (what each host would restart and why, or why it is skipped), asks
  on a terminal or needs `--yes`, then runs `systemctl start` of each
  host's restart unit concurrently, over SSH as the system user with
  `sudo -n` where the inventory says so, prints its progress, and reports
  each host's outcome. The host unit goes on if nixfleet is interrupted. It
  exits 1 when a host could not be read, has no restart unit, stayed busy or
  failed.

Drain and resume need no command: under the runner hold a drain is an
ordinary `systemctl stop`, which the hold leaves alone, and a resume is
`systemctl start`. Registrations on the Forgejo side stay manual.

## Alternatives Considered

- **nixfleet waits for idle runners and restarts them itself.** No host unit
  is needed, but the wait ends with the SSH session or the operator's
  machine, and the idle checks (engine sockets, journals) need root and host
  knowledge that the runner module already has for the store reset.
- **Restart automatically after each switch**, through the runner hold or a
  unit the switch starts. No operator step, but restarts would come at
  unplanned times, a bad runner configuration would reach every runner
  without anyone watching, and the hold would gain a second job. The host
  unit can be started that way later without changing the contract.
- **Wait for an idle host, with the runners still polling.** Needs no patch,
  but a busy host may never get idle: on 2026-11-04 and 2026-11-05 xev gave up
  three times in a row.
- **Restart at once and let the stop drain.** A stopping runner stops polling
  and finishes its jobs, so this needs no patch either. But the stop cancels
  jobs after the shutdown timeout, 120 seconds on xyz, and a switch waits for a
  restart job that is still stopping a runner, because it starts the active
  targets.
- **A pause in Forgejo.** Gitea skips a disabled runner on the server side
  (go-gitea/gitea#36776), and Forgejo would need no runner patch for the same
  thing. Forgejo has no such state up to 17.0.1, and its API can only read and
  delete a runner. The patch can go once it does.
- **`restartIfChanged = true` on the units.** A switch would restart a
  runner with a running job, which the runner then cancels after its
  shutdown timeout (two minutes on xyz).
- **Infer drift without a record**, from start times against the last
  switch or from switch logs in the journal. A unit started before the last
  switch may still run the current definition, and the journal is not
  readable by every system user.
- **Read the hold's markers.** They are root-only to protect the start lock;
  the hold's status text, systemd's results and the store reset's state
  explain the same stops without root.

## Consequences

- One `nixfleet runners restart --all` replaces the hand-run restarts after
  a runner change; `nixfleet status` shows what still waits for one.
- Hosts without the status command keep the name-pattern listing, which now
  includes the extra Podman runners, but show no reasons or restart flags
  and cannot restart from nixfleet.
- After the change that adds the start records is deployed, every running
  tracked unit is flagged (`no start record`), correctly: its definition
  gained the recorder. The first restart run clears them.
- The status format is a contract between two repositories: a change that
  breaks it bumps its version, and nixfleet must learn the new version
  before hosts print it.
- A paused runner takes no new jobs until its running ones finish, up to an
  hour. Jobs queue rather than fail, on the other hosts' runners when they have
  room.
- The patch has to be carried through runner upgrades.
- A runner must already run the patched build to be restarted this way. The
  first time, or on a host still on an older build, the unit fails and says so,
  and a plain `systemctl restart` of its units is the way through: it stops
  polling and finishes jobs up to the shutdown timeout.
