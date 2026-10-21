// deck: filters, sorting, "new since your last visit", actions and
// auto-refresh. Server-rendered HTML arrives already escaped; this script
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
  const visitKey = "deck.lastVisit";
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

  function applyFilters() {
    const q = text.value.trim().toLowerCase();
    rows().forEach((r) => {
      const hay = [r.dataset.title, r.dataset.project, r.dataset.next].join(" ").toLowerCase();
      const show =
        (!q || hay.includes(q)) &&
        (!project.value || r.dataset.project === project.value) &&
        (!prefix.value || r.dataset.prefix === prefix.value) &&
        (!newOnly.checked || r.classList.contains("new"));
      r.hidden = !show;
    });
    content.querySelectorAll("section.group[data-section]").forEach((s) => {
      const list = s.querySelectorAll("li.row");
      if (list.length === 0) return;
      s.hidden = !Array.from(list).some((r) => !r.hidden);
    });
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

  refreshControls();
  setInterval(() => {
    if (document.visibilityState === "visible") reload();
  }, 60000);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") reload();
  });
})();
