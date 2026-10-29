# deck

Deck also serves operator-authored interactive briefings in read-only page
collections, and optionally a live backlog view (below). The root index links
to the thread dashboard, the backlog view when configured, and each
collection. A second deck process without thread instances can serve the
collections alone, from a Git checkout, on a host that stays up (see
[Dashboard and pages-only modes](#dashboard-and-pages-only-modes)).
See [ADR-0013](../../docs/adr/0013-deck-page-collections.md) for content ownership.

A live web dashboard of T3 Code threads: which ones wait on you, which have
stalled, which are ready to settle, and which settled threads may not be
done. See [ADR-0012](../../docs/adr/0012-deck-t3-thread-dashboard.md).
A thread the grouping lists in `unwatchedChecks` (stopped while a linked open
PR's checks run, with no PR watch armed) gets a `checks unwatched` badge by
its title and a `PR N: checks pending, unwatched` link per PR, and a
`stopped, PR checks unwatched` count appears while any thread has one; the
flag is informational and does not change the grouping.

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
collections = true                   # default; false leaves the collections to a pages-only process

[auth]
issuer = "https://id.example.com"
clientID = "deck"
groups = ["admins"]           # full access; the older group = "admins" still works
readOnlyGroups = ["viewers"]  # optional; GET and HEAD only

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
arguments, that prints the backlog inventory JSON (`backlog-inventory`) for
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
audience (`clientID`), expiry, and the `groups` claim. It answers 401 without
a valid token and 403 without a configured group. Members of `groups` (or the
single `group`) get full access. Members of only `readOnlyGroups` may make GET
and HEAD requests; their other requests get 403. At least one group must be
configured. POST requests also need an `Origin` header equal to
`publicOrigin`.

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
download routes, and there are no directory listings. Assets sit beside
`index.html`, not in subdirectories. Their names are lowercase letters and
digits, with single `-` or `_` separators between them (not leading, trailing
or doubled), and one of the extensions `html`, `css`, `js`, `png`, `jpg`,
`jpeg`, `gif`, `webp`, `avif`, `svg`, `ico` or `json`; any other file returns 404.
A `json` asset lets a page load data that a process outside Deck refreshes;
`metadata.json` is still not served.
Publish complete bundles
before moving them into place; no service restart is needed to add a page.

HTML uses local stylesheet and script files, for example
`<link rel="stylesheet" href="style.css">` and
`<script src="app.js" defer></script>`. The existing content security policy
does not permit inline script, inline style attributes or external assets.
Bundles may also use Deck's shared colour scheme toggle and design
stylesheet, the only assets a bundle takes from outside its own directory.
Load them in `<head>`: the stylesheets in this order before the bundle's own,
and the script without `defer`, so the choice applies before the first paint.
Name the page's purpose on `<body>`:

```html
<link rel="stylesheet" href="/static/theme.css">
<link rel="stylesheet" href="/static/base.css">
<link rel="stylesheet" href="style.css">
<script src="/static/theme.js"></script>
...
<body data-deck="sheet">
```

The script draws a light, dark and system toggle in the bottom right corner,
or inside an element marked `data-deck-theme` if the page has one; give it a
slot where it does not cover content. It keeps the choice per browser, shared
by Deck's views, its library and every bundle, and without a choice follows
the system. It sets `data-theme="light"` or `"dark"` on `<html>`.

`base.css` is deck's design ([ADR-0015](../../docs/adr/0015-deck-design-system.md)):

- **Purpose.** `data-deck` is one of the following. It sets the page's text
  size, line height and width (`--deck-size`, `--deck-width`):
  - `live`: a dense overview;
  - `index`: a list to find and open;
  - `sheet`: a worksheet to read and decide;
  - `slides`: presentation frames;
  - `tool`: an interactive page.
- **Tokens.** `--deck-bg`, `--deck-surface`, `--deck-surface-2`, `--deck-text`,
  `--deck-text-2`, `--deck-muted`, `--deck-line` and `--deck-line-strong`
  (control borders).
  - The accent: `--deck-accent`, `--deck-on-accent`, `--deck-accent-text` and
    `--deck-accent-soft`.
  - Status, each with a `-soft` background: `--deck-attn` (waits on the
    owner), `--deck-ok` (done) and `--deck-danger` (stalled or failed).
  - `--deck-hero`, `--deck-hero-text` and `--deck-hero-muted` for panels that
    stay dark, such as a cover.
  - Chart series `--deck-c1` to `--deck-c4`, the fonts `--deck-sans` and
    `--deck-mono`, type sizes `--deck-text-xs` to `--deck-text-3xl`, spacing
    `--deck-space-1` to `--deck-space-6`, and `--deck-radius`.
  - Each token has a light and a dark value, so a bundle built on them needs
    no dark rules of its own.
- **Base elements.** Under `data-deck`: body, headings, links, code, form
  controls, `button.primary` and focus rings. They carry no specificity, so
  any rule in the bundle's own stylesheet wins.
- **Components:**
  - `deck-card` (`hero` for a dark panel);
  - `deck-chip` (`outline`);
  - `deck-badge` (`attn`, `ok`, `danger`, `accent`, `solid`, `quiet`);
  - `deck-btn` (`primary`);
  - `deck-kpi` with `deck-kpi-value`;
  - `deck-table` (`num` cells);
  - `deck-kicker`, `deck-muted`, `deck-num` and `deck-wrap`;
  - `deck-noprint` hides an element on paper.

Use the tokens rather than fixed colours, and colour only what carries
meaning. Keep text at 4.5:1 or more against its background. Where a bundle
needs its own colour in dark mode, put the rule under the attribute in screen
media only, so printing and PDF export stay light:

```css
@media screen {
  :root[data-theme="dark"] .callout { background: var(--deck-attn-soft); }
}
```

Do not use `prefers-color-scheme` in a bundle: the toggle decides. Without
the script a page shows its light scheme. Print rules stay in the bundle:
worksheets on A4 with navigation hidden, presentations one frame per page at
their frame size, tools as a list without controls.

`/static/` exists only on Deck. A standalone export or PDF build of a bundle
ships its own copy of `base.css`. A bundle that uses `base.css` needs a Deck
release that serves it, so publish it after that release is deployed.
`/static/` is reserved: no collection can use that name.

Bundles are trusted application content; Deck supplies authentication, not an
untrusted-content execution sandbox. All pages and assets require the same
operator token as the thread dashboard.

The operator owns persistence and backup of the content directory. Keep private
briefings outside public repositories, images and example fixtures. An upstream
proxy that routes only `/threads/` also needs explicit routes for the root index
and the collections it should expose. Its existing audience remains unchanged.

## Dashboard and pages-only modes

Deck runs in one of two modes, chosen by its configuration
([ADR-0013](../../docs/adr/0013-deck-page-collections.md#pages-only-mode-run-apart-from-the-dashboard)):

- **Dashboard mode**, with at least one `[[threads.instance]]`, runs beside T3:
  the thread dashboard at `basePath`, the backlog view when `[backlog]` is set,
  and, unless `collections = false`, the root index and the collections. Its
  content directory is a local directory the operator fills.
- **Pages-only mode**, with no `[[threads.instance]]`, runs on a host that
  stays up and serves only the root index, the collections, `/static/` and
  `/healthz`. It runs no adapter commands, summaries or backlog, and writes no
  state, cache or model configuration. `basePath` and `[backlog] basePath`
  then name the dashboard views' paths in the dashboard process: the root
  index links to them and they stay reserved (not found here, the bare path
  redirected to its slash form), so no collection can claim a path the proxy
  routes to the dashboard. `basePath` must not be `/`, and `[backlog]` takes
  only `basePath` and refuses any other key in it, even one set to its
  default; the backlog path is reserved only when it is set.

Both modes verify the forwarded ID token against the same issuer, client ID
and groups, with the same content security policy. One authenticating proxy
in front of both, on one origin, routes the dashboard views' paths (such as
`/threads/` and `/backlog/`) to the dashboard process and every other path to
the pages process; the dashboard process can then set `collections = false`.

A pages-only configuration:

```toml
listen = "0.0.0.0:3775"
publicOrigin = "https://deck.example.com"
contentDir = "/content/current"   # the sync process's checkout link
basePath = "/threads/"            # the dashboard's path, linked and reserved

[auth]
issuer = "https://id.example.com"
clientID = "deck"
groups = ["admins"]

[backlog]                         # only when the dashboard serves the backlog view
basePath = "/backlog/"
```

In the pages-only mode the content comes from a Git repository the operator
owns, through its served branch and reviewed pull requests. Deck has no Git
client and holds no repository credential: a sync process beside it, such as a
Git sync sidecar with a read-only credential, keeps a checkout of the branch
and replaces it atomically, for example by swapping a symlink to the new
revision. Point `contentDir` at that link. Deck opens the content root afresh
for each request, so a new revision shows without a restart and a request
never mixes two revisions. Files at the top of the checkout, such as a
README, and dotfiles are ignored, and the root index lists only directories
holding at least one valid bundle, so a `docs/` directory without bundles
does not show as an empty collection.

### Image

`nix build .#deck-image` builds a Docker-format image archive (a gzipped
tar) of the pages-only mode (Linux only), loadable with `podman load` or
`docker load`, or pushed with
`skopeo copy docker-archive:result docker://REGISTRY/deck:TAG`. Its
entrypoint is `deck serve -config /etc/deck/config.toml`; mount the
configuration there and the content volume at the configured `contentDir`.
The image runs as user and group 65532, needs no writable path, so it works
with a read-only root filesystem and all capabilities dropped, carries a CA
bundle for the issuer's OIDC discovery, and listens on port 3775 once
`listen` names a non-loopback address. Use the unauthenticated `/healthz` for
liveness and readiness probes.

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
