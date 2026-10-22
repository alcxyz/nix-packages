# ADR-0013: Operator-authored page collections in Deck

**Status:** Accepted
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
and local assets. Metadata supplies its title, date and summary. The operator
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
  using the same operator access and the existing Deck process.
- **Build a content editor and persistence API:** deferred because approved
  filesystem bundles satisfy publishing without another mutation workflow.

## Consequences

New pages need no Deck rebuild or restart. They retain the existing operator
audience and are not a client-facing publishing surface. Operators own the
content directory's persistence and backup, separately from the application
package. Each page remains responsible for its own responsive presentation
and interaction code.
