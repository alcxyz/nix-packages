# deck

Deck also serves operator-authored interactive briefings in read-only page
collections, and optionally a live backlog view (below). The root index links
to the thread dashboard, the backlog view when configured, and each
collection.
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
of those changes, it has been quiet for `quietMinutes` (0 for no wait; it
need not wait once settled) and its last attempt, failed or not, is at least
`summaryIntervalMinutes` old (default 360, so at most four automatic attempts
a day for a thread that keeps changing; settled threads wait too), by one model call through `summaryRole`
(default `light`) from `$XDG_CONFIG_HOME/llm/config.toml` (the format devlog
reads; only CLI transports are used), trying its backups in turn on failure. A failed
command or model call is retried after 15 minutes, doubling up to six hours;
invalid model output waits for the thread to change.
A worker runs `refreshConcurrency` calls at a time within `callsPerHour`.
Beside each thread's **Refresh summary** button, a role picker offers usable
roles from `summaryRoles` (default `light`, `standard`, `deep`). The selected
role applies to that refresh only; automatic summaries after a thread changes
use `summaryRole`. Refresh summary and Refresh now bypass the quiet period,
summary interval and retry backoff (and, being attempts, restart the interval), while still waiting for concurrency and call budget.
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
summaryIntervalMinutes = 360 # default; least time between automatic attempts at a changed thread
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

## Backlog view

With a `[backlog]` table, deck also serves a live view of the backlog
overview at `backlog.basePath` (default `/backlog/`), linked from the root
index. See [ADR-0014](../../docs/adr/0014-deck-backlog-view.md). Without the
table it registers nothing under that path, which then belongs to the page
collections like any other.

deck holds no forge credentials. It runs one adapter command, with no
arguments, that prints the backlog inventory JSON (nix-config ADR-0093) for
the consumer's chosen scope and credentials, every `intervalMinutes` under a
`timeoutMinutes` deadline, and when **Refresh now** is pressed. A failed or
slow run keeps the last good facts on the page with their time and the
error.

The page lists the overview skill's shortlist first: up to 24 issues that may
be done (in the close lane, or flagged `untouched-since-merge` or
`related-closed` and not in the `inflight` lane), then up to 12
other stale issues, idlest first. Pull requests follow by lane, then every
other issue, collapsed. Suggested actions are shown, never performed.

With an item command, each shortlisted issue also gets a verdict, as the
skill's readers give: `close`, `dispatch`, `refresh`, `keep`, `drop` or
`unclear`, a one-line reason and the issue's completion criteria. deck runs
`ITEM --max-chars 2000 REF [PR_REF...]` for the issue and its three newest
merged pull requests (each reference checked and passed as its own argument),
then makes one model call with no tools through `verdictRole`, at
`verdictEffort`. The prompt holds deck's facts, then the issue's text as
untrusted data. Verdicts are cached in
`$XDG_STATE_HOME/deck/backlog/verdicts.json` against the issue's `updatedAt`
and merged pull requests. A changed issue is judged again once its last
attempt is `verdictIntervalMinutes` old; failures back off as summaries do.
Automatic verdicts run one at a time and share `threads.callsPerHour` with the
summaries, starting only while more than a quarter of it is left. **Refresh
verdict**, with the role picker of `threads.summaryRoles`, and **Refresh
now**, which queues every shortlisted issue without a current verdict, skip
the reserve, the interval and the backoff, not the budget.

```toml
[backlog]
basePath = "/backlog/"   # default; must not overlap basePath
intervalMinutes = 15     # default
timeoutMinutes = 5       # default
verdictRole = "light"    # default; one of threads.summaryRoles
verdictEffort = "low"    # default; "" keeps the role's own effort
verdictIntervalMinutes = 360 # default; least time between automatic attempts at a changed issue

[backlog.commands]
inventory = "deck-backlog-inventory" # required: deck has no default
item = "deck-backlog-item"           # optional; without it the view shows facts only
```

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
Bundles may also use Deck's shared colour scheme toggle, its only assets a
bundle takes from outside its own directory. Load both in `<head>`, the
stylesheet before the bundle's own and the script without `defer`, so the
choice applies before the first paint:

```html
<link rel="stylesheet" href="/static/theme.css">
<link rel="stylesheet" href="style.css">
<script src="/static/theme.js"></script>
```

The script draws a light, dark and system toggle in the bottom right corner,
or inside an element marked `data-deck-theme` if the page has one. It keeps
the choice per browser, shared by Deck's views, its library and every bundle,
and without a choice follows the system. It sets `data-theme="light"` or
`"dark"` on `<html>`; style the light scheme as the default and put dark rules
under the attribute in screen media only, so printing and PDF export stay
light:

```css
@media screen {
  :root[data-theme="dark"] { --bg: #0b1020; --ink: #e6e9ef; }
  :root[data-theme="dark"] .callout { background: #2a1c16; }
}
```

Do not use `prefers-color-scheme` in a bundle: the toggle decides. Without
the script a page shows its light scheme. `/static/` is reserved: no
collection can use that name.

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
