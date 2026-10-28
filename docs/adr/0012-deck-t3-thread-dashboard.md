# ADR-0012: deck, a live T3 thread dashboard

**Status:** Accepted
**Date:** 2026-10-20, updated 2026-10-24 (summary interval, per #642)
**Applies to:** `tools/deck/`

## Context

The user runs many T3 Code threads at once and needs to see which wait on
them, which have stalled, which are ready to settle, and which settled
threads may not really be done. Until now an agent skill built that overview
on request: it read thread facts from T3's state database, ran several reader
agents to summarise each thread, and drew HTML frames inside the thread that
asked for them.

That works but costs minutes and several agent runs per look, and re-reads
threads that did not change. The frames are snapshots: they cannot open a
thread, accept a settled thread as done, or show what changed since the last
look. The facts are cheap; only the summaries are expensive.

The consumer configuration already has commands that own every query against
T3's database (`t3-thread-inventory`, `t3-thread-status`) and the overview's
grouping rules, and adds `t3-thread-messages` (a thread's first and latest
messages and a revision that changes with any finished message) and
`t3-thread-overview-group` (the grouping as JSON). Tracked in
nix-packages #595.

## Decision

`deck` is a Go web server in `tools/deck`, run as a user service on the host
that runs T3 and reached through an authenticating proxy.

- **The adapter commands are deck's only view of T3.** deck runs the five
  commands, with paths from its configuration, and parses their JSON; it has
  no SQL of its own, so a T3 schema change is fixed in the consumer's
  commands and their contract tests. Arguments are passed as separate argv
  entries, never through a shell, and IDs are checked against the commands'
  own patterns first. Each command runs in its own process group under a
  deadline, and the whole group is killed when it expires, so a child that
  keeps the output open cannot hold deck.
- **Facts are refreshed on a timer and served from memory.** Every 60
  seconds, and when the page is opened on facts older than 15 seconds, deck
  runs inventory, status and the grouping per configured instance, and
  publishes the three together only when all succeeded. A failed or slow
  command keeps the last good facts on the page, with their time and the
  error. Refreshes and the regroups that follow new summaries are serialised
  per instance, so an older grouping never replaces a newer one. Checking
  threads for new messages runs beside the facts loop, so a slow messages
  command delays summaries, never facts.
- **Summaries are cached per thread and made incrementally.** Each summary
  records what it was made from: the thread's message revision, its bucket
  and its settle time. A thread gets a new one only when one of those
  changes (so a thread that settles without new messages gets a settled
  summary with a verdict), it has been quiet for a while (or is settled),
  its last attempt, failed or not, is at least the summary interval old (six
  hours by default, so at most four automatic attempts a day at a thread that
  keeps changing; retries of an unchanged thread follow the backoff below),
  and its facts do not
  already settle it (settled and done, or accepted). A
  worker makes one model call per thread through the shared `light` role of
  the LLM configuration devlog uses (ADR-0002), over the CLI transport,
  trying the role's backups in turn, under a concurrency cap and an hourly
  call budget. A queued thread that turns active again waits for the quiet
  period before its call. A failed command or model call is retried after
  15 minutes, doubling up to six hours; invalid output is not retried until
  the thread changes or the user asks for a refresh. A cached verdict made for
  another settlement than the thread's current one is withheld from the
  grouping, so it cannot mark a reopened thread done. Model output
  is validated strictly against the summary schema; invalid output keeps the
  previous summary. The page never waits for a model and marks summaries
  older than the thread's latest activity.
- **The model sees thread text as data.** The prompt carries deck's own
  facts, then everything written in the thread (its title, Settle line,
  waiting-on notes and messages) as JSON between markers, says that text is
  untrusted, and asks for one JSON object. A summary runs only on a client that can run with no
  tools at all: Claude Code with `--tools ""`, in safe and restricted mode,
  without MCP servers, hooks or a saved session, in an empty directory.
  `codex exec` keeps tools (its patch and image tools among them) even with
  its shell features off, and a read-only sandbox still lets it read files,
  so Codex entries of the role are skipped for summaries, with a log line
  saying so; so are API-only entries, since deck has no API client. The client gets a minimal environment (`PATH`, `HOME`, its own
  configuration directory, `TMPDIR`, `LANG`), never the service's.
- **deck checks the proxy's token itself.** Every route but the health check
  needs the OIDC ID token the proxy forwards: issuer, audience, expiry and a
  required group are verified against the issuer's published keys, so the
  proxy is not the only check. POST requests, the only ones that change
  anything, also need an `Origin` equal to the configured public origin. A
  development flag turns the token check off, and only on a loopback listen
  address.
- **The page is server-rendered.** Go templates escape all thread text; a
  small embedded script filters, sorts, highlights what is new since the
  last visit and refreshes the page data every minute by fetching a
  server-rendered fragment. The content security policy allows only the
  page's own script and style. No build step and no external assets.

## Alternatives Considered

- **Keep the in-thread frames** (the existing overview skill). Rejected: every
  look costs minutes and several agent runs, summaries are recomputed for
  unchanged threads, and the frames cannot act or link into T3.
- **A cluster-hosted app fed by snapshots pushed from the T3 host.** Rejected:
  it needs a push job, a store and a second deployment, its data is as old as
  the last push, and acknowledging a thread would need a path back to the
  host. Running beside T3 reads current facts directly through the existing
  commands.
- **Build the dashboard into a T3 fork.** Rejected: T3 is packaged from
  upstream builds, and carrying a UI feature in a fork means rebasing it on
  every release. The overview's rules also live in the consumer's commands,
  which a fork would have to duplicate.
- **Give deck its own SQL against T3's database.** Rejected: two places would
  have to follow T3's schema, and the commands already carry the schema
  checks and contract tests.
- **Trust the proxy alone.** Rejected: anything that reaches the listen
  address, or a proxy misconfiguration, would expose thread content and the
  acknowledge action. Verifying the forwarded token is cheap.

## Consequences

- Opening the dashboard costs no agent run; model calls happen only for
  threads with new activity, within a budget the configuration sets.
- deck depends on the adapter commands' JSON contracts. A change there
  needs a matching change here; the tests use fixtures and fake commands in
  `testdata/` that follow the contracts.
- Summaries can lag behind a busy thread by the quiet period, and longer when
  the budget is used up; the page shows when a summary predates the latest
  activity.
- The summary interval was added after a day of service logs: a thread that
  wakes on a schedule every 15 minutes outlasted the quiet period each time
  and took about 80 of 124 calls. With the interval a changed or newly
  settled thread can keep an outdated summary, or show no verdict, for up to
  six hours; Refresh summary and Refresh now are the way to get one sooner.
- A role with no Claude CLI entry leaves summaries off; the page says so
  and the facts still work. Summaries for such a role would need an API
  transport, which deck does not have yet.
- Accepting a settled thread as done names the settlement the page showed;
  if the thread settled again since, deck refuses with 409 and the user
  looks again.
- The OIDC issuer must be reachable from the host for discovery and key
  refresh; while it is not, requests fail with 503 rather than falling back
  to no check.
- Its Go dependencies are fetched by a pinned `vendorHash`, not committed to
  `vendor/` as devlog's are: the OIDC and JOSE modules are about 470 kB of
  third-party code that would dominate every review diff. A dependency change
  therefore needs a new hash.
