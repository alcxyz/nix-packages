# ADR-0007: Continuous grouped provider updates

**Status:** Accepted; scan cadence amended 2026-09-30
**Date:** 2026-09-30
**Applies to:** AI provider update and merge workflows

## Context

Stable AI provider releases can arrive several times per day. A daily scan
leaves consumers behind even when builds are healthy. Separate Claude, Codex
CLI, and Codex app-server PRs also invalidate each other's T3 wrapper checks
as each update changes the integration branch.

## Decision

Check the stable Claude and Codex release sources every hour. Perform a
lightweight HTTP and version comparison before installing Nix or downloading
package assets. Compare the release tuple with both the installed package pins
and the immutable head of any open provider update PR. Identical candidates
must not refresh the branch or rerun builds. Failed requests, malformed release
metadata, prereleases, and version regressions must fail loudly.

Publish provider changes together on `update/ai-tools`. Serialize discovery and
publication for that branch. Reuse the existing individual updater validation
and ordinary PR checks, including both T3 variants and provider contracts.
A newer release may supersede an outstanding candidate; the existing stale-head
watcher stops obsolete PR builds. Unchanged failed candidates remain visible
for diagnosis or explicit CI retry rather than endlessly regenerating them.

Keep the existing non-waiting merge queue hourly, at half past the hour after
the provider scan at the top of the hour. Preserve required
checks, current-base validation, exact-head merges, and the consumer promotion
and idle-session activation gates. Polling intervals are detection opportunities,
not promises of deployment latency when runners, builds, or active sessions delay
progress. Failed or stale-base publication is retried from a fresh integration
branch on the next scan.

Keep other package scans daily. T3 has a separate six-hourly source scan under
ADR-0005, with a lightweight probe before Nix setup. Provider releases do not
require discovering a new T3 source revision, although their PR checks still
build and verify the pinned T3 wrappers.

## Alternatives

- **Poll all packages more often:** adds unrelated scanning and frequent T3
  source churn without helping stable provider delivery.
- **Keep one PR per provider:** repeats costly reverse-dependency checks after
  sequential merges and rebases. A grouped candidate verifies one combination.
- **Update a pending branch on every tick:** wastes successful work and can
  prevent a candidate from finishing. Compare immutable candidate versions first.
- **Release webhooks:** upstream projects do not publish into this repository's
  workflow directly. Bounded polling works across npm and GitHub without a new
  webhook receiver.

## Consequences

Provider changes share one validation and merge lifecycle. A broken provider
candidate blocks that combined update until fixed or superseded, while the
last verified deployed packages remain available. Package-level updaters stay
usable independently for diagnosis. Detection becomes more frequent without
weakening release channels or build and activation gates.

## Amendment (2026-09-30)

Hourly provider scans exhausted GitHub's anonymous API limit without an
authenticated upstream token. Since #442 provider scans run every six hours,
and the T3 source scan workflow matches the six-hourly cadence ADR-0005
already recorded; the merge queue stays hourly. Revisit hourly provider scans
once `UPSTREAM_GITHUB_TOKEN` exists (#443).
