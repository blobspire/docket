// docket queue: keyboard control, one-click actions, live refresh.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const toast = (msg) => {
    const t = $(".toast");
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(t._h);
    t._h = setTimeout(() => (t.hidden = true), 3500);
  };

  // ---- selection ----------------------------------------------------------
  const rows = () => $$(".row");
  let sel = 0;
  try { sel = +sessionStorage.getItem("docket.sel") || 0; } catch (_) {}
  const detail = $("[data-detail]");
  const current = () => detail || rows()[sel];
  const select = (i) => {
    const rs = rows();
    if (!rs.length) return;
    sel = Math.max(0, Math.min(i, rs.length - 1));
    rs.forEach((r, j) => r.classList.toggle("sel", j === sel));
    rs[sel].scrollIntoView({ block: "nearest" });
    try { sessionStorage.setItem("docket.sel", sel); } catch (_) {}
  };
  if (!detail) select(sel);

  // ---- actions ------------------------------------------------------------
  async function act(el, action, extra = {}) {
    const id = el.dataset.id;
    const body = new URLSearchParams({ action, ...extra });
    const res = await fetch(`/item/${encodeURIComponent(id)}/act`, {
      method: "POST", body, headers: { "X-Requested-With": "fetch" },
    });
    if (!res.ok) {
      const j = await res.json().catch(() => ({}));
      toast(j.error || `Couldn't ${action}`);
      return;
    }
    try { sessionStorage.setItem("docket.last", id); } catch (_) {}
    const done = { approve: "Approved, sent to the agent", decline: "Declined, sent to the agent", choose: "Sent to the agent",
      done: "Marked done", dismiss: "Dismissed", snooze: "Snoozed", promote: "Moved to the queue", undo: "Undone" }[action];
    toast(done || "Done");
    setTimeout(() => (detail && action !== "undo" && action !== "promote" ? (location.href = "/") : location.reload()), 400);
  }

  document.addEventListener("click", (e) => {
    const b = e.target.closest("button[data-act]");
    if (b) {
      e.preventDefault();
      e.stopPropagation();
      const el = b.closest("[data-id]");
      const extra = {};
      if (b.dataset.option !== undefined) extra.option = b.dataset.option;
      if (b.dataset.act === "snooze") extra.until = e.shiftKey ? "tomorrow" : "hour";
      act(el, b.dataset.act, extra);
      return;
    }
    if (e.target.closest("[data-help]")) return toggleHelp();
    if (e.target.closest("[data-collect]")) {
      fetch("/api/collect", { method: "POST", headers: { "X-Requested-With": "fetch" } }).then(() => toast("Collecting…"));
      return;
    }
    const row = e.target.closest(".row");
    if (row && !e.target.closest("a, button, summary")) location.href = row.dataset.href;
  });

  const toggleHelp = () => { const h = $(".help"); h.hidden = !h.hidden; };
  let g = false;
  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
    if (e.key === "Escape") {
      $(".help").hidden = true;
      if (typing) document.activeElement.blur();
      return;
    }
    if (typing) {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) document.activeElement.form?.requestSubmit();
      return;
    }
    const k = e.key;
    if (g) {
      g = false;
      const to = { q: "/", c: "/checklist", l: "/leads", e: "/evidence", b: "/brain", d: "/done", h: "/health", s: "/search" }[k];
      if (to) location.href = to;
      return;
    }
    if (k === "g") { g = true; setTimeout(() => (g = false), 800); return; }
    if (k === "?") return toggleHelp();
    if (k === "/") { e.preventDefault(); const q = $("#q"); return q ? q.focus() : (location.href = "/search"); }
    if (!detail) {
      if (k === "j" || k === "ArrowDown") { e.preventDefault(); return select(sel + 1); }
      if (k === "k" || k === "ArrowUp") { e.preventDefault(); return select(sel - 1); }
      if (k === "Enter" && current()) return (location.href = current().dataset.href);
    }
    const el = current();
    if (!el) return;
    if (k === "o" && el.dataset.session) return window.open(el.dataset.session, "_blank", "noopener");
    if (k === "a") {
      e.preventDefault();
      const t = $("#answer-text");
      return t ? t.focus() : (location.href = el.dataset.href + "#answer");
    }
    if (k === "u") {
      let last = null;
      try { last = sessionStorage.getItem("docket.last"); } catch (_) {}
      if (last) return act({ dataset: { id: last } }, "undo");
      return toast("Nothing to undo");
    }
    if (k === "s" || k === "S") return act(el, "snooze", { until: k === "S" ? "tomorrow" : "hour" });
    const btn = $$("button[data-key]", el).find((b) => b.dataset.key === k);
    if (btn) btn.click();
  });

  if (location.hash === "#answer") $("#answer-text")?.focus();

  // ---- live refresh ---------------------------------------------------------
  // Reload the list when something changes, unless the user is typing.
  const v0 = document.body.dataset.version;
  setInterval(async () => {
    if (document.hidden || /^(INPUT|TEXTAREA)$/.test(document.activeElement.tagName)) return;
    try {
      const r = await fetch("/api/version");
      const { v } = await r.json();
      if (v !== v0 && !detail) location.reload();
    } catch (_) {}
  }, 5000);
})();
