// docket dashboard: expand in place, keyboard control, one-click actions,
// and a refresh that never yanks a row out from under you.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const store = {
    get: (k) => { try { return sessionStorage.getItem(k); } catch (_) { return null; } },
    set: (k, v) => { try { sessionStorage.setItem(k, v); } catch (_) {} },
  };
  const toast = (msg) => {
    const t = $(".toast");
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(t._h);
    t._h = setTimeout(() => (t.hidden = true), 3500);
  };
  const single = $("[data-detail]"); // an item's own page

  // ---- rows: selection and expansion ------------------------------------------
  // Rows you can see. A collapsed "Snoozed" section may hide its rows without
  // taking them out of layout (content-visibility), so check for it too.
  const rows = () => $$(".row[data-id]").filter((r) => r.offsetParent !== null && !r.closest("details:not([open])"));
  // Nothing is selected until you move with j/k or click a row on this page:
  // a one-key action must never land on a row you can't see is selected.
  let sel = -1;
  const kbd = () => document.body.classList.add("kbd");
  const select = (i, scroll = true) => {
    const rs = rows();
    if (!rs.length) return;
    sel = Math.max(0, Math.min(i, rs.length - 1));
    rs.forEach((r, j) => r.classList.toggle("sel", j === sel));
    if (scroll) rs[sel].scrollIntoView({ block: "nearest" });
  };
  const current = () => (sel >= 0 && rows()[sel]) || (single && $(".row[data-id]")) || null;
  const setOpen = (row, open) => {
    const d = $(".detail", row);
    if (!d) return;
    d.hidden = !open;
    row.classList.toggle("open", open);
    $(".row-head", row)?.setAttribute("aria-expanded", open ? "true" : "false");
    const nf = $(".noise-form", row);
    if (!open && nf) nf.hidden = true;
  };
  const toggle = (row) => setOpen(row, !row.classList.contains("open"));
  const anyOpen = () => !single && rows().some((r) => r.classList.contains("open"));

  // ---- actions --------------------------------------------------------------------
  // The page this one should show after an action, without a stale ?err=.
  const fresh = (href) => {
    const u = new URL(href, location.origin);
    u.searchParams.delete("err");
    return u.pathname + u.search;
  };
  let inFlight = false; // one action at a time: a double click sends it once
  async function act(id, action, extra = {}) {
    if (inFlight) return false;
    inFlight = true;
    const body = new URLSearchParams({ action, ...extra });
    let res;
    try {
      res = await fetch(`/item/${encodeURIComponent(id)}/act`, { method: "POST", body, headers: { "X-Requested-With": "fetch" } });
    } catch (_) {
      inFlight = false;
      toast(`Couldn't ${action}: the server didn't answer`);
      return false;
    }
    if (!res.ok) {
      inFlight = false;
      const j = await res.json().catch(() => ({}));
      toast(j.error || `Couldn't ${action}`);
      return false;
    }
    store.set("docket.last", id);
    const msg = { approve: "Approved", decline: "Declined", choose: "Sent to the agent", answer: "Sent to the agent",
      done: "Marked done", dismiss: "Dismissed", noise: "Closed. The agents will hear why.", snooze: "Snoozed",
      reclassify: "Moved", undo: "Undone", reopen: "Reopened" }[action];
    toast(msg || "Done");
    // From an item's own page, go back to the dashboard it came from (fresh,
    // not a cached copy that still shows the item).
    const back = () => {
      try {
        const r = new URL(document.referrer);
        if (r.origin === location.origin && !r.pathname.startsWith("/item/")) return (location.href = fresh(r.href));
      } catch (_) {}
      location.href = "/";
    };
    const stay = ["undo", "reclassify", "reopen"].includes(action);
    setTimeout(() => (single && !stay ? back() : (location.href = fresh(location.href))), 450);
    return true;
  }

  const openNoise = (row) => {
    const f = $(".noise-form", row);
    if (!f) return (location.href = row.dataset.href + "#noise"); // e.g. the Only-you list
    setOpen(row, true);
    f.hidden = false;
    $("textarea", f).focus();
  };

  document.addEventListener("click", (e) => {
    const b = e.target.closest("button[data-act]");
    if (b) {
      e.preventDefault();
      e.stopPropagation();
      const extra = {};
      if (b.dataset.option !== undefined) extra.option = b.dataset.option;
      if (b.dataset.act === "snooze") extra.until = e.shiftKey ? "tomorrow" : "hour";
      act(b.closest("[data-id]").dataset.id, b.dataset.act, extra);
      return;
    }
    if (e.target.closest("[data-noise]")) {
      e.preventDefault();
      return openNoise(e.target.closest(".row"));
    }
    if (e.target.closest("[data-noise-cancel]")) {
      e.target.closest(".noise-form").hidden = true;
      return;
    }
    if (e.target.closest("[data-edit-project]")) {
      const f = $(".project-edit");
      if (f) { f.hidden = !f.hidden; if (!f.hidden) $("input", f).focus(); }
      return;
    }
    if (e.target.closest("[data-refresh]")) return location.reload();
    if (e.target.closest("[data-help]")) return toggleHelp();
    if (e.target.closest("[data-help-close]") || e.target.classList.contains("help")) {
      $(".help").hidden = true;
      return;
    }
    if (e.target.closest("[data-collect]")) {
      fetch("/api/collect", { method: "POST", headers: { "X-Requested-With": "fetch" } }).then(() => toast("Collecting…"));
      return;
    }
    // Clicking a row's head (not its title link) expands it in place.
    const head = e.target.closest(".row-head");
    if (head && !single && !e.target.closest("a, button, select, input, textarea")) {
      const row = head.closest(".row");
      const i = rows().indexOf(row);
      if (i >= 0 && $(".detail", row)) {
        kbd();
        select(i, false);
        toggle(row);
      }
    }
  });

  document.addEventListener("submit", (e) => {
    const f = e.target.closest(".noise-form");
    if (!f) return;
    e.preventDefault();
    act(f.closest("[data-id]").dataset.id, "noise", { text: $("textarea", f).value.trim() });
  });

  // Moving an item: a pick with the mouse acts at once; from the keyboard
  // (where typing a letter changes a closed select), only Enter does.
  const reclassify = (s) => {
    const field = s.dataset.reclassify;
    let value = s.value;
    if (value === "__new") {
      value = (prompt("New project name (letters, digits, dashes):") || "").trim();
      if (!value) { location.href = fresh(location.href); return; }
    }
    act(s.closest("[data-id]").dataset.id, "reclassify", { [field]: value });
  };
  document.addEventListener("pointerdown", (e) => {
    const s = e.target.closest("select[data-reclassify]");
    if (s) s.dataset.pointer = "1";
  });
  document.addEventListener("change", (e) => {
    const s = e.target.closest("select[data-reclassify]");
    if (!s) return;
    if (s.dataset.pointer) { delete s.dataset.pointer; return reclassify(s); }
    toast("Press Enter to move it");
  });

  // ---- keyboard ---------------------------------------------------------------------
  const toggleHelp = () => { const h = $(".help"); h.hidden = !h.hidden; };
  const projectLinks = () => $$(".side a");
  let g = false;
  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) {
      // Cmd/Ctrl+Enter sends the form you're typing in.
      if (e.key === "Enter" && /^(TEXTAREA)$/.test(document.activeElement.tagName)) document.activeElement.form?.requestSubmit();
      return;
    }
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
    if (e.key === "Escape") {
      if (!$(".help").hidden) return ($(".help").hidden = true);
      if (typing) return document.activeElement.blur();
      const r = current();
      if (r && r.classList.contains("open") && !single) setOpen(r, false);
      return;
    }
    if (typing) {
      // Enter sends a "not worth my time" note; Shift+Enter is a new line.
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing && document.activeElement.closest(".noise-form")) {
        e.preventDefault();
        document.activeElement.form.requestSubmit();
      }
      // Enter on a project or urgency select moves the item.
      if (e.key === "Enter" && document.activeElement.matches("select[data-reclassify]")) {
        e.preventDefault();
        reclassify(document.activeElement);
      }
      return;
    }
    const k = e.key;
    // A focused link or button keeps its own Enter and Space.
    const a = document.activeElement;
    if ((k === "Enter" || k === " ") && a && a !== document.body && !a.classList.contains("row-head")) return;
    // While the help panel is open, only ? (and Esc, above) do anything.
    if (!$(".help").hidden && k !== "?") return;
    if (g) {
      g = false;
      const to = { d: "/", q: "/", c: "/checklist", l: "/leads", e: "/evidence", b: "/brain", h: "/health", s: "/search" }[k];
      if (to) location.href = to;
      return;
    }
    if (k === "g") { g = true; setTimeout(() => (g = false), 800); return; }
    if (k === "?") return toggleHelp();
    if (k === "/") { e.preventDefault(); const q = $("#q"); return q ? q.focus() : (location.href = "/search"); }
    if (k === "[" || k === "]") {
      const ls = projectLinks();
      if (!ls.length) return;
      let i = ls.findIndex((a) => a.classList.contains("on"));
      i = k === "]" ? (i + 1) % ls.length : (i <= 0 ? ls.length - 1 : i - 1);
      location.href = ls[i].href;
      return;
    }
    if (!single) {
      if (k === "j" || k === "ArrowDown") { e.preventDefault(); kbd(); return select(sel + 1); }
      if (k === "k" || k === "ArrowUp") { e.preventDefault(); kbd(); return select(sel - 1); }
      if ((k === "Enter" || k === " ") && current()) { e.preventDefault(); return toggle(current()); }
    }
    const el = current();
    // u undoes the last action, selected row or not (on the Done page, the
    // selected row's own Undo).
    if (k === "u") {
      const own = el && $("button[data-act=undo]", el);
      if (own) return own.click();
      const id = store.get("docket.last");
      return id ? act(id, "undo") : toast("Nothing to undo");
    }
    if (!el) return;
    if (k === "o" && el.dataset.session) return window.open(el.dataset.session, "_blank", "noopener");
    if (k === "a") {
      e.preventDefault();
      setOpen(el, true);
      const t = $(".answer-form textarea", el);
      return t ? t.focus() : toast(el.dataset.closed ? "This item is closed" : "This item has no agent session to answer");
    }
    if (k === "w") { e.preventDefault(); return $("[data-noise]", el) && openNoise(el); }
    // Only a row that offers Snooze can be snoozed (never a closed item).
    if ((k === "s" || k === "S") && $("button[data-act=snooze]", el)) return act(el.dataset.id, "snooze", { until: k === "S" ? "tomorrow" : "hour" });
    const btn = $$("button[data-key]", el).find((b) => b.dataset.key === k);
    if (btn) btn.click();
  });

  // "Not worth my time" from a list without the note box opens it here.
  if (single && location.hash === "#noise") { const r = $(".row[data-id]"); if (r) openNoise(r); }
  // A dashboard restored from the back-forward cache may be stale.
  window.addEventListener("pageshow", (e) => { if (e.persisted && !single) location.reload(); });

  // Answer forms post normally; remember where to come back to.
  $$(".answer-form input[name=back]").forEach((i) => { if (!i.value) i.value = location.pathname + location.search; });

  // ---- live refresh ---------------------------------------------------------------
  // Reload when the queue changes, but never while you're reading an expanded
  // item or typing: show a quiet "refresh" pill instead.
  const v0 = document.body.dataset.version;
  setInterval(async () => {
    if (document.hidden) return;
    try {
      const r = await fetch("/api/version");
      const { v } = await r.json();
      if (v === v0) return;
      const busy = single || anyOpen() || /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
      if (busy) $(".updated").hidden = false;
      else location.reload();
    } catch (_) {}
  }, 5000);
})();
