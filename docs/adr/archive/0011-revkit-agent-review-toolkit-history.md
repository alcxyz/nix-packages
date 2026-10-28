**History of** [ADR-0011](../0011-revkit-agent-review-toolkit.md): text replaced on 2026-10-28.

# ADR-0011: revkit, an agent review toolkit with isolated reviewers

**Status:** Accepted; findings store, matching, resume and filing added 2026-10-17
**Date:** 2026-10-17
**Applies to:** `tools/revkit/`

## Context

Automated pull request reviews run two read-only reviewers, one per client
(Claude Code and Codex), on a PR's diff. A diff cannot show problems that
build up across many changes that each looked fine: decisions and documents
drifting from the code, mechanisms whose premise no longer holds, dead or
duplicated code. A review of a whole repository at rest covers those (#557).

A hand-run pilot with three lenses (decision drift, stale mechanisms,
documentation drift) on one consumer repository found 12 distinct problems,
none rejected by a cross-model check, and showed four things this design
answers:

- The two clients overlap little and miss different things, so both find
  and each checks the other's findings.
- Codex loads `AGENTS.md`, skills and memories from its configuration
  directory even with `--ignore-user-config`, and in two of three jobs it
  followed those user instructions to material outside the reviewed tree.
  Findings that may be filed publicly must come from reviewers that see only
  the tree.
- Claude's reported input excludes cache reads, which were millions of
  tokens per job, so any usage or budget figure must add them.
- A reviewer that fails can still reply as if it found nothing. In the first
  run of this tool, a system-managed client hook blocked every command, and
  the reviewer answered with an empty findings list.

The same reviewer runner, tree checkout and usage counting will later serve
the PR review command, a spare-capacity job queue and filing, so they belong
in one program rather than in another script.

## Decision

`revkit` is a Go program in `tools/revkit`, standard library only, with
concrete types and explicit control flow (as [ADR-0008](../0008-nix-deploy-in-go.md)).
Package `review` is the shared core; each command is a subcommand. The first
is `revkit repo-review`, read-only, which writes a local report and files
nothing. Fingerprints, filing, the queue and the port of the PR review
command (behind its existing contract) are later steps with their own
amendments.

### Runtime configuration

As [ADR-0006](../0006-runtime-configured-nix-deploy.md): the binary ships no
reviewers, repositories or lenses. It reads a versioned JSON document from
`--config FILE` or `REVKIT_CONFIG`, which the consumer generates (resolving
its model roles to models, and merging any private lenses from its private
configuration). Unknown fields are rejected. Version 1:

| Field | Type | Meaning |
| --- | --- | --- |
| `schemaVersion` | integer | Must be `1` |
| `reviewers` | non-empty list | `{ name, client, model, effort }`; `client` is `claude` or `codex`; names are unique, `[A-Za-z0-9_-]+` |
| `timeoutSeconds` | positive integer | Deadline of each reviewer run |
| `environment` | string array, optional | Further variable names reviewers may see (see below) |
| `homeLinks` | string array, optional | Entries of the user's home linked into each Codex reviewer's home (see below) |
| `readablePaths` | string array, optional | Absolute paths a Codex reviewer may read besides the tree and the platform minimum, for example where its tools live |
| `stateDir` | absolute path, optional | Trees, runs and reports; default `$XDG_STATE_HOME/revkit`; with a Codex reviewer never under `/tmp`, which its profile denies (a deny beats the grant for the tree) and where Codex refuses a home |
| `matcher` | reviewer, optional | Groups findings that describe the same problem (amendment of 2026-10-17); without one, findings match by lens and title |
| `repositories` | list | `{ name, context, lenses, issues }`; `context` tells reviewers what the repository is; each lens is `{ name, prompt }` and becomes one job per reviewer; `issues`, optional, is the tracker findings are filed in: `{ url, owner, repo, tokenFile, limit }` (https only; `limit` per command, default 5) |

The finding and verdict formats, and the instructions that go with them, are
part of the program, since its parser depends on them.

### Reviewers

The reviewed commit is written from raw git objects (`git ls-tree`, `git
cat-file`, hooks disabled), so no filter, hook or attribute runs; symbolic
links become text files and submodules are skipped. Each reviewer runs in
that tree, read-only, without network, web search, MCP servers, plugins or
app connectors, and without the user's instructions, skills, memories or
settings. How that is achieved differs by client, because the clients find
their configuration and save their logins differently (verified on Claude
Code 2.1.301 and Codex 0.163.0, here and in the PR review fix of the same
problem):

- **Codex** reads `AGENTS.md`, configuration, skills and memories from
  `CODEX_HOME` even with `--ignore-user-config`, and skills and plugins from
  `~/.agents`. Each Codex run therefore gets a temporary home: `HOME`,
  `CODEX_HOME`, the XDG base directories and `TMPDIR` point into it, and it
  holds one symbolic link to the user's `auth.json` (with file storage
  pinned) and the `homeLinks` entries, nothing else. Codex rewrites
  `auth.json` in place, which follows the link, so a refreshed login reaches
  the user's file; if a later Codex replaced the link instead, the run says
  so. The home is removed after the run. Codex runs under a permission
  profile that denies the whole file system except the platform minimum,
  the tree and `readablePaths`, with `/tmp`, its temporary directory and
  login shells denied, in place of the read-only sandbox that let commands
  read the whole disk. `PATH` entries are resolved through symbolic links,
  since profile directories in the denied home would otherwise vanish. Web
  search is disabled, and the commands Codex runs do not inherit its sign-in
  variables, so a prompt in the tree cannot have a key printed into a
  finding.
