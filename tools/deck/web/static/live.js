// Refreshes deck's live parts in place (ADR-0013): the overview and the
// collection indexes with tiles mark them with data-deck-live, and deck
// renders them on the server from each page's tile.json. Every five minutes
// while the tab is visible, and on coming back to it after more than a
// minute, this fetches the page again and swaps each marked element for its
// fresh counterpart. A failed fetch, or a redirect such as an expired
// sign-in, keeps the page as it is. Without script the page shows the tiles
// as they were when it loaded.
"use strict";

(function () {
  const refreshMs = 5 * 60 * 1000;
  const returnMs = 60 * 1000;

  // An open Pages menu stays open across a swap of the shell.
  const swap = (old, fresh) => {
    const open = new Set();
    old.querySelectorAll("details[open]").forEach((d, i) => open.add(i));
    fresh.querySelectorAll("details").forEach((d, i) => { d.open = open.has(i); });
    old.replaceWith(document.importNode(fresh, true));
  };

  // One refresh at a time, so an older answer never overwrites a newer one.
  let lastRun = Date.now();
  let running = null;
  const refresh = () => {
    if (running) return running;
    lastRun = Date.now();
    running = fetch(location.href, { cache: "no-store", credentials: "same-origin", redirect: "manual" })
      .then((res) => {
        if (!res.ok || res.type === "opaqueredirect") return;
        return res.text().then((html) => {
          const doc = new DOMParser().parseFromString(html, "text/html");
          for (const old of document.querySelectorAll("[data-deck-live]")) {
            const id = old.getAttribute("data-deck-live");
            const fresh = doc.querySelector('[data-deck-live="' + CSS.escape(id) + '"]');
            if (fresh) swap(old, fresh);
          }
        });
      })
      .catch(() => {
        // Keep the page as it is; the next refresh tries again.
      })
      .finally(() => { running = null; });
    return running;
  };

  setInterval(() => { if (!document.hidden) refresh(); }, refreshMs);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && Date.now() - lastRun > returnMs) refresh();
  });
})();
