// deck's shell for page bundles (ADR-0013, ADR-0015): the same top bar the
// deck views render, built from /nav.json. A bundle marks its own top bar
// with data-deck-shell and loads this script with defer after theme.js. The
// marked bar stays as it is if this script or /nav.json is missing, so it
// is the fallback; otherwise it is replaced by a full-width shell at the top
// of the page, which keeps the bar's colour scheme toggle.
"use strict";

(function () {
  const old = document.querySelector("[data-deck-shell]");
  if (!old) return;

  const current = (url) => (url === "/" ? location.pathname === "/" : location.pathname.startsWith(url));

  const build = (items) => {
    const header = document.createElement("header");
    header.className = "deck-shell deck-noprint";
    const brand = document.createElement("a");
    brand.className = "deck-shell-brand";
    brand.href = "/";
    brand.textContent = "Deck";
    const nav = document.createElement("nav");
    nav.className = "deck-shell-nav";
    nav.setAttribute("aria-label", "Deck");
    for (const item of items) {
      const a = document.createElement("a");
      a.href = item.url;
      a.textContent = item.label;
      if (item.collection) a.className = "collection";
      if (current(item.url)) a.setAttribute("aria-current", "page");
      if (item.count) {
        const n = document.createElement("span");
        n.className = "deck-shell-count" + (item.tone ? " " + item.tone : "");
        n.textContent = String(item.count);
        n.setAttribute("aria-label", item.count + " waiting");
        a.append(n);
      }
      nav.append(a);
    }
    // theme.js may already have filled the bar's slot; move it over as it is.
    const slot = old.querySelector("[data-deck-theme]") || document.createElement("span");
    slot.classList.add("deck-shell-theme");
    header.append(brand, nav, slot);
    return header;
  };

  fetch("/nav.json", { cache: "no-store", credentials: "same-origin" })
    .then((res) => (res.ok ? res.json() : Promise.reject(new Error("HTTP " + res.status))))
    .then((view) => {
      if (!view || !Array.isArray(view.items)) return;
      document.body.prepend(build(view.items));
      old.remove();
    })
    .catch(() => {
      // Keep the bundle's own bar.
    });
})();
