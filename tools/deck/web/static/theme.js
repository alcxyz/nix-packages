// Light, dark or system colour scheme, shared by deck's views, its library
// and page bundles (ADR-0013). Load it in <head> without defer, so the
// choice applies before the first paint. It sets data-theme="light" or
// "dark" on <html>; pages put their dark rules under
// @media screen { :root[data-theme="dark"] ... }, so print stays light.
// The choice is kept per browser; without one the page follows the system.
"use strict";

(function () {
  const key = "deck.theme";
  const root = document.documentElement;
  const system = window.matchMedia("(prefers-color-scheme: dark)");
  const choices = [
    ["light", "☀", "Light"],
    ["dark", "☾", "Dark"],
    ["system", "◐", "System"],
  ];
  const buttons = [];

  // Storage may be denied; the page then follows the system.
  const read = () => {
    try {
      const v = localStorage.getItem(key);
      return v === "light" || v === "dark" ? v : "system";
    } catch (e) {
      return "system";
    }
  };
  let choice = read();

  const apply = () => {
    root.dataset.theme = choice === "system" ? (system.matches ? "dark" : "light") : choice;
    for (const b of buttons) b.setAttribute("aria-pressed", String(b.value === choice));
  };
  apply();
  system.addEventListener("change", apply);
  // Another tab or page of this origin changed the choice.
  window.addEventListener("storage", (e) => {
    if (e.key === key || e.key === null) {
      choice = read();
      apply();
    }
  });

  const choose = (v) => {
    choice = v;
    try {
      if (v === "system") localStorage.removeItem(key);
      else localStorage.setItem(key, v);
    } catch (e) {
      // Not persisted; the choice still holds for this page.
    }
    apply();
  };

  // The toggle goes into a [data-deck-theme] element if the page has one,
  // otherwise it floats in the bottom right corner.
  const mount = () => {
    const group = document.createElement("div");
    group.className = "deck-theme";
    group.setAttribute("role", "group");
    group.setAttribute("aria-label", "Colour scheme");
    for (const [v, symbol, label] of choices) {
      const b = document.createElement("button");
      b.type = "button";
      b.value = v;
      b.textContent = symbol;
      b.title = label;
      b.setAttribute("aria-label", label);
      b.addEventListener("click", () => choose(v));
      buttons.push(b);
      group.append(b);
    }
    const slot = document.querySelector("[data-deck-theme]");
    if (slot) {
      slot.append(group);
    } else {
      group.classList.add("deck-theme-float");
      document.body.append(group);
    }
    apply();
  };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", mount);
  else mount();
})();
