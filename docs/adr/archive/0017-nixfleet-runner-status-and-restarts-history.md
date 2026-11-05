**History of** [ADR-0017](../0017-nixfleet-runner-status-and-restarts.md): text replaced on 2026-11-05.

Replaced because waiting for an idle host starved on a busy one: xev's restart
gave up as busy three times in a row while its runners kept taking jobs.

## Date

**Date:** 2026-11-02

## Decision

- **`forgejo-runner-restart.service`**, a root oneshot, restarts the flagged
  units in one `systemctl restart` once the whole host is idle: no Podman
  store reset runs, no runner is starting or stopping, no job container runs
  on a running engine, and no runner took a task within the last 5 minutes.
  The whole host, because restarting the resource policy or an API restarts
  the runners that require it. While busy it waits, up to 60 minutes; then
  it gives up and nothing is restarted, so it never cancels a job on
  purpose. A job taken between the last check and the stop gets the runner's
  graceful stop. Its progress and result (`waiting`, `restarting`, `done`,
  `nothing`, `busy`, `failed`) go to the world-readable
  `/run/nixfleet-runners/restart`, which the status command reports. It
  leaves the runner hold as it is: a restarted runner whose gate refuses the
  start is the hold's to start later.

## Consequences

- A busy host may need several runs, or a quieter time, before its restart
  goes through; the restart never trades a job for it.

## Text replaced later on 2026-11-05

- `nixfleet runners restart [--all | <host>...] [--dry-run] [--yes]` plans
  first (what each host would restart and why, or why it is skipped), asks
  on a terminal or needs `--yes`, then runs `systemctl start` of each
  host's restart unit concurrently, over SSH as the system user with
  `sudo -n` where the inventory says so, prints its progress, and reports
  each host's outcome.
