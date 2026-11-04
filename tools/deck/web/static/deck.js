// deck: filters, count cards, sorting, "new since your last visit", actions
// and auto-refresh. Server-rendered HTML arrives already escaped; this script
// only moves nodes and sets text, never assigns untrusted text as markup.
"use strict";

(function () {
  const base = document.body.dataset.base || "/";
  const content = document.getElementById("content");
  const notice = document.getElementById("notice");
  const text = document.getElementById("f-text");
  const project = document.getElementById("f-project");
  const prefix = document.getElementById("f-prefix");
  const sortBy = document.getElementById("f-sort");
  const newOnly = document.getElementById("f-new");

  // Storage may be denied; the page then works without a visit marker.
  // Each view keeps its own marker, so a visit to one does not mark the
  // other as seen.
  const visitKey = document.body.dataset.visitKey || "deck.lastVisit";
  const stored = () => {
    try {
      return Date.parse(localStorage.getItem(visitKey) || "") || 0;
    } catch (e) {
      return 0;
    }
  };
  const lastVisit = stored();
  // A visit marks as seen only what the shown facts cover: changes after
  // they were read are still new next time. Another tab may have seen
  // more, so the mark never moves back.
  // Only content shown while the tab was visible counts as seen.
  let seen = 0;
  const noteSeen = () => {
    if (document.visibilityState !== "visible") return;
    const overview = content.querySelector("section.overview");
    seen = Date.parse((overview && overview.dataset.seen) || "") || seen;
  };
  const remember = () => {
    if (!seen || seen <= stored()) return;
    try {
      localStorage.setItem(visitKey, new Date(seen).toISOString());
    } catch (e) {
      // storage denied
    }
  };
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") remember();
    else noteSeen();
  });
  window.addEventListener("pagehide", remember);

  function rows() {
    return Array.from(content.querySelectorAll("li.row"));
  }

  function fillSelect(select, values) {
    const current = select.value;
    while (select.options.length > 1) select.remove(1);
    Array.from(new Set(values)).filter(Boolean).sort().forEach((v) => {
      const o = document.createElement("option");
      o.value = v;
      o.textContent = v;
      select.appendChild(o);
    });
    select.value = values.includes(current) ? current : "";
  }

  function markNew() {
    rows().forEach((r) => {
      const t = Date.parse(r.dataset.updated || "");
      r.classList.toggle("new", lastVisit > 0 && t > lastVisit);
    });
  }

  function sortRows() {
    content.querySelectorAll("ul.rows").forEach((ul) => {
      const items = Array.from(ul.children);
      items.forEach((li, i) => {
        if (li.dataset.order === undefined) li.dataset.order = String(i);
      });
      const key = sortBy.value;
      items.sort((a, b) => {
        if (key === "idle") return (Number(b.dataset.idle) || 0) - (Number(a.dataset.idle) || 0);
        if (key === "updated") return (Date.parse(b.dataset.updated) || 0) - (Date.parse(a.dataset.updated) || 0);
        return Number(a.dataset.order) - Number(b.dataset.order);
      });
      items.forEach((li) => ul.appendChild(li));
    });
  }

  // A count card filters to the rows it counts. The active card lives in the
  // URL fragment, so it survives a reload and can be linked to.
  const cardFromURL = () => (/^#card=([a-z-]+)$/.exec(location.hash) || [])[1] || "";
  let activeCard = cardFromURL();
  // Sections opened to show the active card's rows, closed again after it,
  // and sections the reader closed meanwhile, which stay closed until then.
  let openedForCard = new Set();
  let closedForCard = new Set();

  function cardButtons() {
    return Array.from(content.querySelectorAll("button[data-card]"));
  }

  const inCard = (row, card) => (row.dataset.cards || "").split(" ").includes(card);

  function setCard(card) {
    activeCard = card;
    const url = card ? "#card=" + card : location.pathname + location.search;
    history.replaceState(null, "", url);
    content.querySelectorAll("details.sub[data-key]").forEach((d) => {
      if (openedForCard.has(d.dataset.key)) d.open = false;
    });
    openedForCard = new Set();
    closedForCard = new Set();
    applyFilters();
  }

  // Updates the cards: each counts the rows that pass the page's filters,
  // whichever card is active, so the others still say what they would show.
  function updateCards(matching) {
    const buttons = cardButtons();
    buttons.forEach((b) => {
      const n = matching.filter((r) => inCard(r, b.dataset.card)).length;
      b.querySelector("b").textContent = String(n);
      b.setAttribute("aria-pressed", String(b.dataset.card === activeCard));
      const empty = n === 0 && b.dataset.card !== activeCard;
      b.disabled = empty;
      // A card shown only while it counts something stays while it is active,
      // so it can be cleared and a refresh does not drop the filter.
      const holder = b.closest("[data-hide-empty]");
      if (holder) holder.hidden = empty;
    });
    // The group command's rule: progress, or 0 without one, over unsettled
    // threads, here the ones on screen.
    const average = content.querySelector("[data-average]");
    if (average) {
      const shown = matching.filter((r) => !r.hidden && inCard(r, "unsettled"));
      average.hidden = shown.length === 0;
      const sum = shown.reduce((total, r) => total + (Number(r.dataset.progress) || 0), 0);
      average.querySelector("b").textContent = Math.round(sum / (shown.length || 1)) + "%";
    }
  }

  // Hides lists left empty and counts what stays, so headings match the rows.
  function updateGroups() {
    content.querySelectorAll("ul.rows").forEach((list) => {
      const shown = Array.from(list.children).filter((li) => !li.hidden).length;
      const holder = list.closest("details.sub");
      const previous = list.previousElementSibling;
      const heading = holder ? holder.querySelector("summary h3") : previous && previous.matches("h3") ? previous : null;
      if (heading) {
        const n = heading.querySelector(".n");
        if (n) n.textContent = String(shown);
      }
      if (holder) {
        holder.hidden = shown === 0;
        if (activeCard && shown > 0 && !holder.open && !closedForCard.has(holder.dataset.key)) {
          holder.open = true;
          openedForCard.add(holder.dataset.key);
        }
      } else if (heading) {
        heading.hidden = shown === 0;
      }
    });
    content.querySelectorAll("section.group[data-section]").forEach((s) => {
      const list = Array.from(s.querySelectorAll("li.row"));
      // A section without rows, such as the unblockers, says nothing about
      // the active card's rows.
      if (list.length === 0) {
        s.hidden = Boolean(activeCard);
        return;
      }
      const shown = list.filter((r) => !r.hidden).length;
      s.hidden = shown === 0;
      const n = s.querySelector("h2 .n");
      if (n) n.textContent = String(shown);
    });
  }

  function applyFilters() {
    if (activeCard && !cardButtons().some((b) => b.dataset.card === activeCard)) {
      setCard("");
      return;
    }
    const q = text.value.trim().toLowerCase();
    const matching = rows().filter((r) => {
      const hay = [r.dataset.title, r.dataset.project, r.dataset.next].join(" ").toLowerCase();
      const match =
        (!q || hay.includes(q)) &&
        (!project.value || r.dataset.project === project.value) &&
        (!prefix.value || r.dataset.prefix === prefix.value) &&
        (!newOnly.checked || r.classList.contains("new"));
      r.hidden = !match || Boolean(activeCard && !inCard(r, activeCard));
      return match;
    });
    updateCards(matching);
    updateGroups();
  }

  function refreshControls() {
    const all = rows();
    fillSelect(project, all.map((r) => r.dataset.project));
    fillSelect(prefix, all.map((r) => r.dataset.prefix));
    markNew();
    sortRows();
    applyFilters();
    noteSeen();
  }

  function showNotice(message) {
    notice.textContent = message;
    notice.hidden = !message;
  }

  // A reload asked for while one runs follows it, so an action's result
  // is not lost to a fetch that started before the action.
  let loading = false;
  let again = false;
  async function reload() {
    if (loading) {
      again = true;
      return;
    }
    loading = true;
    try {
      const res = await fetch(base + "fragment", { credentials: "same-origin" });
      if (!res.ok) throw new Error("refresh failed: " + res.status);
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      const open = new Set(
        Array.from(content.querySelectorAll("details[open]")).map((d) => d.dataset.key)
      );
      // Read selections just before replacing the fragment, so a choice
      // made during a slow fetch survives too.
      const roles = new Map(rows().map((row) => [
        row.dataset.key, row.querySelector('select[name="role"]')?.value,
      ]));
      const focused = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.card : "";
      content.replaceChildren(...Array.from(doc.body.childNodes).map((n) => document.importNode(n, true)));
      content.querySelectorAll("details[data-key]").forEach((d) => {
        if (open.has(d.dataset.key)) d.open = true;
      });
      rows().forEach((row) => {
        const select = row.querySelector('select[name="role"]');
        const role = roles.get(row.dataset.key);
        if (select && Array.from(select.options).some((o) => o.value === role)) {
          select.value = role;
        }
      });
      refreshControls();
      if (focused) {
        const again = cardButtons().find((b) => b.dataset.card === focused);
        if (again) again.focus();
      }
      showNotice("");
    } catch (e) {
      showNotice(String(e.message || e) + "; showing the last loaded data.");
    } finally {
      loading = false;
    }
    if (again) {
      again = false;
      await reload();
    }
  }

  content.addEventListener("click", (ev) => {
    const summary = ev.target.closest("details.sub > summary");
    if (summary && activeCard) {
      const key = summary.parentElement.dataset.key;
      if (summary.parentElement.open) closedForCard.add(key);
      else closedForCard.delete(key);
    }
    const button = ev.target.closest("button[data-card]");
    if (!button) return;
    setCard(button.dataset.card === activeCard ? "" : button.dataset.card);
  });
  window.addEventListener("hashchange", () => setCard(cardFromURL()));

  content.addEventListener("submit", async (ev) => {
    const form = ev.target;
    if (!form.matches("form[data-action]")) return;
    ev.preventDefault();
    const button = form.querySelector("button");
    if (button) button.disabled = true;
    try {
      const res = await fetch(form.action, {
        method: "POST",
        credentials: "same-origin",
        headers: { "X-Deck-Fetch": "1" },
        body: new URLSearchParams(new FormData(form)),
      });
      if (!res.ok) throw new Error((await res.text()).trim() || "request failed: " + res.status);
      await reload();
    } catch (e) {
      showNotice(String(e.message || e));
      if (button) button.disabled = false;
    }
  });

  document.getElementById("filters").addEventListener("submit", (ev) => ev.preventDefault());
  [text, project, prefix, newOnly].forEach((el) => el.addEventListener("input", applyFilters));
  sortBy.addEventListener("change", sortRows);

  // Print every row expanded, then close again the ones that were closed.
  // They are found by key, since a reload during printing replaces the nodes.
  let closedForPrint = new Set();
  window.addEventListener("beforeprint", () => {
    content.querySelectorAll("details[data-key]:not([open])").forEach((d) => {
      closedForPrint.add(d.dataset.key);
      d.open = true;
    });
  });
  window.addEventListener("afterprint", () => {
    content.querySelectorAll("details[data-key]").forEach((d) => {
      if (closedForPrint.has(d.dataset.key)) d.open = false;
    });
    closedForPrint = new Set();
  });

  refreshControls();
  setInterval(() => {
    if (document.visibilityState === "visible") reload();
  }, 60000);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") reload();
  });
})();
