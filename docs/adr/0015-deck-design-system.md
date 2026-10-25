# ADR-0015: One design for deck's views and page bundles, chosen by purpose

**Status:** Accepted
**Date:** 2026-10-25
**Applies to:** `tools/deck/web/`, page bundles served by deck

## Context

deck's pages were added one at a time: the thread dashboard and the backlog
view shared `deck.css`, with warm neutrals and a bright blue; the library and
collection indexes had `library.css`, with cool ink and blue; and each page
bundle carried its own palette, close to the library's but separate from it.
Both deck stylesheets defined the same token names with different values.
None had a type scale or print rules for the live views. Some text was below
4.5:1 and control borders below 3:1. [ADR-0013](0013-deck-page-collections.md)
already gave every page one colour scheme toggle. Tracked in
[#661](https://git.alc.xyz/alcxyz/nix-packages/issues/661).

The pages serve five purposes, which differ in density and output:

- **Live overview** (thread dashboard, backlog): glanced at often, dense, with
  status colour carrying meaning.
- **Index** (library, collection pages): find and open a page.
- **Worksheet** (internal decision sheets): read and decide, sometimes printed.
- **Presentation** (client frames): shown on screen or exported to PDF at a
  fixed frame size.
- **Personal tool** (interactive lists): form controls, kept in the browser.

## Decision

- **One design, "Mono".** Near-monochrome neutrals, one indigo accent for
  links, actions and selection, and three status colours used only where they
  carry meaning:
  - attention (amber): waits on the owner;
  - ok (teal): done or accepted;
  - danger (red): stalled or failed.

  Corners are small, there are no shadows, and figures use a monospaced face
  with tabular numerals. Every text pair in both schemes meets 4.5:1 and
  control borders meet 3:1.
- **System fonts only.** The content security policy has no `font-src`, and
  the design does not change it. The PDF builds of bundles may still embed a
  font at build time.
- **Deck serves it as `/static/base.css`**, beside `theme.css` and from each
  view's `static/`, the same file. It holds:
  - `--deck-` prefixed tokens for light and dark (surfaces, text, accent,
    status, a fixed-dark hero panel, chart series);
  - a type scale and spacing;
  - base element rules;
  - shared components: card, chip, badge, button, KPI tile, table, kicker,
    the toggle.

  Dark values follow `theme.js` under the screen-only attribute rule from
  ADR-0013, so print stays light.
- **A page states its purpose** with `data-deck` on `<body>` (`live`, `index`,
  `sheet`, `slides`, `tool`), which sets its text size, line height and width.
  Base element rules apply only under that attribute and carry no specificity
  (`:where()`). A page's own stylesheet therefore always wins, and a page
  without the attribute is unaffected by loading the file.
- **Views and bundles keep their own layout CSS.** `deck.css` and
  `library.css` lay out the views on the shared tokens and components. Bundles
  do the same for what is particular to them: slide frames, charts, worksheet
  rails. Prefixed names (`--deck-`, `.deck-`) keep the shared layer apart from
  bundle classes.
- **Print by purpose:**
  - live views print the rows the filter shows, expanded, with the filter bar
    so a filtered printout says what it leaves out, and without row actions;
  - worksheets print on A4 with navigation hidden;
  - presentations print one frame per page at their frame size;
  - personal tools print the list without controls.

  Status fills print as shown. `@page` rules stay in the page that needs them.

## Alternatives considered

- **Ink: keep the cool ink and blue the library and bundles already used**, and
  move only the dashboard to it. The least churn, but the owner preferred a
  plainer, tool-like look that suits the live views, which are used most.
- **Paper: warm neutrals, a teal accent and serif headings.** More editorial
  for documents, but softer for the dense live views.
- **Self-host a web font** so that screen and PDF match. It needs
  `font-src 'self'` in the content security policy and a font file in the
  package; the system faces are close enough.
- **One stylesheet per purpose** instead of one base file with a purpose
  attribute. Purposes differ only in a few values, and separate files would
  repeat the tokens and components.
- **Copy the tokens into each bundle.** Keeps bundles self-contained, but the
  copies drift, as the bundle palettes already had.

## Consequences

- Deck's views, its library and every migrated bundle look and switch the
  same way. A change to the design is a deck release; bundles that use the
  tokens follow without edits.
- Bundles that load `base.css` depend on a deck release that serves it.
  `/static/` exists only on deck, so a standalone export or PDF build of a
  bundle ships its own copy of `base.css` beside the export.
- `deck.css` and `library.css` no longer define colours; new colours belong
  in `base.css` as tokens.
- The live views' "new since your last visit" marker is an accent bar
  instead of a tint, so status colours stay readable on new rows.