- **Claude Code** keeps the user's home and configuration directory.
  `--restricted` skips the user's `CLAUDE.md` and settings and keeps the
  file tools (only `Read,Grep,Glob`) inside the tree; `--safe-mode` also
  turns off skills, plugins and agents; `--strict-mcp-config` and no session
  persistence complete it. A separate configuration directory would break
  its login: Claude saves a refreshed login by renaming a new file over
  `.credentials.json`, which would replace a link and leave the user's file
  with a refresh token already used, and on macOS the login is in the
  keychain under the configuration directory.

Asked to name their instruction sources, Codex with the user's home lists
the user's global instructions, and neither client lists any user
instructions, skills or memories under these settings. In a smoke run the
Codex reviewer could not read a file outside the tree or list `/tmp`, saw
only its own empty home, and still ran `rg` and `git`.

`homeLinks` exists for system-managed client hooks, which apply whatever
the configuration directory is, and may run tools they find in the user's
home (for example a Nix profile); hooks run outside the sandbox. Names are
single entries of the home; the client configuration directories,
`.agents`, `.config`, `.local`, `.cache` and the reviewer's own `tmp` are
refused.

The environment is an allowlist: the absolute entries of `PATH` (an empty or
relative one would look commands up in the reviewed tree), locale and time
zone, CA bundle
and proxy variables, each client's own sign-in variables
(`ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` for Claude,
`OPENAI_API_KEY` and `CODEX_API_KEY` for Codex), Claude's `HOME` and
`CLAUDE_CONFIG_DIR`, and the configured `environment` names. Forge tokens,
session sockets and everything else stay out. A consumer adds what its
platform's shells need; on NixOS, for example, `__NIXOS_SET_ENVIRONMENT_DONE`
keeps the shell Codex runs commands in from resetting `PATH` to directories
in the reviewer's home. Each run is a process group, killed at its deadline,
on interruption and when the client exits.

### Results

A failed reviewer is a missing review, never a review without findings. A
job counts only when the client exits 0 with a reply, the reply is the
asked-for JSON without an `error` (a findings list whose every entry is
malformed is not), and the reviewer made at least one successful tool call:
a Codex command that exited 0, or a Claude tool call that did not fail. That
last check rules out a reviewer whose tools were all blocked, the failure
seen in practice; it does not prove the reviewer read what it needed. A
verification counts only when every finding it was given got a verdict.
Everything else is `failed`, `timeout`, `interrupted` or `unparsed`.
`repo-review` exits 0 when every job and verification completed, 1 when any
is missing, and 2 for usage and configuration errors.

Each reviewer's findings are verified by the next configured reviewer, so
each model checks another's work; with one reviewer they stay unverified.
Each run gets a directory of its own under the state directory, holding
every prompt, client output and log, `results.json` and `report.md`; job
files are named with dots, which names cannot contain, so no two jobs share
one. The tree is kept per commit and reused.

