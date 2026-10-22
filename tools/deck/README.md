# deck

Deck also serves operator-authored interactive briefings in read-only page
collections. The root index links to the thread dashboard and each collection.
See [ADR-0013](../../docs/adr/0013-deck-page-collections.md) for content ownership.

A live web dashboard of T3 Code threads: which ones wait on you, which have
stalled, which are ready to settle, and which settled threads may not be
done. See [ADR-0012](../../docs/adr/0012-deck-t3-thread-dashboard.md).

deck holds no queries against T3's database. It runs five adapter commands,
which the consumer installs, and parses their JSON:

| Command | Used for |
| --- | --- |
| `t3-thread-inventory --days N --db PATH` | the threads of the last N days |
| `t3-thread-status --db PATH ID...` | each thread's verdict, Settle line and idle time |
| `t3-thread-messages --db PATH --head N --tail N --max-chars N ID` | a thread's message revision, and its first and latest messages for a summary |
| `t3-thread-overview-group REPORT.json STATUS.json` | the overview's sections and counts |
| `t3-thread-overview-ack ID@SETTLED_AT` and `--forget ID` | accepting a settled thread as done, and undoing it |

Every 60 seconds (and when the page is opened on facts older than 15
seconds) deck refreshes each instance's facts. A failing or slow command never
blocks the page: it shows the last good facts, their time and the error.
The page's **Refresh now** button requests an immediate facts refresh for every
instance without waiting for the commands. Once each refresh finishes, every
thread that needs a summary is queued first, including ones found by the
refresh and ones whose cached summaries are current. If a facts refresh fails,
its last good facts supply the threads. Pending per-row role requests are
preserved when Refresh now queues the default role.

Each thread that needs one gets a summary, cached in
`$XDG_STATE_HOME/deck/threads/summaries.json` against the thread's message
revision, bucket and settle time. A thread is summarised again only when one
of those changes and it has been quiet for `quietMinutes` (0 for no wait; it
need not wait once settled), by one model call through `summaryRole`
(default `light`) from `$XDG_CONFIG_HOME/llm/config.toml` (the format devlog
reads; only CLI transports are used), trying its backups in turn on failure. A failed
command or model call is retried after 15 minutes, doubling up to six hours;
invalid model output waits for the thread to change.
A worker runs `refreshConcurrency` calls at a time within `callsPerHour`.
Beside each thread's **Refresh summary** button, a role picker offers usable
roles from `summaryRoles` (default `light`, `standard`, `deep`). The selected
role applies to that refresh only; automatic summaries after a thread changes
use `summaryRole`. Refresh summary and Refresh now bypass the quiet period
and retry backoff, while still waiting for concurrency and call budget.
Settled threads the facts call done or left for you, and accepted ones, need
no summary.

Each row shows `summary by MODEL (ROLE)`. The version 1 JSON summary cache
adds an optional `role` field beside `model`; entries without `role` are read
as the configured default role. A failed refresh keeps the previous summary's
role and model.

