# ADR-0013: Operator-authored page collections in Deck

**Status:** Accepted
**Date:** 2026-10-22
**Amended:** 2026-10-24 (content from Git, pages-only mode), 2026-10-25 (shared colour scheme assets), 2026-10-25 (shared design stylesheet), 2026-10-26 (JSON data assets), 2026-11-04 (pinned pages), 2026-11-07 (overview, live tiles and the shell, [#786](https://git.alc.xyz/alcxyz/nix-packages/issues/786)), 2026-11-10 (tool links and the Pages menu, [#691](https://git.alc.xyz/alcxyz/nix-packages/issues/691))
**History:** [replaced text](archive/0013-deck-page-collections-history.md)
**Applies to:** `tools/deck/`

## Context

Interactive briefings and planning worksheets produced during work need durable,
organised URLs alongside Deck's thread dashboard. Their content belongs to the
operator and may be private, while Deck is reusable public tooling. Tracked in
[#626](https://git.alc.xyz/alcxyz/nix-packages/issues/626).

The thread dashboard has to run beside T3
([ADR-0012](0012-deck-t3-thread-dashboard.md)), so collections served by the
same process from a directory on that host would be reachable only while that
one workstation is up, with the host's backup as their only copy and no
history of edits. The collections hold worksheets and proposals that outlive a
work session, and nothing in them depends on T3. Tracked in
[#643](https://git.alc.xyz/alcxyz/nix-packages/issues/643).

The same Deck should look and switch colour scheme the same way on every page,
Deck's views and bundles alike, and offers printed to PDF must stay light
([#652](https://git.alc.xyz/alcxyz/nix-packages/issues/652)).
[ADR-0015](0015-deck-design-system.md) gives Deck's views and the bundles one
design ([#661](https://git.alc.xyz/alcxyz/nix-packages/issues/661)).

A bundle can be a live report: its script loads a data file that a process
outside Deck rewrites on a schedule, such as a timer that fetches activity
from another service's read-only API. With several of them, seeing whether
any shows a problem meant opening each one, and every page had its own top
bar ([#786](https://git.alc.xyz/alcxyz/nix-packages/issues/786)).

## Decision

### Collections and bundles

Deck serves read-only collections of operator-authored static page bundles.
The root index is Deck's overview: it links to the thread dashboard, the
configured tools (below) and the available collections, counts what waits on the owner in the views the
process runs, and shows a live tile for each page that ships one (below).
Each collection has an index and pages at `/<collection>/<slug>/`; dated slugs
can preserve individual proposals or assessments without replacing older ones.

Content lives in `contentDir`, defaulting to `$XDG_DATA_HOME/deck/pages` or
`~/.local/share/deck/pages`. It is runtime data, separate from Deck's source,
Nix package and synthetic fixtures. A bundle has `metadata.json`, `index.html`
and local assets: top-level files with lowercase names and a short list of web
extensions (the README lists them). Metadata supplies its title, date and summary, and may pin a page that stays current ahead of the dated ones. The operator
publishes bundles through the filesystem; there is no upload or editing API.

All collection routes and assets use the existing token verification and
response headers from [ADR-0012](0012-deck-t3-thread-dashboard.md). The content
security policy stays unchanged: JavaScript and CSS use local files rather
than inline code or external dependencies. Bundles are trusted application
content authored or reviewed by the operator.

Filesystem access is confined to the configured content root, including when
symlinks are present. Metadata, dotfiles and directory listings are not served.
Only valid page bundles appear in the indexes. The thread adapters and their
mutation routes retain their existing ownership and behaviour.

### JSON data assets

- **Bundles may serve top-level `json` assets** under the same naming rules,
  token check and headers as their other assets. The writer replaces the file
  atomically, so Deck never serves a partial file. A request that races the
  rename gets 404 (Deck refuses a file that changed while opening), so a live
  page should retry on its next refresh rather than treat 404 as final.
- **`metadata.json` stays unserved.** It is index input, not a page asset.

### Content from Git

**Content comes from a Git repository the operator owns.** Its served branch
is the source of the collections, and changes reach it by commit and pull
request. A sync process outside Deck, such as a Git sync sidecar, keeps a
checkout of that branch in the content directory and replaces it
atomically. Deck stays filesystem-only: it has no Git client and holds no
repository credentials. It resolves the content root per request, so a new
revision needs no restart and a partly written checkout is never served.

### Pages-only mode, run apart from the dashboard

- **Pages-only mode.** A Deck process configured without thread instances runs
  no adapters or summaries and registers no dashboard routes. It serves the
  root index and the collections, and the root index links to the configured
  dashboard views instead. It still reserves the dashboard views' paths, taken
  from its configuration, so no collection can claim a path the proxy routes
  to the dashboard. It serves its own static assets, such as the index
  stylesheet, at `/static/`, so its pages do not depend on the dashboard
  process. A dashboard process can turn its collections off.
- **The two run apart.** The pages process runs on a host that stays up; the
  dashboard stays beside T3. The authenticating proxy routes the dashboard
  views' paths (such as the thread dashboard's base path) to the dashboard
  process and every other path to the pages process. Both keep verifying the
  forwarded token themselves, with the same issuer, audience, group and
  content security policy.
- **One origin, one trust boundary.** Both processes share the public origin,
  so links stay stable and one sign-in covers both. A page's script can
  therefore act on the dashboard as the signed-in operator. The served branch,
  everyone who can push to it, the sync process and the host running the pages
  process are inside the dashboard's trust boundary, as page bundles already
  were as trusted application content. The served branch changes only through
  reviewed pull requests, and the sync credential is read-only.
- **An image of the pages mode** is built from the Nix package, so a cluster
  can run it without a Nix toolchain.

### Live tiles

- **A bundle that ships a top-level `tile.js` gets a live tile** on the
  overview and on its collection index. Deck only notes that the file exists,
  a regular file like any other asset, and never reads the bundle's data.
- **The browser computes the tile.** `/static/tiles.js` imports `tile.js` as
  an ES module and calls its default export with a context that fetches the
  bundle's own files (`json(name)`, which reports a missing file as status
  404, and `url(name)`). The module returns a status (`ok`, `watch` or `act`),
  a one-line answer, up to three short figures, an optional series for a
  sparkline, when its data was made and after how many minutes it counts as
  stale. The runtime marks stale data at least `watch`, puts the tiles that
  need a look first and refreshes them every five minutes while the page is
  visible. Without script a tile shows the page's title and summary.
- **A tile runs as the page it sits on.** Its module runs on the overview with
  the same origin as every bundle script, inside the same trust boundary
  (above), so it adds no new trust.

### The shell

- **One top bar on every page** ([ADR-0015](0015-deck-design-system.md)):
  Deck's views, the overview and the collection indexes render the overview,
  the views, the configured tools and the collections that have a page (in
  a Pages menu), with the current one marked and counts on Threads (waiting
  on the owner) and Approvals (pending).
- **`/nav.json` serves the same links to bundles**, with the counts, behind
  the same token check, in both modes; the pages-only mode has no counts. It
  is no collection slug, so no collection can claim it.
- **A bundle opts in** by marking its own top bar with `data-deck-shell` and
  loading `/static/shell.js` with `defer` after `theme.js`. The script
  replaces the marked bar with the shell and keeps the bar's colour scheme
  toggle. Without the script, or where `/nav.json` fails, the bundle's own
  bar stays.

### Tool links

- **Operator tools on the same origin are configured links.** Each `[[tool]]`
  entry names a label, an absolute, slash-terminated path and an optional
  one-line description, for a tool such as devstats that another process
  serves behind the same proxy
  ([#691](https://git.alc.xyz/alcxyz/nix-packages/issues/691)). The shell
  links them after the views, `/nav.json` marks them with `tool`, and the
  overview lists them with their descriptions, in both modes.
- **Their paths are reserved** like the pages-only mode's dashboard paths: no
  collection whose name a tool path starts with is served or listed, deck
  answers not found below the path and redirects the bare path to its slash
  form. A tool path must not overlap `basePath`, `[backlog] basePath`, the
  approvals path, `/static/`, `/nav.json`, `/healthz` or another tool's path.
- **Deck neither proxies nor checks the tools.** Routing, authentication and
  availability stay with the proxy and the tool.

### Shared theme and design assets

- **Deck serves shared theme assets at `/static/`.** `theme.js` resolves a
  light, dark or system choice, kept per browser in local storage, to
  `data-theme="light"` or `"dark"` on `<html>` before the first paint, and
  draws the toggle; `theme.css` sets the matching `color-scheme`, styles the
  toggle and hides it in print. Deck's views load them from their own
  `static/` path and the library and bundles from `/static/`, the same files;
  one origin gives every page the same choice.
- **Deck also serves `/static/base.css`**, which carries the design's tokens
  and shared components ([ADR-0015](0015-deck-design-system.md)).
- **Bundles may reference `theme.css`, `theme.js`, `base.css` and
  `shell.js` at `/static/`** besides their own local files. These four files
  are the only assets a bundle takes from outside its own directory;
  everything else stays local. The content security policy is unchanged, since the assets come from
  the same origin and contain no inline code.
- **Load order:** `theme.css`, then `base.css`, then the bundle's own
  stylesheets. `base.css` applies its element rules only to a page that names
  its purpose with `data-deck` on `<body>`.
- **Dark styles key on the attribute in screen media**
  (`@media screen { :root[data-theme="dark"] ... }`), not on
  `prefers-color-scheme`, so the toggle decides and print stays light.
- **`/static/` is reserved** from collection names and from the views' base
  paths in every configuration, so the pages-only mode can serve the same
  theme assets and the library stylesheet there without the dashboard.

## Alternatives considered

- **Embed briefings in Deck's package:** rejected because private content would
  enter a public source and artifact lifecycle, and each page would require a
  package release.
- **Build a content editor and persistence API:** deferred because approved
  filesystem bundles satisfy publishing without another mutation workflow.
- **Keep one process on the T3 host, with a Git checkout as content
  directory.** Gives history and an off-host copy, but the collections still go
  down with the workstation.
- **Object storage as the content source.** Rejected: objects have no review
  or history, Deck would need a storage client and credentials, and publishing
  would still need its own tool.
- **A separate origin for the collections.** Would keep page script away from
  the dashboard, but needs its own hostname, certificate and proxy client, and
  changes existing page links. Not chosen: the authenticating proxy in front
  of both already sees and could forge all dashboard traffic, so the pages
  host adds little to what the dashboard already trusts.
- **A generic static server for the collections.** Rejected: it would drop
  Deck's own token check, generated indexes, metadata handling and content
  confinement, or have to reimplement them.
- **A copy of the theme script and rules, or of the design tokens, in each
  bundle.** Keeps bundles self-contained, but the copies drift (the bundle
  palettes already had) and a change to the toggle means editing every bundle
  and Deck separately.
- **Deck injecting the assets when it serves a bundle.** Needs no bundle
  markup, but Deck would rewrite trusted HTML, and a bundle would behave
  differently when opened any other way.
- **`light-dark()` colour tokens driven by `color-scheme`.** Would let pages
  without the script follow the system, but every existing dark override would
  have to be folded into its light rule instead of keeping its dark block.
- **Write the data under an allowed extension, such as `data.js`.** Rejected:
  it mislabels data as script and hides the bundle's real contract.
- **A Deck API that proxies the other service.** Rejected: Deck would hold
  that service's credentials, against its filesystem-only design.
- **Tiles from a summary file that each refresher writes and Deck renders.**
  Every refresher and Deck would share a schema, a change to a tile would
  need a refresher deploy, and Deck would read operator data it otherwise
  only serves. A `tile.js` beside the page keeps the tile with the code that
  already understands that page's data.
- **Deck computing tiles from each page's `data.json`.** Deck would have to
  know every report's schema.
- **A frame per tile.** Heavier, and the `frame-ancestors 'none'` policy
  forbids it.

## Consequences

- New pages need no Deck rebuild or restart. They retain the existing operator
  audience and are not a client-facing publishing surface. Each page remains
  responsible for its own responsive presentation and interaction code.
- Operators own the content's persistence and backup, separately from the
  application package, through a Git repository. Publishing is a commit to
  the served branch; the operator owns the repository, its read-only sync
  credential and the sync process's schedule.
- The collections stay reachable while the T3 host is off; the dashboard does
  not.
- There are two Deck deployments to configure and keep on the same release.
- A content directory on the T3 host stops being a source once the pages
  process serves the same bundles.
- One toggle and one stored choice cover Deck's views, the library and every
  bundle that loads the shared assets. Changing the toggle is a Deck release;
  bundles need no edit.
- A bundle opened outside Deck, or saved without the shared assets, shows its
  light scheme without a toggle.
- A bundle that uses `base.css` needs a Deck release that serves it, and
  shows unstyled tokens on an older Deck. Publish such bundles after that
  release is deployed.
- Standalone exports and PDF builds of a bundle ship their own copy of
  `base.css`, since `/static/` exists only on Deck.
- A tile or a change to one is a bundle change, with no Deck release. The
  overview fetches each tiled page's data every five minutes while it is
  open.
- A collection with tiles is linked beside the overview's tiles rather than
  listed in its library.
- A bundle that loads `shell.js` keeps its own top bar on a Deck release
  without the shell.
- Each process's shell lists what that process knows. With the pages and the
  dashboard run apart, the dashboard's bar lacks the collections when its
  collections are off, and the pages process's bar and `/nav.json` have no
  Approvals link and no counts; one process serving both shows the full bar.
- Bundle authors must not leave private JSON in a bundle they would not serve,
  as with any other asset.
- A data file written on one host is served only by the Deck process that
  reads that host's content directory. A Git-synced checkout does not support
  writer-refreshed files yet: each sync swaps in a fresh revision without
  them. Such a setup needs its own decision, for example a data directory
  outside the checkout.