Every run records the client's native token counters, unknown counters as
null, never zero, and a total input that includes cache reads and writes for
Claude (Codex's input already includes its cache reads). It also records the
usage windows each client reported: Claude's from the `rate_limit_event` of
its stream, Codex's from the session log in the reviewer's home (which is
why Codex runs without `--ephemeral`). These shapes are undocumented client
output; anything missing or unexpected means "unknown", which a later
budget policy must treat as "do not run".

## Alternatives Considered

- **Keep extending the pilot script or the PR review script** — rejected for
  the reasons of ADR-0008: run state, concurrency and deadlines are easier to
  get right and test in Go, and the PR review script is tied to one
  consumer's configuration.
- **Rely on the clients' flags to skip user configuration** — rejected for
  Codex: the pilot showed it loading the user's instructions with
  `--ignore-user-config`. Enough for Claude, whose restricted mode was
  verified to skip them.
- **A temporary configuration directory for Claude too, with its login
  linked or copied in** — rejected: a refresh replaces a link, a copy
  diverges, and either can leave the user signed out; on macOS the login is
  not a file. (The first version of this tool did this and moved a
  refreshed login back over the user's file; keeping Claude's own directory
  removes the need.)
- **A long-lived token for reviewers** (for example from `claude
  setup-token`, passed through a file descriptor) — possible later; it needs
  a provisioned token per client and is not needed for the logins the user
  already has.
- **Strip known token variables instead of an allowlist** — rejected: the
  clients and their sessions add variables over time, and an allowlist does
  not need to know them.

## Consequences

- Reviewers see the reviewed tree and the clients' built-in instructions
  only, so their findings can be filed in the reviewed repository without
  carrying private context. What stays readable is the platform minimum and
  `readablePaths`; a consumer that lists a store of built configuration
  should know a reviewer that goes looking can read it.
- Each Codex run starts without its caches or history, which costs a few
  seconds and a fresh client state per run.
- A consumer whose managed hooks need tools from the user's home names them
  in `homeLinks`, or makes the hooks find them without `HOME`; one whose
  shells reset `PATH` passes the variable that stops them.
- A run's usage and windows are known only after a call; the queue reads
  them from the runs it makes, as #557 planned.
- Changing the finding or verdict format changes the program and its tests,
  not only the configuration.

## Amendment (2026-10-17): findings store, matching and resume

Step 3 of #557. A run's findings now outlive its report.

- **Store.** Each repository has a findings store
  (`repo-review/<repository>/findings.json` under the state directory) of
  distinct problems with stable IDs (`F1`, `F2`, ...): status (`open`,
  `dismissed`, `filed`), text and verdict, the lenses, paths and reviewers
  that reported it, and its first and last sighting. Within one review the
  best-verified report describes the problem (any verification beats none);
  a later review's report replaces it, and an earlier review, resumed, only
  replaces an unverified one. Reviews are named after their first run, whose
  name starts with its UTC start time to the nanosecond, so names order
  them. Sightings count reviews. Updates hold a file lock, so concurrent runs and commands do
  not lose each other's changes, and a run's findings get their IDs only
  once the store is saved: a failed save leaves them without IDs and fails
  the run.
- **Matching by meaning.** The pilot's duplicates cited different lines for
  the same problem, so path and line fingerprints do not work. After
  verification, the configured `matcher` (one reviewer, in the same sandbox
  and home as the others; a light model suffices) groups the run's findings
  with each other and with the store's findings of the same lenses, and may
  read the cited files to decide. A reply may only point at a known ID or an
  earlier finding of the run, so it cannot form a cycle, and must cover
  every finding. Its "no match" stands for every problem it was shown.
  Findings without a match are then matched by lens and title, under the
  store's lock, against the problems the matcher did not see (all of them
  when there is no matcher or it fails), which also catches a problem
  another run added meanwhile; only the matcher's prompt is bounded (to the
  200 most recently seen findings). Each run states how it matched.
  Matching is not a review, so a failed matcher does not make a run
  incomplete.
- **What a run reports.** Each finding carries its store ID and whether the
  problem is new or already open, filed or dismissed. A dismissed or filed
  problem keeps its status when it is seen again.
- **`revkit findings`** lists the open problems (`--all` for every one),
  shows one, and dismisses (with a required reason) or reopens them.
