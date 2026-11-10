# deck

Deck also serves operator-authored interactive briefings in read-only page
collections, and optionally a live backlog view (below). The root index is
an overview: what waits on you in the threads, approvals, backlog and live
pages, a live tile for each page that declares one, and the other
collections. Every page
shares one top bar linking the overview, the views and each collection. A second deck process without thread instances can serve the
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

The count cards at the top of both views are filters. Pressing one shows only
the rows it counts, and pressing it again clears that. It is kept in the URL
fragment (`#card=you`), so it survives a refresh and a reload and can be
linked to. Each card counts the rows that pass the page's search and selects,
whichever card is active, and a card with nothing to show is disabled.
Section headings count the rows on screen, and the average progress follows
the unsettled threads on screen.

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
  --permission-mode dontAsk --output-format json
```

in an empty directory, with only `PATH`, `HOME`, `CLAUDE_CONFIG_DIR`,
`TMPDIR` and `LANG` from deck's environment. Every command and model call
runs in its own process group, killed as a whole at its deadline.

Since a call saves no session, deck appends what each one spent to
`$XDG_STATE_HOME/deck/model-usage.jsonl`, one JSON line of the time, `summary`
or `verdict`, the model and its token counts, with no prompt or answer. Only a
call that reaches a result reports its usage, so one killed at its deadline or
crashing partway isn't counted, and the file can undercount.

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

[[tool]]                 # optional, repeatable; see Tool links
label = "Stats"
path = "/devstats/"
description = "Development activity" # optional, one line

[live]                   # optional; see Live tiles and alerts
notify = ["/path/to/notify", "--class", "live", "{text}", "--link", "{link}"]
stateFile = "/home/user/.local/state/deck/live.json" # optional; default described below
```

`summaryRole` is also included in `summaryRoles` when absent. The offered
roles retain configuration order, with duplicates removed, and must resolve
to at least one usable no-tools entry of the shared LLM configuration.

Thread links are `{webURL}/{environmentId}/{threadId}`.

## Top bar and tool links

