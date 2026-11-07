// Live tiles on deck's overview and collection indexes (ADR-0013). Each
// a[data-tile] names a bundle's tile.js, an ES module whose default export
// takes a context and returns the tile:
//   { status: "ok" | "watch" | "act", headline, metrics: [{label, value, tone}],
//     series: {label, values}, updated: ISO time or null, staleAfter: minutes }
// This script imports each one, fills its tile, marks data older than
// staleAfter, sorts the tiles that need a look first and refreshes them every
// few minutes. Without it a tile shows the page's title and summary.
"use strict";

(function () {
  const refreshMs = 5 * 60 * 1000;
  // A tile that has not answered by then shows as unavailable, so one slow
  // page cannot hold up the others or the next refresh.
  const tileTimeoutMs = 30 * 1000;
  const rank = { act: 0, watch: 1, error: 2, ok: 3, pending: 4 };
  const label = { act: "Needs action", watch: "Worth a look", ok: "OK", error: "Unavailable", pending: "Loading" };
  const svgNS = "http://www.w3.org/2000/svg";

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };

  const ago = (ms) => {
    const m = Math.max(0, Math.round(ms / 60000));
    if (m < 1) return "just now";
    if (m < 60) return m + " min ago";
    const h = Math.round(m / 60);
    if (h < 48) return h + " h ago";
    return Math.round(h / 24) + " d ago";
  };

  // ctx gives a tile its bundle's files: json(name) rejects with .status 404
  // when the file is missing, so the tile can say that no data has arrived.
  const context = (base) => ({
    url: (name) => new URL(name, base).href,
    json: async (name) => {
      const res = await fetch(new URL(name, base), { cache: "no-store", credentials: "same-origin" });
      if (!res.ok) {
        const err = new Error(name + ": HTTP " + res.status);
        err.status = res.status;
        throw err;
      }
      return res.json();
    },
  });

  const spark = (values) => {
    const nums = values.filter((v) => typeof v === "number" && isFinite(v));
    if (nums.length < 2) return null;
    const w = 100, h = 30, pad = 2;
    const min = Math.min(...nums, 0), max = Math.max(...nums);
    const span = max - min || 1;
    const pts = nums.map((v, i) => [
      (i / (nums.length - 1)) * w,
      h - pad - ((v - min) / span) * (h - 2 * pad),
    ]);
    const line = pts.map((p) => p[0].toFixed(2) + "," + p[1].toFixed(2)).join(" ");
    const svg = document.createElementNS(svgNS, "svg");
    svg.setAttribute("class", "deck-spark");
    svg.setAttribute("viewBox", "0 0 " + w + " " + h);
    svg.setAttribute("preserveAspectRatio", "none");
    const area = document.createElementNS(svgNS, "polygon");
    area.setAttribute("class", "deck-spark-area");
    area.setAttribute("points", "0," + h + " " + line + " " + w + "," + h);
    const poly = document.createElementNS(svgNS, "polyline");
    poly.setAttribute("class", "deck-spark-line");
    poly.setAttribute("points", line);
    svg.append(area, poly);
    return svg;
  };

  const render = (a, model, now) => {
    let status = ["ok", "watch", "act"].includes(model.status) ? model.status : "watch";
    const fresh = a.querySelector(".deck-tile-fresh");
    fresh.classList.remove("stale");
    fresh.textContent = "";
    const updated = model.updated ? Date.parse(model.updated) : NaN;
    if (!isNaN(updated)) {
      const age = now - updated;
      fresh.textContent = ago(age);
      fresh.title = "Data from " + new Date(updated).toLocaleString();
      if (model.staleAfter > 0 && age > model.staleAfter * 60000) {
        fresh.textContent = "stale, " + ago(age);
        fresh.classList.add("stale");
        if (status === "ok") status = "watch";
      }
    }
    a.dataset.status = status;
    a.querySelector(".deck-tile-state").textContent = label[status] + ".";
    a.querySelector(".deck-tile-headline").textContent = model.headline || "";

    const metrics = a.querySelector(".deck-tile-metrics");
    metrics.replaceChildren();
    for (const m of (model.metrics || []).slice(0, 3)) {
      const box = el("span", "deck-metric" + (["ok", "watch", "act"].includes(m.tone) ? " " + m.tone : ""));
      const value = el("b", "", String(m.value));
      value.title = String(m.value);
      box.append(value, el("span", "", m.label));
      metrics.append(box);
    }

    const sparkBox = a.querySelector(".deck-tile-spark");
    sparkBox.replaceChildren();
    const s = model.series && Array.isArray(model.series.values) ? spark(model.series.values) : null;
    if (s) sparkBox.append(s, el("small", "", model.series.label || ""));
  };

  const fail = (a, err) => {
    a.dataset.status = "error";
    a.querySelector(".deck-tile-state").textContent = label.error + ".";
    a.querySelector(".deck-tile-headline").textContent = "Could not read this page's tile.";
    a.querySelector(".deck-tile-fresh").textContent = "";
    a.querySelector(".deck-tile-metrics").replaceChildren();
    a.querySelector(".deck-tile-spark").replaceChildren();
    console.error("deck tile", a.dataset.tile, err);
  };

  // Each module is imported once per page load and called again on each
  // refresh; its data, fetched with no-store, is what changes. A changed
  // tile.js shows after a reload.
  const modules = new Map();
  // The browser caches a module that failed to load or run under its URL, so
  // a retry after a failure imports it under a new one.
  const failures = new Map();
  const timeout = () => new Promise((_, reject) =>
    setTimeout(() => reject(new Error("no answer in " + tileTimeoutMs / 1000 + " s")), tileTimeoutMs));
  const load = async (a) => {
    const src = new URL(a.dataset.tile, location.href).href;
    try {
      if (!modules.has(src)) {
        const n = failures.get(src) || 0;
        modules.set(src, import(n ? src + "?retry=" + n : src));
      }
      const run = async () => {
        const mod = await modules.get(src);
        return mod.default(context(new URL(".", src)));
      };
      const model = await Promise.race([run(), timeout()]);
      render(a, model || {}, Date.now());
    } catch (err) {
      // A failed import is retried on the next refresh, under a new URL.
      if (modules.has(src)) {
        modules.get(src).catch(() => {
          modules.delete(src);
          failures.set(src, (failures.get(src) || 0) + 1);
        });
      }
      fail(a, err);
    }
  };

  // Tiles that need a look first; otherwise keep the server's order.
  const sort = (list) => {
    const items = Array.from(list.children);
    items.forEach((li, i) => { li.dataset.order = li.dataset.order || String(i); });
    items.sort((x, y) => {
      const a = x.querySelector("[data-tile]"), b = y.querySelector("[data-tile]");
      const ra = rank[a ? a.dataset.status : "ok"] ?? 3, rb = rank[b ? b.dataset.status : "ok"] ?? 3;
      return ra - rb || Number(x.dataset.order) - Number(y.dataset.order);
    });
    list.append(...items);
  };

  // One refresh at a time, so an older answer never overwrites a newer one.
  let lastRun = 0;
  let running = null;
  const refresh = () => {
    if (running) return running;
    lastRun = Date.now();
    const tiles = Array.from(document.querySelectorAll("a[data-tile]"));
    running = Promise.all(tiles.map(load)).then(() => {
      for (const list of document.querySelectorAll(".deck-tiles")) sort(list);
    }).finally(() => { running = null; });
    return running;
  };

  const start = () => {
    refresh();
    setInterval(() => { if (!document.hidden) refresh(); }, refreshMs);
    // Coming back to the tab refreshes tiles older than a minute.
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && Date.now() - lastRun > 60000) refresh();
    });
  };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