- **Resume.** `repo-review --resume` reuses the jobs that completed in the
  newest earlier run on the same commit, matched by reviewer and a hash of
  the job's prompt, so a run interrupted by a failed client or an exhausted
  usage window does not repeat the jobs that finished. Verifications are
  reused only when they checked exactly the same findings. A resumed run
  belongs to the same review: its problems still count as new, it adds no
  sighting, and reused jobs' usage is not counted again.
- **Placement.** The matcher's links between findings of the run join them
  into groups, followed through every link. A reused finding keeps the
  problem it joined before (in a field no reviewer reply can set), whatever
  the matcher says now. A group's other findings join the first problem one
  of its reused findings keeps, else the known problem the matcher named,
  else a problem with the same lens and title that is not yet claimed, else
  a new one. Kept problems are reserved before any group is placed, and a
  problem a group joined by title is claimed, so groups the matcher told
  apart never share a problem through a title. A resumed earlier review
  does not move a problem's last sighting back.
- **Security flag.** Reviewers mark a finding `security` when it concerns
  secrets, credentials, access control or private details in a public
  repository. The store keeps the flag.

Filing, decided with the owner:

- A person picks the findings to file with `revkit findings file <ids>`;
  a run never files anything itself.
- Only open, `confirmed` findings are filed; `partly` confirmed and
  unverified ones stay in the store.
- Security findings are never filed anywhere until a private tracker and a
  hygiene lens exist; `file` refuses them.
- Issues go to the repository's configured tracker and are filed by a
  dedicated bot account with issue-write scope, not the owner's: the
  consumer names that account's token file. The token is read when filing
  starts and only sent in the Authorization header, and redirects are not
  followed, so it cannot reach another host.
- Before filing, every open issue of the tracker is listed, page by page
  until an empty page (forges may cap page sizes) and from the database
  rather than the search index (which can lag behind new issues), and an
  issue with the same full title is linked instead of filing a duplicate,
  including one filed earlier in the same command; two findings whose
  titles the cap shortens to the same text stay two issues. An issue revkit
  filed is recognised by the footer it writes (the finding's ID, a random
  ID of its store, given once, and a 96-bit hash of the full title, as a
  paragraph of its own that must end the body, so a quote of it does not
  count) and matched only by that hash, never by its possibly capped
  title; any finding with that full title links it, and the issue filed
  for the finding itself (all three match, so another store's `F1`, such
  as a renamed repository's, does not) links first, even under `--new`,
  so neither a capped title nor a retry after a lost response files it
  twice. The check
  is best effort: a finding retitled by a later review between a lost
  response and the retry, or an issue closed while the pages are read, can
  still lead to a second issue. A wrong link, such as an unrelated issue
  with the same title, is undone with `revkit findings reopen`, which
  leaves the issue alone; `file --new` then files the finding as an
  issue of its own and links no other. With more open
  issues than it can check, filing refuses. One filing command runs at a
  time per repository. It files at most the tracker's limit, stops when
  interrupted, checks each finding against the rules again just before
  creating or linking its issue (a run may have flagged it, or someone
  dismissed it), and marks it filed as soon as the issue exists. The
  store's lock is not held while the forge is called, so a change in the
  seconds a create request takes can still let that one issue be created;
  the command then leaves the finding unmarked and says to review the issue.
- The issue states the claim, evidence, paths and fix, and how it was found
  (the lens and commit of the review whose report it is, or no commit when
  an older store cannot tell, the number of reviews, the verifier's reason) without naming models. Reviewers read a
  repository that may hold text written to steer them, so their text goes
  into fenced code blocks, shown exactly as written: the forge renders
  nothing inside one and runs no mention, reference, link or short-link
  processing on it, and the fence is longer than any backtick run in the
  text. Escaping Markdown instead was tried and rejected: the forge's own
  processing of the rendered text (short links, bare URLs, references)
  still acted on it. Paths are code spans; titles, which are not Markdown,
  get a zero-width break after `@`, `#`, `!` and in `://`, are compared
  without the breaks, and are cut to 200 characters. `--dry-run` reads the
  tracker and says what would be filed or linked.

Considered and rejected:

- **Fingerprints from paths, lines or text hashes**: the same problem is
  cited at different lines by different reviewers and runs.
- **Matching inside the verifier's prompt**: it would mix two judgements in
  one reply and give the matcher only one reviewer's findings at a time.

