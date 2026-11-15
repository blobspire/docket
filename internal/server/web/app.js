// docket dashboard: expand in place, keyboard control, one-click actions,
// and a refresh that never yanks a row out from under you.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const store = {
    get: (k) => { try { return sessionStorage.getItem(k); } catch (_) { return null; } },
    set: (k, v) => { try { sessionStorage.setItem(k, v); } catch (_) {} },
  };
  // A toast, with an Undo button when the action it reports can be undone.
  const toast = (msg, undoId) => {
    const t = $(".toast");
    t.replaceChildren(document.createTextNode(msg));
    if (undoId) {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = "Undo";
      b.addEventListener("click", () => { t.hidden = true; act(undoId, "undo"); });
      t.append(b);
    }
    t.hidden = false;
    clearTimeout(t._h);
    t._h = setTimeout(() => (t.hidden = true), undoId ? 6000 : 3500);
  };
  // A toast for the page about to load (an item's page goes back to the list).
  const carry = (msg, undoId) => store.set("docket.toast", JSON.stringify({ msg, undoId, at: Date.now() }));
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
    $$(".row.sel").forEach((r) => r.classList.remove("sel")); // hidden ones too
    rs[sel].classList.add("sel");
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
    if (nf) nf.hidden = true;
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
  // Actions on different items can overlap (clearing a list quickly); one
  // item takes one action at a time, so a double click sends it once.
  const pending = new Set();
  // Items acted on from this page: id -> the action's sequence number
  // (Infinity while it's on its way). They stay faded until a refresh that
  // started after the action shows its result (gone, answered, moved).
  const leaving = new Map();
  let seq = 0;
  const CLOSES = ["done", "dismiss", "noise", "snooze"];
  // A closed item -> the row after it when it closed: where the selection
  // goes next, so d, d, d works down the list.
  const follow = new Map();
  // Actions in the order you took them: only the latest sets what u undoes
  // and owns the toast, whichever answer arrives last.
  let order = 0, latest = 0, navigating = false;
  const fade = () => $$(".row[data-id]").forEach((r) => r.classList.toggle("leaving", leaving.has(r.dataset.id)));
  const verb = { approve: "approve it", decline: "decline it", choose: "send that", answer: "send that", done: "mark it done",
    dismiss: "dismiss it", noise: "close it", snooze: "snooze it", reclassify: "move it", undo: "undo that", reopen: "reopen it" };

  async function act(id, action, extra = {}) {
    // One action per item at a time, and nothing more on one already on its
    // way out: a quick second key is dropped, not turned into an error.
    if (navigating || pending.has(id) || (leaving.has(id) && action !== "undo")) return false;
    pending.add(id);
    const mine = ++order;
    // The row is held (faded, deaf to keys) until a refresh shows what the
    // action did: a second y before then would answer twice. An undo is the
    // one action that brings a row back rather than settling it.
    const holds = !single && action !== "undo";
    if (holds) {
      if (CLOSES.includes(action)) {
        const rs = rows(), i = rs.findIndex((r) => r.dataset.id === id);
        if (i >= 0 && rs[i + 1]) follow.set(id, rs[i + 1].dataset.id);
      }
      leaving.set(id, Infinity);
      fade();
    }
    let err = "";
    try {
      const body = new URLSearchParams({ action, ...extra });
      const res = await fetch(`/item/${encodeURIComponent(id)}/act`, { method: "POST", body, headers: { "X-Requested-With": "fetch" } });
      if (!res.ok) err = (await res.json().catch(() => ({}))).error || `Couldn't ${verb[action] || action}`;
    } catch (_) {
      err = `Couldn't ${verb[action] || action}: the server didn't answer`;
    }
    pending.delete(id);
    const last = mine >= latest;
    if (last) latest = mine;
    if (err) {
      if (holds) { leaving.delete(id); follow.delete(id); fade(); }
      toast(err); // a failure always says so, even behind a later success
      if (!single) refresh(); // it may have changed elsewhere
      return false;
    }
    if (holds) leaving.set(id, ++seq);
    if (action === "undo") { leaving.delete(id); follow.delete(id); fade(); } // open again
    // What u (and the toast's Undo) undoes next; an undo itself leaves
    // nothing more to undo. Reopening a human close already undid it; another
    // u would reach past it to whatever came before (an answer, a move).
    const undoId = action === "undo" || action === "reopen" ? "" : id;
    if (last) store.set("docket.last", undoId);
    const msg = { approve: "Approved", decline: "Declined", choose: "Sent to the agent", answer: "Sent to the agent",
      done: "Marked done", dismiss: "Dismissed", noise: "Closed. The agents will hear why.", snooze: "Snoozed",
      reclassify: "Moved", undo: "Undone", reopen: "Reopened" }[action] || "Done";
    if (single) {
      // From an item's own page, go back to the dashboard it came from (fresh,
      // not a cached copy that still shows the item); stay for an undo or move.
      carry(msg, undoId);
      navigating = true; // nothing more from this page until the next one loads
      if (["undo", "reclassify", "reopen"].includes(action)) return (location.href = fresh(location.href)), true;
      try {
        const r = new URL(document.referrer);
        if (r.origin === location.origin && !r.pathname.startsWith("/item/")) return (location.href = fresh(r.href)), true;
      } catch (_) {}
      location.href = "/";
      return true;
    }
    if (last) toast(msg, undoId);
    // The acted-on row's own controls (a sent note, a picked project) mustn't
    // count as typing or a draft and hold the refresh back.
    const row = $(`.row[data-id="${CSS.escape(id)}"]`);
    if (row) {
      if (row.contains(document.activeElement)) document.activeElement.blur();
      // A closed row's note box goes with it (sent, or no longer needed).
      if (CLOSES.includes(action)) $$(".noise-form", row).forEach((f) => { f.hidden = true; $("textarea", f).value = ""; });
    }
    refresh();
    return true;
  }

  // ---- refresh in place -------------------------------------------------------------
  // Swap in a fresh copy of the page instead of reloading it: the scroll
  // position, an expanded row and the selection all survive, so a closed row
  // simply goes and the next one slides up. Never while you're typing or a
  // note is half-written; the "queue changed" pill says so instead.
  let v0 = document.body.dataset.version;
  const typing = () => /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement.tagName);
  // Something half-done: an unsent answer (even in a folded row), an open
  // note box (empty or not: you opened it to write), or the project form.
  // A cancelled note is abandoned, not a draft.
  const drafting = () => $$("textarea").some((t) => t.value.trim() && !t.closest(".noise-form")) ||
    !!$(".noise-form:not([hidden]), .project-edit:not([hidden])");
  const swap = (doc, url) => {
    if (typing() || drafting()) return false;
    const where = [".shell", "main.page"].find((s) => $(s) && $(s, doc));
    if (!where) return false;
    const openIds = $$(".row.open[data-id]").map((r) => r.dataset.id);
    const selId = sel >= 0 ? rows()[sel]?.dataset.id : null;
    const side = $(".side"), sideAt = side && [side.scrollLeft, side.scrollTop];
    const snoozedOpen = $("details.snoozed")?.open;
    $(where).replaceWith($(where, doc));
    const top = $("header.top"), nextTop = $("header.top", doc);
    if (top && nextTop) top.replaceWith(nextTop);
    document.title = doc.title;
    v0 = document.body.dataset.version = doc.body.dataset.version;
    if (url !== location.pathname + location.search) history.replaceState(null, "", url);
    if ($(".side") && sideAt) [$(".side").scrollLeft, $(".side").scrollTop] = sideAt;
    if (snoozedOpen && $("details.snoozed")) $("details.snoozed").open = true;
    openIds.forEach((id) => { const r = $(`.row[data-id="${CSS.escape(id)}"]`); if (r) setOpen(r, true); });
    reselect(selId);
    backLinks();
    $(".updated").hidden = true;
    return true;
  };
  // Keep the selected item selected. If you closed or snoozed it, the row
  // that came after it is next, even if it's still on the page (a snoozed
  // row lands in the Snoozed list). If it went any other way (closed
  // elsewhere, or a decision took it off the list), nothing is selected: a
  // key must never land on a row you didn't pick, or one you can't see.
  const reselect = (selId) => {
    if (!selId) return;
    const at = (id) => rows().findIndex((r) => r.dataset.id === id);
    let id = selId;
    for (let hops = 0; follow.has(id) && hops < 100; hops++) id = follow.get(id);
    const i = at(id);
    sel = -1;
    $$(".row.sel").forEach((r) => r.classList.remove("sel"));
    if (i >= 0) select(i, true);
  };
  // When the page can't be swapped (you're mid-answer), still settle the
  // rows you acted on: a closed one goes, the rest show what happened. A row
  // holding your unsent words is left exactly as it is.
  const patch = (doc, start) => {
    const selId = sel >= 0 ? rows()[sel]?.dataset.id : null;
    for (const [id, n] of [...leaving]) {
      if (n > start) continue;
      leaving.delete(id);
      const q = `.row[data-id="${CSS.escape(id)}"]`;
      const cur = $(q), next = $(q, doc);
      if (!cur || cur.contains(document.activeElement) || $$("textarea", cur).some((t) => t.value.trim()) || $(".noise-form:not([hidden])", cur)) continue;
      if (!next) { cur.remove(); continue; }
      const open = cur.classList.contains("open");
      cur.replaceWith(next);
      if (open) setOpen(next, true);
    }
    reselect(selId);
  };
  let refreshing = false, again = false;
  async function refresh() {
    if (refreshing) { again = true; return; }
    refreshing = true;
    try {
      do {
        again = false;
        const start = seq, url = fresh(location.href);
        const res = await fetch(url, { signal: AbortSignal.timeout(10000) }); // a hung fetch mustn't stop refreshing
        if (!res.ok) throw new Error(String(res.status));
        const doc = new DOMParser().parseFromString(await res.text(), "text/html");
        if (swap(doc, url)) { for (const [id, n] of leaving) if (n <= start) leaving.delete(id); }
        else { patch(doc, start); $(".updated").hidden = false; }
        // What's settled no longer needs a successor (a row that wakes from
        // snooze later is a fresh row).
        for (const k of [...follow.keys()]) if (!leaving.has(k)) follow.delete(k);
        fade();
      } while (again);
    } catch (_) {
      // Server unreachable or mid-restart: never navigate away from unsent
      // words; the pill offers a reload, and the next check tries again.
      $(".updated").hidden = false;
    } finally {
      refreshing = false;
    }
  }

  // The note box opens under the row's title; the row folds up so the box
  // sits where you tapped. On an item's own page it opens under the buttons.
  // With a mouse or the w key the cursor goes in; on a phone, the keyboard
  // waits until you tap in.
  const openNoise = (row, focus = matchMedia("(hover: hover)").matches) => {
    const f = $(".noise-form", row);
    if (!f) return (location.href = row.dataset.href + "#noise"); // e.g. the Only-you list
    if (single) $(".detail .actions", row)?.after(f);
    else setOpen(row, false);
    f.hidden = false;
    f.scrollIntoView({ block: "nearest" });
    if (focus) $("textarea", f).focus({ preventScroll: true });
  };
  // A closed note box no longer holds back the refresh it was blocking.
  const closeNoise = (f) => {
    f.hidden = true;
    if (!single && (leaving.size || !$(".updated").hidden)) refresh();
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
    if (e.target.closest("[data-noise-cancel]")) return closeNoise(e.target.closest(".noise-form"));
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
    // An answer posts the plain way; remember it for u all the same.
    const ans = e.target.closest(".answer-form");
    if (ans) return store.set("docket.last", ans.closest("[data-id]").dataset.id);
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
      if (!value) { s.value = [...s.options].find((o) => o.defaultSelected)?.value ?? s.value; return; }
    }
    act(s.closest("[data-id]").dataset.id, "reclassify", { [field]: value });
  };
  document.addEventListener("pointerdown", (e) => {
    const s = e.target.closest("select[data-reclassify]");
    if (s) s.dataset.pointer = "1";
  });
  // A click that picked nothing mustn't license a later keystroke.
  document.addEventListener("focusout", (e) => {
    if (e.target.matches && e.target.matches("select[data-reclassify]")) delete e.target.dataset.pointer;
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
      // Esc closes an empty note box; a half-written note only loses focus,
      // so the words aren't thrown away.
      const empty = (f) => !$("textarea", f).value.trim();
      if (typing) {
        const nf = document.activeElement.closest(".noise-form");
        document.activeElement.blur();
        return nf && empty(nf) && closeNoise(nf);
      }
      const nf = $$(".noise-form").find((f) => !f.hidden && empty(f));
      if (nf) return closeNoise(nf);
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
      // Enter on a project or urgency select moves the item; any other key
      // means the keyboard is driving it now, not an earlier click.
      if (document.activeElement.matches("select[data-reclassify]")) {
        delete document.activeElement.dataset.pointer;
        if (e.key === "Enter") {
          e.preventDefault();
          reclassify(document.activeElement);
        }
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
    // u undoes the last action, selected row or not (on a closed item, the
    // selected row's own Reopen).
    if (k === "u") {
      const own = el && $("button[data-act=reopen]", el);
      if (own) return own.click();
      // While an action is on its way, what u would undo isn't known yet.
      if (pending.size) return toast("Still sending; press u again in a moment");
      const id = store.get("docket.last");
      return id ? act(id, "undo") : toast("Nothing to undo");
    }
    // Nothing selected, or the selected row is on its way out (a key pressed
    // twice quickly): do nothing rather than act on something else.
    if (!el || leaving.has(el.dataset.id)) return;
    if (k === "o" && el.dataset.session) return window.open(el.dataset.session, "_blank", "noopener");
    if (k === "a") {
      e.preventDefault();
      setOpen(el, true);
      const t = $(".answer-form textarea", el);
      return t ? t.focus() : toast(el.dataset.closed ? "This item is closed" : "This item has no agent session to answer");
    }
    if (k === "w") { e.preventDefault(); return $("[data-noise]", el) && openNoise(el, true); }
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
  function backLinks() {
    $$(".answer-form input[name=back]").forEach((i) => { if (!i.value) i.value = location.pathname + location.search; });
  }
  backLinks();

  // The toast an action on the last page left for this one.
  try {
    const c = JSON.parse(store.get("docket.toast") || "null");
    store.set("docket.toast", "");
    if (c && Date.now() - c.at < 5000) toast(c.msg, c.undoId);
  } catch (_) {}

  // ---- live refresh ---------------------------------------------------------------
  // Refresh when the queue changes, but never while you're reading an expanded
  // item or typing: show a quiet "refresh" pill instead.
  setInterval(async () => {
    if (document.hidden || refreshing) return;
    try {
      const r = await fetch("/api/version");
      const { v } = await r.json();
      if (v === v0) return;
      // A selected or expanded row, or typing, means you're in the middle of
      // something: offer the refresh instead of moving rows under you.
      // Rows held after an action (a refresh that failed) always retry: the
      // swap itself still waits out typing and drafts.
      const busy = single || sel >= 0 || anyOpen() || typing() || drafting();
      if (busy && !leaving.size) $(".updated").hidden = false;
      else refresh();
    } catch (_) {}
  }, 5000);
})();
