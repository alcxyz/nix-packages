// deck's shell (ADR-0013, ADR-0015). Deck's own pages load it for the Pages
// menu; page bundles load it for the whole top bar.
//
// The Pages menu is a <details>, which opens and closes without script; this
// closes it on Escape, on a click outside it, after following a link in it
// and when the browser restores the page from its cache.
//
// For bundles it builds the same top bar the deck views render from
// /nav.json. A bundle marks its own top bar with data-deck-shell and loads
// this script with defer after theme.js. The marked bar stays as it is if
// this script or /nav.json is missing, so it is the fallback; otherwise it is
// replaced by a full-width shell at the top of the page, which keeps the
// bar's colour scheme toggle.
"use strict";

(function () {
  const openMenus = () => document.querySelectorAll("details.deck-shell-menu[open]");
  const close = (menu, focus) => {
    menu.open = false;
    if (focus) menu.querySelector("summary").focus();
  };

  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    for (const menu of openMenus()) close(menu, menu.contains(document.activeElement));
  });
  document.addEventListener("click", (e) => {
    const target = e.target instanceof Element ? e.target : null;
    const inside = target && target.closest("details.deck-shell-menu");
    const followed = target && target.closest(".deck-shell-menu-list a");
    for (const menu of openMenus()) {
      if (menu !== inside || followed) close(menu, false);
    }
  });
  window.addEventListener("pageshow", () => {
    for (const menu of openMenus()) close(menu, false);
  });

  const old = document.querySelector("[data-deck-shell]");
  if (!old) return;

  const current = (url) => (url === "/" ? location.pathname === "/" : location.pathname.startsWith(url));

  const link = (item) => {
    const a = document.createElement("a");
    a.href = item.url;
    a.textContent = item.label;
    if (item.tool) a.className = "tool";
    if (current(item.url)) a.setAttribute("aria-current", "page");
    if (item.count) {
      const n = document.createElement("span");
      n.className = "deck-shell-count" + (item.tone ? " " + item.tone : "");
      n.textContent = String(item.count);
      n.setAttribute("aria-label", item.count + " waiting");
      a.append(n);
    }
    return a;
  };

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
    const links = document.createElement("div");
    links.className = "deck-shell-links";
    const list = document.createElement("div");
    list.className = "deck-shell-menu-list";
    let pagesCurrent = false;
    for (const item of items) {
      if (item.collection) {
        list.append(link(item));
        pagesCurrent = pagesCurrent || current(item.url);
      } else {
        links.append(link(item));
      }
    }
    nav.append(links);
    // With no collections there is no menu.
    if (list.childElementCount > 0) {
      const menu = document.createElement("details");
      menu.className = "deck-shell-menu";
      const summary = document.createElement("summary");
      summary.textContent = "Pages";
      if (pagesCurrent) summary.setAttribute("aria-current", "true");
      menu.append(summary, list);
      nav.append(menu);
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