Every deck page shares one top bar: the brand, Overview, Threads (with the
count waiting on you), Backlog and Approvals when configured, the tool links,
then a **Pages** menu holding the collections at the right end, and the
colour scheme toggle. A collection with live tiles counts its pages that
need a look (see [Live tiles and alerts](#live-tiles-and-alerts)).
The menu is a `<details>` disclosure, so it opens without script;
`/static/shell.js`, which deck's pages and opted-in bundles load, closes it on
Escape, on a click outside it and after a link in it is followed. With no
collections there is no menu. At phone width the links scroll sideways and
the menu stays at the end of the row.

Each `[[tool]]` entry links an operator tool that another process serves on
the same origin behind the same proxy, such as devstats at `/devstats/`
([#691](https://git.alc.xyz/alcxyz/nix-packages/issues/691)). The bar links
it after the views, `/nav.json` lists it with `"tool": true`, and the
overview lists it under Tools with its description, in both modes. Its path
must be absolute and end in a slash, hold only letters, digits and
`. _ ~ -` in its segments, and must not overlap `basePath`,
`[backlog] basePath`, `/consent/`, `/static/`, `/nav.json`, `/healthz` or
another tool's path. The path is reserved: a collection whose name it starts
with is neither served nor listed, deck answers not found below it, and the
bare path redirects to its slash form. Deck does not proxy the tool; route the
path to it in the proxy.

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
other issue, collapsed. Suggested actions are shown, never performed. The
open issues count is of the rows shown, so it leaves out a dependency dashboard
issue unless the shortlist picks it.

With an item command, each shortlisted issue also gets a verdict, as the
skill's readers give: `close`, `dispatch`, `refresh`, `keep`, `drop` or
`unclear`, a one-line reason and the issue's completion criteria. deck runs
`ITEM --max-chars 8000 REF [PR_REF...]` for the issue and its three newest
merged pull requests (each reference checked and passed as its own argument),
then makes one model call with no tools through `verdictRole`, at
`verdictEffort`. The prompt holds deck's facts, then the issue's text as
untrusted data: up to 8000 characters of its body and 2000 of each other
text. If the item command's output lists `linked` items (each with
`ref`, `kind`, `state` and `title`), deck adds them to the items the inventory
relates to the issue. Verdicts are cached in
`$XDG_STATE_HOME/deck/backlog/verdicts.json` against the issue's `updatedAt`,
its merged pull requests and the body limit, so a release that changes the limit
judges every shortlisted issue again. A changed issue is judged again once its
last attempt is `verdictIntervalMinutes` old; failures back off as summaries do.
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

## Credential approvals

With `[consent]`, deck relays credential approvals to a person
([ADR-0018](../../docs/adr/0018-passkey-credential-approvals.md)): `/consent/`
lists the requests waiting in `spoolDir`, `/consent/ID` shows one and asks for
a passkey, and `/consent/enroll` creates that passkey. deck only passes files
between the browser and
[credential-approval](../credential-approval/README.md), which verifies the
signature; deck holds no key. Read-only members can see the pages and answer
nothing. Serve deck over HTTPS at its `publicOrigin`,
since passkeys need a secure context and are bound to that host name. Only the
dashboard process can relay, so a proxy in front of two processes routes
`/consent/` to it.

```toml
[consent]
spoolDir = "/run/user/1000/credential-consent"
```

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
`/briefings/`. Indexes escape metadata and list newer dates first. A page that
stays current, such as a live report, can add `"pinned": true` to its metadata
to stay ahead of the dated pages, under its own heading, and
`"tile": true` for a live tile (see
[Live tiles and alerts](#live-tiles-and-alerts)). Collection
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
Bundles may also use Deck's shared colour scheme toggle, design stylesheet
and shell, the only assets a bundle takes from outside its own directory.
Load them in `<head>`: the stylesheets in this order before the bundle's own,
and `theme.js` without `defer`, so the choice applies before the first paint.
Name the page's purpose on `<body>`:

```html
<link rel="stylesheet" href="/static/theme.css">
<link rel="stylesheet" href="/static/base.css">
<link rel="stylesheet" href="style.css">
<script src="/static/theme.js"></script>
...
<body data-deck="sheet">
```

To share Deck's top bar, mark the bundle's own bar with `data-deck-shell` and
load `<script src="/static/shell.js" defer></script>` after `theme.js`; it
needs `defer`, since it looks for the bar once, when it runs. The
script replaces the marked bar with Deck's shell, built from `/nav.json`, and
keeps the bar's `data-deck-theme` toggle; without it, or on an older Deck,
the bundle's own bar stays.

The script draws a light, dark and system toggle in the bottom right corner,
or inside an element marked `data-deck-theme` if the page has one; give it a
slot where it does not cover content. It keeps the choice per browser, shared
by Deck's views, its library and every bundle, and without a choice follows
the system. It sets `data-theme="light"` or `"dark"` on `<html>`.

`base.css` is deck's design ([ADR-0015](../../docs/adr/0015-deck-design-system.md)):

- **Purpose.** `data-deck` is one of the following. It sets the page's text
  size, line height and width (`--deck-size`, `--deck-width`):
  - `live`: a dense overview, on the grey canvas on screen;
  - `index`: a list to find and open, on the canvas too;
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
  - `deck-shell` (from `shell.js`), `deck-tiles` with `deck-tile` (which
    deck renders on its overview and indexes), `deck-dot` and `deck-spark`,
    coloured by a `data-status` of `ok`, `watch` or `act` on an ancestor;
  - `deck-seg`, a segmented control such as a time window, with
    `aria-pressed` on the chosen button;
  - `deck-sr` hides text visually but keeps it for screen readers;
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
`/static/` is reserved: no collection can use that name, and `/nav.json` is
no collection slug. Tool paths are reserved too (see
[Top bar and tool links](#top-bar-and-tool-links)).

Bundles are trusted application content; Deck supplies authentication, not an
untrusted-content execution sandbox. All pages and assets require the same
operator token as the thread dashboard.

The operator owns persistence and backup of the content directory. Keep private
briefings outside public repositories, images and example fixtures. An upstream
proxy that routes only `/threads/` also needs explicit routes for the root index
and the collections it should expose. Its existing audience remains unchanged.

## Live tiles and alerts

A page that answers a question at a glance, such as a live report whose data
a refresher rewrites on a timer, can have a live tile on the overview and on
its collection index
([ADR-0013](../../docs/adr/0013-deck-page-collections.md#live-tiles)). The
bundle declares it in its metadata with `"tile": true`, and the refresher
that writes the page's data also writes `tile.json` beside it, every time,
after the data, atomically (a temporary file in the same directory, renamed
over `tile.json`):

```json
{
  "status": "watch",
  "headline": "Wait 24 s at p90 in 24 h, but 116 job starts were not counted.",
  "metrics": [
    {"label": "Jobs (24h)", "value": "289"},
    {"label": "Wait p90 (24h)", "value": "24 s"},
    {"label": "Failed (24h)", "value": "11", "tone": "watch"}
  ],
  "series": {"label": "Queue wait p90 per day, seconds", "values": [3, 0, null, 12.5]},
  "updated": "2026-11-09T22:51:02Z",
  "staleAfter": 15
}
```

| Field | Rule |
| --- | --- |
| `status` | `ok` (nothing to do), `watch` (worth a look) or `act` (the owner should do something now). |
| `headline` | One sentence that answers the page's question, 1 to 200 characters. |
| `metrics` | At most 3, each a `label` of at most 40 characters, a `value` of at most 12, already formatted (`24 s`, `42%`, `1,234`; 7 or fewer fit the tile, longer ones are cut with an ellipsis), and an optional `tone` of `ok`, `watch` or `act`. |
| `series` | Optional: a `label` and 2 to 60 `values`, oldest first, each a number or `null` for a missing point. |
| `updated` | When the page's data was made, RFC 3339 (`...Z`), or `null`. |
| `staleAfter` | Minutes after `updated` when the data counts as stale, above 0. |

Deck ignores unknown fields. A refresher that cannot read its source still
writes `tile.json`, with the status and headline its page gives for that
case. The tile logic lives with the refresher, which may sit in another
repository than the page: it owns both the data and the tile.

Deck reads `tile.json` on each request and renders the tile on the server,
so the tile needs no script:

- **Missing:** status `watch`, "No data yet."
- **Invalid** (not a regular file, such as a symlink; over 64 KiB; not JSON;
  or breaking a rule above): "Unavailable", with the reason in deck's log,
  once an hour per page.
- **Stale** (`updated` plus `staleAfter` in the past): at least `watch`, and
  the age reads "stale, 2 h ago".

Tiles sort `act`, `watch`, unavailable, then `ok`, and otherwise keep the
index order. A collection's entry in the top bar and `/nav.json` counts its
tiles that are `act`, `watch` or unavailable, in red when one is `act`, and
the overview's "Needs you" row ends with the same count per collection with
tiles, such as "2 live pages need a look". The pages-only mode shows those
counts too.

The overview and a collection index with tiles load `/static/live.js`, which
fetches the page again every five minutes while the tab is visible, and on
coming back to it after more than a minute, and swaps in the fresh tiles,
counts and top bar. A failed fetch or a redirect, such as an expired
sign-in, leaves the page as it is.

With a `[live]` table, the process that serves the collections checks every
tile page once a minute and runs `notify` once per turn to `act`, at most once
an hour per page. Deck runs the command directly, without a shell, replacing
an argument that is exactly `{text}` with "<Title> needs action: <headline>"
(at most 500 characters) and one that is exactly `{link}` with
`publicOrigin` and the page's path, and gives it 30 seconds. A failed command
is retried on a later check, at most every ten minutes; deck logs its exit
status, never its output. `notify` needs `publicOrigin` and the collections
served by this process.

Deck records each page's last status in `stateFile` (default
`$XDG_STATE_HOME/deck/live.json`, falling back to
`~/.local/state/deck/live.json`), written atomically and readable by its
user only, so a restart does not alert again on a page that already needed
action. On a first start without the file it records the statuses and sends
nothing. The record follows these rules:

- **Only a current tile counts.** A tile that is valid, has data and is not
  stale sets the page's status (`ok`, `watch` or `act`). A missing, unreadable
  or stale tile leaves the record as it was and sends nothing, a pending
  retry included: the retry waits until the tile is current and still `act`.
  A blip in between is therefore never a turn.
- **A turn** is a current `act` on a page whose recorded status is not `act`.
- **Absent pages keep their record** for 24 hours, so a check that cannot
  read the content directory, a collection or a bundle for a while changes
  nothing; after 24 hours absent the record is dropped.
- **Stopping deck** saves what the check had done, so a sent alert is never
  sent again after a restart.

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
  state, cache or model configuration, except the live alerts' state file
  when `[live]` is set. `basePath` and `[backlog] basePath`
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
Each process's top bar lists only what that process knows: in this split the
dashboard's bar lacks the collections, and the pages process's bar, its
overview and `/nav.json` have no Approvals link and count only the live
pages, since the pages-only mode relays no approvals. The pages process
reads the tiles, so it is the one that sends the live alerts. One process serving both shows the full
bar.

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

[[tool]]                          # linked and reserved; the proxy routes it
label = "Stats"
path = "/devstats/"
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
