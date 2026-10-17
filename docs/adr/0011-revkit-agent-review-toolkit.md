# ADR-0011: revkit, an agent review toolkit with isolated reviewers

**Status:** Accepted
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
concrete types and explicit control flow (as [ADR-0008](0008-nix-deploy-in-go.md)).
Package `review` is the shared core; each command is a subcommand. The first
is `revkit repo-review`, read-only, which writes a local report and files
nothing. Fingerprints, filing, the queue and the port of the PR review
command (behind its existing contract) are later steps with their own
amendments.

### Runtime configuration

As [ADR-0006](0006-runtime-configured-nix-deploy.md): the binary ships no
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
| `stateDir` | absolute path, optional | Trees, runs and reports; default `$XDG_STATE_HOME/revkit` |
| `repositories` | list | `{ name, context, lenses }`; `context` tells reviewers what the repository is; each lens is `{ name, prompt }` and becomes one job per reviewer |

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

The environment is an allowlist: `PATH`, locale and time zone, CA bundle
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
