# ADR-0013: Operator-authored page collections in Deck

**Status:** Accepted; content source and pages-only mode amended 2026-10-24; shared colour scheme assets amended 2026-10-25
**Date:** 2026-10-22
**Applies to:** `tools/deck/`

## Context

Interactive briefings and planning worksheets produced during work need durable,
organised URLs alongside Deck's thread dashboard. Their content belongs to the
operator and may be private, while Deck is reusable public tooling. Tracked in
[#626](https://git.alc.xyz/alcxyz/nix-packages/issues/626).

## Decision

Deck serves read-only collections of operator-authored static page bundles.
The root index links to the thread dashboard and the available collections.
Each collection has an index and pages at `/<collection>/<slug>/`; dated slugs
can preserve individual proposals or assessments without replacing older ones.

Content lives in `contentDir`, defaulting to `$XDG_DATA_HOME/deck/pages` or
`~/.local/share/deck/pages`. It is runtime data, separate from Deck's source,
Nix package and synthetic fixtures. A bundle has `metadata.json`, `index.html`
and local assets: top-level files with lowercase names and a short list of web
extensions (the README lists them). Metadata supplies its title, date and summary. The operator
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

## Alternatives considered

- **Embed briefings in Deck's package:** rejected because private content would
  enter a public source and artifact lifecycle, and each page would require a
  package release.
- **Run a separate static-content service:** unnecessary for a small collection
  using the same operator access and the existing Deck process. Revisited in
  the amendment below: the collections now run as a second Deck process.
- **Build a content editor and persistence API:** deferred because approved
  filesystem bundles satisfy publishing without another mutation workflow.

## Consequences

New pages need no Deck rebuild or restart. They retain the existing operator
audience and are not a client-facing publishing surface. Operators own the
content's persistence and backup, separately from the application package
(from the amendment below, through a Git repository). Each page remains responsible for its own responsive presentation
and interaction code.

## Amendment (2026-10-24): content from Git, served apart from the dashboard

Tracked in [#643](https://git.alc.xyz/alcxyz/nix-packages/issues/643).

### Context

The thread dashboard has to run beside T3 ([ADR-0012](0012-deck-t3-thread-dashboard.md)),
so the collections, served by the same process from a directory on that host,
were only reachable while that one workstation was up. Their only copy was the
host's backup, and edits left no history. The collections now hold worksheets
and proposals that outlive a work session, and nothing in them depends on T3.

### Decision

- **Content comes from a Git repository the operator owns.** Its served branch
  is the source of the collections, and changes reach it by commit and pull
  request. A sync process outside Deck, such as a Git sync sidecar, keeps a
  checkout of that branch in the content directory and replaces it
  atomically. Deck stays filesystem-only: it has no Git client and holds no
  repository credentials. It resolves the content root per request, so a new
  revision needs no restart and a partly written checkout is never served.
- **Pages-only mode.** A Deck process configured without thread instances runs
  no adapters or summaries and registers no dashboard routes. It serves the
  root index and the collections, and the root index links to the configured
  dashboard views instead. It still reserves the dashboard views' paths, taken
  from its configuration, so no collection can claim a path the proxy routes
  to the dashboard. It serves its own static assets, such as the index
  stylesheet, outside those paths, so its pages do not depend on the
  dashboard process, and reserves that asset path from collection names as
  well. A dashboard process can turn its collections off.
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

### Alternatives considered

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

### Consequences

- The collections stay reachable while the T3 host is off; the dashboard does
  not, as before.
- There are two Deck deployments to configure and keep on the same release.
- Publishing is a commit to the served branch; the operator owns the
  repository, its read-only sync credential and the sync process's schedule.
- A content directory on the T3 host stops being a source once the pages
  process serves the same bundles.

## Amendment (2026-10-25): one colour scheme toggle for views and bundles

Tracked in [#652](https://git.alc.xyz/alcxyz/nix-packages/issues/652).

### Context

Deck's views and the library followed the system colour scheme with no
toggle, while each bundle styled itself: some followed the system scheme,
others were light only. The same Deck should look and switch the same way on
every page, and offers printed to PDF must stay light.

### Decision

- **Deck serves shared theme assets at `/static/`.** `theme.js` resolves a
  light, dark or system choice, kept per browser in local storage, to
  `data-theme="light"` or `"dark"` on `<html>` before the first paint, and
  draws the toggle; `theme.css` sets the matching `color-scheme`, styles the
  toggle and hides it in print. Deck's views load them from their own
  `static/` path and the library and bundles from `/static/`, the same files;
  one origin gives every page the same choice.
- **Bundles may reference these two assets** besides their own local files.
  This is the only exception to bundles being self-contained; everything else
  stays local, and the content security policy is unchanged, since the assets
  come from the same origin and contain no inline code.
- **Dark styles key on the attribute in screen media**
  (`@media screen { :root[data-theme="dark"] ... }`), not on
  `prefers-color-scheme`, so the toggle decides and print stays light.
- **`/static/` is reserved** from collection names and from the views' base
  paths in every configuration, so the pages-only mode from the previous
  amendment, not yet implemented
  ([#643](https://git.alc.xyz/alcxyz/nix-packages/issues/643)), can serve the
  same theme assets and the library stylesheet there without the dashboard.

### Alternatives considered

- **A copy of the script and rules in each bundle.** Keeps bundles
  self-contained, but the copies drift and a change to the toggle means
  editing every bundle and Deck separately.
- **Deck injecting the assets when it serves a bundle.** Needs no bundle
  markup, but Deck would rewrite trusted HTML, and a bundle would behave
  differently when opened any other way.
- **`light-dark()` colour tokens driven by `color-scheme`.** Would let pages
  without the script follow the system, but every existing dark override would
  have to be folded into its light rule instead of keeping its dark block.

### Consequences

- One toggle and one stored choice cover Deck's views, the library and every
  bundle that loads the shared assets.
- A bundle opened outside Deck, or saved without the shared assets, shows its
  light scheme without a toggle.
- Changing the toggle is a Deck release; bundles need no edit.