Summaries run only on a client that can run with no tools at all, which is
Claude Code; Codex entries of the role are skipped (logged at start), since
`codex exec` cannot turn off all of its tools, and so are `api` entries.
Roles with no usable entry are omitted from the picker and logged at startup.
If the default role is unavailable, automatic summaries are disabled but
facts and refreshes with other usable roles still work. Codex and API
transports remain deferred per [#617](https://git.alc.xyz/alcxyz/nix-packages/issues/617).
The call is

```sh
claude -p --model M --effort E --safe-mode --restricted --tools "" \
  --strict-mcp-config --disable-slash-commands --no-session-persistence \
  --permission-mode dontAsk --output-format text
```

in an empty directory, with only `PATH`, `HOME`, `CLAUDE_CONFIG_DIR`,
`TMPDIR` and `LANG` from deck's environment. Every command and model call
runs in its own process group, killed as a whole at its deadline.

## Configuration

`deck serve -config PATH` (default `$XDG_CONFIG_HOME/deck/config.toml`):

```toml
listen = "127.0.0.1:3775"            # default
basePath = "/threads/"               # default
publicOrigin = "https://deck.example.com"
contentDir = "/home/user/.local/share/deck/pages" # optional; default described below

[auth]
issuer = "https://id.example.com"
clientID = "deck"
group = "deck-users"

[threads]
windowDays = 3           # default
quietMinutes = 10        # default; 0 for no quiet period
refreshConcurrency = 2   # default
callsPerHour = 60        # default
summaryRole = "light"    # default role for automatic summaries and Refresh now
summaryRoles = ["light", "standard", "deep"] # default picker roles

[commands]               # each defaults to its name on PATH
inventory = "t3-thread-inventory"
status = "t3-thread-status"
messages = "t3-thread-messages"
group = "t3-thread-overview-group"
ack = "t3-thread-overview-ack"

[[threads.instance]]
name = "main"                                   # [a-z0-9-]
label = "Main"                                  # optional
db = "/home/user/.t3/userdata/statev2.sqlite"
webURL = "https://t3.example.com"
# environmentID = "..."   # optional; overrides the inventory's environmentId
```

`summaryRole` is also included in `summaryRoles` when absent. The offered
roles retain configuration order, with duplicates removed, and must resolve
to at least one usable no-tools entry of the shared LLM configuration.

Thread links are `{webURL}/{environmentId}/{threadId}`.

## Authentication

Every route except `/healthz` needs `Authorization: Bearer <ID token>`, as an
oauth2-proxy sends it with `pass_authorization_header = true`. deck checks
the token itself against the issuer's discovery document and keys: issuer,
audience (`clientID`), expiry, and `group` in the `groups` claim. It answers
401 without a valid token and 403 without the group. POST requests also need
an `Origin` header equal to `publicOrigin`.

## Page collections

Page content is runtime data, not part of Deck's public source or Nix package.
The default directory is `$XDG_DATA_HOME/deck/pages`, falling back to
`~/.local/share/deck/pages`. Set `contentDir` to an absolute path to use another
directory. A missing directory gives an empty library.

Publish an operator-reviewed bundle in this layout:

```text
pages/
  briefings/
    2026-01-15-example/
      metadata.json
      index.html
      style.css
      app.js
```

The metadata is deliberately small:

```json
{
  "title": "Example briefing",
  "date": "2026-01-15",
  "summary": "An interactive comparison and its decision inputs."
}
```

The page appears at `/briefings/2026-01-15-example/`, with an index at
`/briefings/`. Indexes escape metadata and list newer dates first. Collection
and page names use lowercase slug segments. The metadata and dotfiles are not
download routes, and there are no directory listings. Publish complete bundles
before moving them into place; no service restart is needed to add a page.

HTML uses local stylesheet and script files, for example
`<link rel="stylesheet" href="style.css">` and
`<script src="app.js" defer></script>`. The existing content security policy
does not permit inline script, inline style attributes or external assets.
Bundles are trusted application content; Deck supplies authentication, not an
untrusted-content execution sandbox. All pages and assets require the same
operator token as the thread dashboard.

The operator owns persistence and backup of the content directory. Keep private
briefings outside public repositories, images and example fixtures. An upstream
proxy that routes only `/threads/` also needs explicit routes for the root index
and the collections it should expose. Its existing audience remains unchanged.

## Development

```sh
go test ./...
nix build .#deck
```

To try the page with the fixture data, point the commands at the fake ones
in `testdata/bin` and turn the token check off, which deck allows only on a
loopback address:

```toml
# dev.toml
listen = "127.0.0.1:3775"
publicOrigin = "http://127.0.0.1:3775"
[commands]
inventory = "/path/to/tools/deck/testdata/bin/t3-thread-inventory"
status = "/path/to/tools/deck/testdata/bin/t3-thread-status"
messages = "/path/to/tools/deck/testdata/bin/t3-thread-messages"
group = "/path/to/tools/deck/testdata/bin/t3-thread-overview-group"
ack = "/path/to/tools/deck/testdata/bin/t3-thread-overview-ack"
[[threads.instance]]
name = "main"
db = "/dev/null"
webURL = "https://t3.example.com"
```

Give it its own config, state and data directories, so it neither calls the
`light` role's real models nor shares the summary cache or private page content
of a deck service running as the same user. This works on Linux; on macOS Go reads the config
from `~/Library/Application Support` whatever `XDG_CONFIG_HOME` says, so run
it there with a `HOME` that has no `llm/config.toml`:

```sh
tmp=$(mktemp -d)
XDG_CONFIG_HOME=$tmp/config XDG_STATE_HOME=$tmp/state XDG_DATA_HOME=$tmp/data \
  go run . serve -config dev.toml -dev-no-auth
# open http://127.0.0.1:3775/threads/
```
