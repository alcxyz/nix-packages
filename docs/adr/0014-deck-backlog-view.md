# ADR-0014: deck's backlog view, a live backlog overview

**Status:** Accepted
**Date:** 2026-10-24
**Applies to:** `tools/deck/`, `tools/gh-app/`

## Context

The backlog overview skill (nix-config ADR-0093) builds a triage page of open
issues and pull requests on request: `backlog-inventory` computes the facts,
`light` readers judge the issues the facts cannot decide, and a renderer draws
a snapshot inside the thread that asked. Like the thread overview before
[ADR-0012](0012-deck-t3-thread-dashboard.md), each look costs minutes and
several agent runs, re-reads issues that did not change and cannot be kept
open. Most of the backlog is the agent account hugin's issues and pull
requests, so a live view is in effect a consolidated view of hugin's work.
Tracked in [#619](https://git.alc.xyz/alcxyz/nix-packages/issues/619), whose
decisions comment fixes the credentials, the scope and the verdict shortlist.

## Decision

deck gets a second tool at `/backlog/`, configured by an optional `[backlog]`
table. It follows ADR-0012 wherever this ADR does not say otherwise: facts
only from adapter commands, model calls with no tools, server-rendered pages,
the same token check and content security policy. It runs in the dashboard
process beside T3, which has the `light` role's CLI and hugin's credentials,
and the proxy routes its path there (ADR-0013's 2026-10-24 amendment). Like
the threads `basePath`, its path is registered and reserved from the
collections only when the `[backlog]` table configures it, so a process
without that table serves neither. The root index links to it through
configuration, not by checking local routes, so a pages-only process can link
to the dashboard process's view.

- **Two adapter commands are its only view of the forges.** deck holds no
  forge credentials and makes no forge requests itself.
  - `inventory`, run with no arguments, prints the ADR-0093 inventory JSON for
    the consumer's fixed scope. The consumer's wrapper chooses the scope, the
    credentials and the thread inventory it joins. deck runs it every
    `intervalMinutes` (15) under a `timeoutMinutes` (5) deadline, and when
    **Refresh now** is pressed; a failed or slow run keeps the last good facts
    on the page with their time and the error.
  - `item --max-chars N REF [PR_REF...]` prints the text a reader needs for
    one issue: its title, body, `updatedAt` and last five comments, and the
    title and description of each listed merged pull request (at most three,
    the newest of the row's `mergedPrs`), each text cut to N characters.
    deck checks every reference against the inventory's ref pattern
    (`OWNER/REPO#N`, or `gh:OWNER/REPO#N`), never starting with a dash, and
    passes each as its own argv entry.
- **Shortlist.** Only the skill's overview shortlist gets verdicts: up to 24
  issues in the `close` lane, or flagged `untouched-since-merge` or
  `related-closed` and not in the `inflight` lane, in inventory order, then up
  to 12 other `stale` issues, idlest first; about 36 at most. Every
  other issue, and every pull request, shows facts only. The limits are
  deck's, as the call budget is: they bound cost, not what the facts say.
- **Verdicts are cached per issue.** One model call per shortlisted issue
  returns the skill's reader answer, validated strictly:
  `{"verdict": "close|dispatch|refresh|keep|drop|unclear", "reason", "criteria"}`.
  The prompt carries deck's facts (lane, flags, idle days, checkbox progress,
  open and merged pull request refs, and the state of related items), then
  the issue's own text (title, status line, body, comments, merged pull
  request text and related titles) as JSON between markers, marked untrusted.
  Verdicts are cached in `$XDG_STATE_HOME/deck/backlog/verdicts.json` against
  their basis, the issue's `updatedAt` and its merged pull request refs: a
  pull request that names an issue can merge without touching it, and that is
  exactly what moves an issue onto the shortlist. An issue is judged again only
  when its basis changes and its last attempt, failed or not, is at least
  `verdictIntervalMinutes` old (360, as thread summaries since ADR-0012's
  amendment, so at most four automatic attempts a day at an issue that keeps
  changing); a newly shortlisted issue does not wait. Failures back off as
  summaries do (15 minutes, doubling to six hours; invalid output waits for a
  change). A verdict whose basis no longer matches is shown as outdated until
  the new one arrives, and entries are dropped once their issue has left the
  inventory and the interval has passed, so an issue that returns cannot skip
  it.
- **One budget, with a reserve for summaries.** Verdicts spend the same
  rolling `callsPerHour` budget as thread summaries and use the same no-tools
  model entries. Automatic verdicts run one at a time, through `verdictRole`
  (`light`) at `verdictEffort` (`low`, as the skill's readers run; empty keeps
  the role's own), and start only while more than a quarter of the hour's
  budget is left, so a backlog change cannot starve thread summaries. Each
  shortlisted row has **Refresh verdict** with the role picker of
  `threads.summaryRoles`, and **Refresh now** re-reads the facts and queues
  every shortlisted issue without a current verdict through `verdictRole`.
  Both skip the reserve, the interval and the backoff, not the budget, and
  restart the interval, as the thread buttons do.
- **Read-only.** Rows link to the forge. Suggested actions (close, drop,
  dispatch, refresh the status, merge a ready pull request, review someone
  else's) are shown from verdicts and lanes, never performed: the only POST
  routes ask deck for a facts refresh or a verdict.
- **What stays in the skill.** Thorough mode (branch lane, `standard`
  checkers, status refresh), focus scopes and groups, the "next up" picks and
  every write.

### Credentials and scope

The view reads the forges as hugin, so it shows what hugin can see. The
consumer's wrapper, not deck, provides:

- **Forgejo:** a dedicated read-only token of hugin, separate from its working
  token, with `read:issue`, `read:repository` and `read:user` (the inventory
  asks `/user` who it reads as), passed through `FORGEJO_API_TOKEN_FILE`.
- **GitHub:** installation tokens of hugin's App (nix-config ADR-0099),
  requested with read-only permissions: metadata, issues, pull requests,
  checks and commit statuses, all read. These are within the App's grant, so
  the App's settings do not change. `gh-app` gains a `read-only` command that
  mints such a token from the App key, caches it apart from the full token, and
  runs a command with it in `GH_TOKEN` and an empty gh configuration
  directory, so the operator's own `gh` login is never used; if minting fails
  the command does not run. GitHub still lets any installation token open new
  issues in the public repositories it can see, whatever its permissions
  (checked when this was written); the token can write nothing else, and the
  commands never write.
- **Scope:** the personal scope (all) with no groups: the wrapper passes an
  empty scope file, so work groups stay out of this instance. No branches.
- **Inventory changes:** an installation token cannot ask GitHub who it is,
  or search `--author @me`, so `backlog-inventory` takes the GitHub account
  (`alcxyz`, the App's owner, whose repositories Forgejo mirrors) as an
  argument and skips the upstream search. Pull requests the App opened
  (`app/alcxyz-hugin`) count as hugin's own rather than as a bot's.
- **Accepted gaps:** open pull requests in repositories outside the App's
  installation (upstream contributions), private related items outside it
  (shown as unavailable) and Forgejo repositories hugin cannot list. The
  first deployment lists them.

## Alternatives Considered

- **Readers with tools, as in the skill.** Rejected: deck's model calls run
  with no tools (ADR-0012), so a reader cannot fetch an issue; the `item`
  command supplies the same text as data.
- **Batches of six to eight issues per call, as the skill's readers.** Fewer
  calls, but one invalid answer loses the batch, and a batch would be re-read
  whenever any of its issues changed. One call per issue matches the
  per-issue cache.
- **A federated credential instead of a token.** Forgejo accepts only the
  tokens it issues and cannot exchange an outside identity for one, and
  GitHub's OIDC runs only from its own Actions outward. On GitHub the App's
  one-hour installation tokens are the closest; on Forgejo a stored token is
  the only way, so it is kept read-only and separate.
- **The operator's credentials.** Rejected: they reach far more than the view
  needs, and the backlog is hugin's work.
- **A separate read-only GitHub App.** Its key could not mint write tokens,
  but it is another App and key to own, and deck runs as the user that already
  holds hugin's App key. Requesting read-only installation tokens keeps one
  identity.
- **Serve the skill's rendered page.** Rejected: it is a snapshot with inline
  script, which the content security policy refuses, and it cannot refresh a
  verdict or show what changed.
- **Compute the shortlist in the adapter.** The lanes and flags are facts and
  stay there; how many issues get a model call is a cost limit like
  `callsPerHour`, so deck owns it.

## Consequences

- Opening the view costs no agent run. Filling the shortlist costs about 36
  calls, spread over the budget; after that, calls follow changes to
  shortlisted issues.
- A changed issue can keep an outdated verdict for up to six hours, as a
  thread can keep its summary; Refresh verdict and Refresh now get one
  sooner. Without the interval an issue that an agent updates on every step
  would be judged on every inventory run, as the thread that woke every 15
  minutes was summarised before ADR-0012's amendment. At most about 140
  automatic calls a day, if every shortlisted issue kept changing.
- The page lags the forges by up to `intervalMinutes`. One fleet-wide run makes
  a few hundred API requests, well within an installation token's hourly
  limit at four runs an hour.
- deck depends on the inventory's JSON contract (ADR-0093) and the `item`
  command's; the tests use fixtures and fake commands in `testdata/` that
  follow them.
- The read-only Forgejo token is one more credential for the operator to
  create, rotate and revoke (nix-secrets #294). The deck service can read
  hugin's App key, which can mint write tokens; that is true of the user it
  runs as already, and the wrapper only ever requests read permissions.
- Once the view covers routine checks, the skill can send them here, as the
  thread overview skill does for threads; that is a separate change.
