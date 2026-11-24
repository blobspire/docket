// docket: the item sheet with its answer dock, one-tap closes, the keyboard,
// and a refresh that never yanks a row out from under you.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const ss = {
    get: (k) => { try { return sessionStorage.getItem(k); } catch (_) { return null; } },
    set: (k, v) => { try { sessionStorage.setItem(k, v); } catch (_) {} },
  };
  const timeout = (ms) => (AbortSignal.timeout ? AbortSignal.timeout(ms) : undefined);
  const scrim = $("#scrim"), nscrim = $("#nscrim"), lbEl = $("#lb"), track = $("#lb-tr");
  const solo = !!$(".sheet-page"); // an item's own page
  const docket = document.body.dataset.page === "docket" && !solo;
  const finePointer = matchMedia("(hover: hover) and (pointer: fine)");
  const escId = (id) => CSS.escape(id);

  // ---- state ------------------------------------------------------------------------
  let sel = null; // the selected row's item id
  // The selection is for the keyboard: a press on the list, or a mouse or a
  // tap that closes an item, clears it; j/k then pick up from resume.
  let pointer = false, resume = null;
  let openId = solo ? $(".sheet")?.dataset.id : null; // the item in the sheet
  let pushed = false; // the sheet added a history entry (Back closes it)
  let sheetSeq = 0;
  let listURL = solo ? "/" : location.pathname + location.search;
  const pending = new Set(); // items with an action on its way: one at a time
  let noteDone = null; // what the open note does with its text

  // ---- rows -----------------------------------------------------------------------------
  // Rows you can see: a folded "Waiting on agents" hides its rows.
  const visible = (r) => r.offsetParent !== null && !r.closest("details:not([open])");
  // Rows on their way out (an action sent) can't be selected or acted on;
  // with all=true they still count, to find what comes after one.
  const gone = (r) => r.matches(".leaving, .gone-l, .gone-r");
  const rowIds = (all = false) => $$(".rw[data-id]").filter((r) => visible(r) && (all || !gone(r))).map((r) => r.dataset.id);
  const rowOf = (id) => id && $(`.row[data-id="${escId(id)}"]`);
  const markSel = (scroll) => {
    $$(".row.sel").forEach((r) => r.classList.remove("sel"));
    const r = rowOf(sel);
    if (!r || !visible(r.closest(".rw"))) return;
    r.classList.add("sel");
    if (scroll) r.scrollIntoView({ block: "nearest" });
  };
  const clearSel = () => { if (sel) resume = sel; sel = null; markSel(false); };
  // An item still waiting on the human: where Done and the others make sense
  // (not an answer waiting on its agent, or a snoozed one).
  const actionable = (id) => { const r = rowOf(id); return !!r && !r.dataset.quiet && !r.dataset.kept; };
  // The next item to show after this one goes: the one after it that still
  // needs you, else the one before.
  const nextAfter = (id) => {
    const order = rowIds(true), i = order.indexOf(id);
    const leaving = (x) => { const r = $(`.rw[data-id="${escId(x)}"]`); return !r || gone(r); };
    const ok = (x) => x !== id && !pending.has(x) && !leaving(x) && (!docket || actionable(x));
    if (i < 0) return null;
    return order.slice(i + 1).find(ok) || order.slice(0, i).reverse().find(ok) || null;
  };

  // ---- toast ----------------------------------------------------------------------------
  const toastEl = $("#toast");
  let toastTimer, undoId = null, undoDecision = "", keepId = null, undoFn = null;
  const toast = (msg, { undo = null, keep = null, err = false, decision = "", ms = 0, onUndo = null } = {}) => {
    $("#toast-t").textContent = msg;
    undoId = undo; keepId = keep; undoDecision = undo ? decision : ""; undoFn = onUndo;
    $("#toast-u").hidden = !(undo || onUndo);
    $("#toast-k").hidden = !keep;
    toastEl.classList.toggle("err", err);
    placeToast();
    toastEl.hidden = false;
    toastEl.style.animation = "none"; void toastEl.offsetWidth; toastEl.style.animation = "";
    clearTimeout(toastTimer);
    toastTimer = setTimeout(hideToast, ms || (undo || keep || err ? 6500 : 3500));
  };
  const hideToast = () => { toastEl.hidden = true; undoId = keepId = null; undoFn = null; };
  // Above the dock while an item is open, and above the "list changed"
  // pill, so it never covers a button.
  const placeToast = () => {
    let b = 22;
    const d = openId && $(".sheet .dock"), pill = $(".updated");
    if (d) b = Math.max(b, innerHeight - d.getBoundingClientRect().top + 12);
    if (pill && !pill.hidden) b = Math.max(b, innerHeight - pill.getBoundingClientRect().top + 8);
    toastEl.style.bottom = b > 22 ? `${b}px` : "";
  };
  const showPill = (on) => { const p = $(".updated"); if (p && p.hidden === on) { p.hidden = !on; placeToast(); } };
  addEventListener("resize", placeToast);
  // A toast for the page about to load (an item's own page goes back to the list).
  const carry = (msg, undo, ms = 0) => ss.set("docket.toast", JSON.stringify({ msg, undo, ms, at: Date.now() }));

  // ---- the item sheet ---------------------------------------------------------------------
  const listLabel = () => $("main[data-label]")?.dataset.label || "docket";
  const setupSheet = (sh) => {
    const order = rowIds(), i = order.indexOf(openId);
    const back = $("[data-back]", sh);
    if (!solo && back) $("span", back).textContent = listLabel();
    const pos = $("[data-pos]", sh);
    if (!solo && i >= 0 && pos) {
      pos.textContent = `${i + 1} of ${order.length}`;
      $$("[data-nav]", sh).forEach((b) => {
        b.hidden = false;
        b.disabled = +b.dataset.nav < 0 ? i <= 0 : i >= order.length - 1;
      });
    } else {
      if (pos) pos.textContent = "";
      $$("[data-nav]", sh).forEach((b) => { b.hidden = true; });
    }
    // An item's own page goes back to the list you came from.
    if (solo && back) {
      try {
        const r = new URL(document.referrer);
        if (r.origin === location.origin && !r.pathname.startsWith("/item/")) {
          back.href = r.pathname + r.search;
          const names = { "/stash": "Stash", "/done": "Done", "/ideas": "Ideas", "/search": "Search", "/brain": "Brain", "/leads": "Leads", "/evidence": "Evidence", "/health": "Health", "/timeline": "Timeline" };
          const q = r.searchParams;
          $("span", back).textContent = r.pathname === "/" ? (q.get("p") || q.get("place") || (q.get("view") === "now" ? "Act now" : "All")) : names[r.pathname] || "Back";
        }
      } catch (_) {}
    }
    fadeOptions($(".dk-a", sh), true);
    // A long attachment folds, with a button to see all of it.
    $$(".doc-b.clamp", sh).forEach((d) => {
      const long = d.scrollHeight > d.clientHeight + 4;
      d.classList.toggle("long", long);
      d.nextElementSibling.hidden = !long;
    });
    placeToast();
  };
  const fadeOptions = (opts, first) => {
    if (!opts) return;
    if (first) opts.classList.toggle("scrolls", opts.scrollHeight > opts.clientHeight + 2);
    opts.classList.toggle("over", opts.classList.contains("scrolls") && opts.scrollTop + opts.clientHeight < opts.scrollHeight - 2);
  };
  document.addEventListener("scroll", (e) => { if (e.target.matches && e.target.matches(".dk-a")) fadeOptions(e.target); }, true);
  const skeleton = (label) => {
    const sh = document.createElement("div");
    sh.className = "sheet";
    sh.innerHTML = `<header class="sh-h"><a class="back" data-back><svg class="ic" aria-hidden="true"><use href="#i-chev-l"/></svg><span></span></a></header><div class="sh-b"></div>`;
    $("a", sh).href = listURL;
    $("span", sh).textContent = label;
    return sh;
  };
  async function openSheet(id, how = "push") {
    const seq = ++sheetSeq;
    const again = !scrim.hidden;
    // The sheet on screen is the previous item's until the next one arrives:
    // nothing on it may act meanwhile.
    if (again) { const old = sheetEl(); if (old) { old.classList.add("busy"); old.inert = true; } }
    openId = sel = id;
    markSel(true);
    let opened = 0;
    if (!again) {
      opened = performance.now();
      scrim.replaceChildren(skeleton(listLabel()));
      scrim.hidden = false;
      scrim.firstElementChild.addEventListener("animationend", placeToast, { once: true });
      document.body.classList.add("sheet-open");
    }
    if (how === "push" && !pushed) { history.pushState({ sheet: id }, "", `/item/${encodeURIComponent(id)}`); pushed = true; }
    else if (how !== "none") history.replaceState(pushed ? { sheet: id } : null, "", `/item/${encodeURIComponent(id)}`);
    let sh = null;
    try {
      const res = await fetch(`/item/${encodeURIComponent(id)}`, { signal: timeout(15000) });
      if (res.ok) sh = $(".sheet", new DOMParser().parseFromString(await res.text(), "text/html"));
    } catch (_) {}
    if (seq !== sheetSeq || openId !== id) return; // another item (or none) since
    if (!sh) { closeSheet(); toast("Couldn't open that item", { err: true }); return; }
    // The real sheet takes the skeleton's place mid-way through its slide,
    // never starting over.
    const elapsed = opened ? performance.now() - opened : Infinity;
    if (elapsed < 320) {
      sh.style.animationDelay = `-${Math.round(elapsed)}ms`;
      sh.addEventListener("animationend", placeToast, { once: true }); // the dock has landed
    } else sh.classList.add("again");
    scrim.replaceChildren(sh);
    setupSheet(sh);
    const body = $(".sh-b", sh);
    body.scrollTop = 0;
    // Focus the scrolling body: the arrows, Space and Page Down read it, and
    // Enter means nothing there.
    body.tabIndex = -1;
    body.focus({ preventScroll: true });
  }
  // Close it: through history when the sheet added an entry, so the
  // browser's Back and this one agree.
  const closeSheet = () => { if (pushed) { pushed = false; history.back(); } else shut(); };
  const shut = () => {
    sheetSeq++;
    pushed = false;
    openId = null;
    scrim.hidden = true;
    scrim.replaceChildren();
    document.body.classList.remove("sheet-open");
    if (location.pathname.startsWith("/item/")) history.replaceState(null, "", listURL);
    if (pointer) clearSel(); else markSel(true);
    placeToast();
  };
  addEventListener("popstate", (e) => {
    if (solo) return;
    const want = e.state && e.state.sheet;
    if (want && want !== openId) { pushed = true; openSheet(want, "none"); }
    else if (!want && openId) { pushed = false; shut(); }
    else if (!want && !scrim.hidden) shut();
  });
  const nav = (d) => {
    if (!openId || solo) return;
    const order = rowIds(), i = order.indexOf(openId), j = i + d;
    if (i < 0 || j < 0 || j >= order.length) return;
    openSheet(order[j], "replace");
  };
  const sheetEl = () => (openId ? $(".sheet") : null);
  const sheetScroller = (sh) => sh && ($(".sh-b", sh) || sh); // what scrolls in a sheet
  const liveSheet = () => { const sh = sheetEl(); return sh && sh.dataset.id === openId && !sh.classList.contains("busy") && $(".dock", sh) ? sh : null; };

  // The answer box: folded under the options until Reply (or r).
  const replyForm = () => $(".sheet [data-reply]");
  const openReply = () => {
    if (!liveSheet()) return;
    const f = replyForm();
    if (!f) return toast("This item takes no written answer");
    f.hidden = false;
    $("textarea", f).focus();
    placeToast();
  };
  const closeReply = () => {
    const f = replyForm();
    if (!f) return;
    if ($("[data-reply-x]", f)) { f.hidden = true; $("[data-reply-err]")?.setAttribute("hidden", ""); placeToast(); }
    $("textarea", f).blur();
    refocus();
  };
  const grow = (t) => { t.style.height = "auto"; t.style.height = Math.min(t.scrollHeight, 160) + "px"; };

  // ---- actions --------------------------------------------------------------------------------
  const verb = { approve: "approve it", decline: "decline it", choose: "send that", answer: "send that", handoff: "hand it to the agent", done: "mark it done",
    dismiss: "dismiss it", noise: "close it", snooze: "snooze it", stash: "stash it", unstash: "take it off the Stash",
    reopen: "put it back", reclassify: "move it", undo: "undo that", ask: "send the question" };
  const clip = (s) => (s.length > 28 ? s.slice(0, 27) + "…" : s);
  const said = (action, extra, id) => {
    switch (action) {
      case "answer": return `Sent “${clip(extra.text)}” to the agent`;
      case "ask": return "Asked the agent. Its answer is added to the item";
      case "choose": { const b = $(`.sheet [data-option="${extra.option}"] span`); return b ? `Sent “${clip(b.textContent)}” to the agent` : "Sent to the agent"; }
      case "approve": return "Approved";
      case "handoff": return extra.text ? "Handed to the agent, with your note" : "Handed to the agent";
      case "decline": return "Declined";
      case "done": return "Marked done";
      case "dismiss": return "Dismissed";
      case "noise": {
        if (!extra.text) return "Marked not worth your time";
        const el = (openId === id && sheetEl()) || rowOf(id);
        return el && el.hasAttribute("data-email") ? "Closed. Triage reads your note from now on." : "Closed. Your note goes to the agent.";
      }
      case "snooze": return extra.until === "hour" ? "Snoozed for an hour" : "Snoozed until tomorrow, 9:00";
      case "stash": return "Stashed";
      case "unstash": return "Moved to Done";
      case "reopen": return "Back on your docket";
      case "reclassify": return "Moved";
      case "undo": return "Undone";
    }
    return "Done";
  };
  // What leaves the list it's on (and slides out).
  const leaves = (action) => !["reclassify", "undo", "ask"].includes(action);
  const answers = ["approve", "decline", "choose", "answer", "handoff"];
  // The actions whose message to the agent the server holds back for a moment
  // (undoHold in deliver.go): an answer, a "marked done" (a done, or a stash
  // that closes the item) and a "not worth my time" note. Their toast offers
  // Undo for a second less than the hold, so an Undo from it reaches the
  // server while the message is still held, unless the round trip takes over
  // a second; after the hold it has gone out.
  const held = [...answers, "done", "noise", "stash"];
  const heldUndoMs = 5000;

  async function act(id, action, extra = {}) {
    if (!id || pending.has(id)) return false;
    pending.add(id);
    const inSheet = openId === id;
    const sh = inSheet ? sheetEl() : null;
    sh?.classList.add("busy");
    const rw = $(`.rw[data-id="${escId(id)}"]`);
    const movedFrom = !inSheet && sel === id && leaves(action) && rw ? id : null;
    let movedTo = null;
    if (movedFrom) { sel = movedTo = nextAfter(id); markSel(true); }
    if (rw && leaves(action)) rw.classList.add("leaving");
    const msg = said(action, extra, id); // while the sheet still shows this item
    let err = "", decision = "";
    try {
      const body = new URLSearchParams({ action, ...extra });
      const res = await fetch(`/item/${encodeURIComponent(id)}/act`, { method: "POST", body, headers: { "X-Requested-With": "fetch" }, signal: timeout(20000) });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) err = data.error || `Couldn't ${verb[action] || action}`;
      else decision = data.decision || "";
    } catch (_) {
      err = `Couldn't ${verb[action] || action}: the server didn't answer`;
    }
    if (!err && solo) {
      // Leaving for the list: nothing more from this page meanwhile.
      sh?.classList.add("busy");
    } else {
      pending.delete(id);
      sh?.classList.remove("busy");
    }
    if (err) {
      rw?.classList.remove("leaving");
      if (movedFrom && (sel === movedTo || sel === null) && !openId) { sel = movedFrom; markSel(true); } // it's still there, and you haven't moved on
      toast(err, { err: true });
      refresh();
      return false;
    }
    // What u (and the toast's Undo) undoes next; an undo leaves nothing more.
    // Undo exactly what this logged; an action that logged nothing (a move to
    // where it already was) has nothing to undo.
    const undo = action === "undo" || action === "reopen" || !decision ? null : id;
    ss.set("docket.last", undo ? `${undo} ${decision}` : "");
    // After a done or an answer, the toast offers to keep it on the Stash.
    const noStash = !!(rw && rw.querySelector("[data-nostash]")) || !!(sh && sh.hasAttribute("data-nostash"));
    const keep = (action === "done" || answers.includes(action)) && !noStash ? id : null;
    const ms = undo && held.includes(action) ? heldUndoMs : 0;
    if (solo) {
      // From an item's own page: back to the list it came from (fresh), or
      // stay for an undo, a move or a reopen, which keep it here.
      carry(msg, undo ? `${undo} ${decision}` : null, ms);
      if (["undo", "reclassify", "reopen", "stash", "unstash", "ask"].includes(action)) { location.reload(); return true; }
      let to = "/";
      try { const r = new URL(document.referrer); if (r.origin === location.origin && !r.pathname.startsWith("/item/")) to = r.pathname + r.search; } catch (_) {}
      location.href = to;
      return true;
    }
    toast(msg, { undo, keep, decision, ms });
    if (inSheet && openId === id) {
      if (!leaves(action)) openSheet(id, "replace"); // show it as it is now
      else {
        const nx = nextAfter(id);
        if (nx) openSheet(nx, "replace"); else closeSheet();
      }
    }
    if (rw && leaves(action) && document.body.dataset.page !== "search") {
      rw.classList.add(action === "dismiss" || action === "noise" || action === "snooze" ? "gone-l" : "gone-r");
      setTimeout(refresh, 190);
    } else refresh();
    return true;
  }
  const undoLast = () => {
    if (undoFn) { const f = undoFn; hideToast(); return f(); }
    // Undo exactly the decision it reported (another tab may have acted since).
    const [id, decision] = undoId ? [undoId, undoDecision] : (ss.get("docket.last") || "").split(" ");
    hideToast();
    if (!id) return toast("Nothing to undo");
    if (pending.size) return toast("Still sending; press u again in a moment");
    act(id, "undo", decision ? { decision } : {});
  };

  // ---- "not worth my time" ------------------------------------------------------------------
  // Every "Not worth it" asks why first, in an optional note: an item's goes
  // to the agent that filed it, an email's to triage, which reads it on
  // every later check. done closes the thing, with the note or without one.
  const openNote = (title, email, done) => {
    noteDone = done;
    const ta = $("#note-ta");
    $("#note-t").textContent = (title || "").trim();
    ta.value = "";
    ta.placeholder = email ? ta.dataset.email : ta.dataset.agent;
    nscrim.hidden = false;
    if (finePointer.matches) ta.focus();
  };
  const noteItem = (id) => {
    const r = rowOf(id), sh = openId === id ? sheetEl() : null;
    const title = (sh && $(".sh-t", sh)?.textContent) || (r && $(".t", r)?.textContent) || "";
    openNote(title, !!(sh || r)?.hasAttribute("data-email"), (text) => act(id, "noise", { text }));
  };
  const refocus = () => { const b = !solo && sheetEl() && $(".sh-b", sheetEl()); if (b && b.tabIndex === -1) b.focus({ preventScroll: true }); };
  const closeNote = () => { nscrim.hidden = true; noteDone = null; refocus(); };
  const sendNote = (text) => { const done = noteDone; closeNote(); if (done) done(text); };

  // ---- attachment viewer ----------------------------------------------------------------------
  let lbImgs = [], lbAt = 0;
  let lbFrom = null;
  const openLB = (imgs, k, from) => {
    if (!imgs.length) return;
    lbFrom = from || null;
    lbImgs = imgs;
    track.replaceChildren(...imgs.map((a) => {
      const s = document.createElement("div"), img = document.createElement("img");
      s.className = "lb-s"; img.src = a.src; img.alt = a.name; s.append(img);
      return s;
    }));
    lbEl.hidden = false;
    $(".lb-p", lbEl).hidden = $(".lb-n", lbEl).hidden = imgs.length < 2;
    requestAnimationFrame(() => { track.scrollLeft = k * track.clientWidth; setLB(k); });
    $(".lb-x", lbEl).focus({ preventScroll: true });
  };
  const setLB = (k) => {
    lbAt = k;
    const n = lbImgs.length, a = lbImgs[k];
    $("#lb-cap").textContent = a.name;
    $("#lb-cnt").textContent = n > 1 ? `${k + 1} of ${n}` : "";
    $("#lb-dl").href = a.src;
    $(".lb-p", lbEl).disabled = k === 0;
    $(".lb-n", lbEl).disabled = k === n - 1;
  };
  const goLB = (d) => {
    const k = Math.max(0, Math.min(lbImgs.length - 1, lbAt + d));
    const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;
    track.scrollTo({ left: k * track.clientWidth, behavior: reduce ? "auto" : "smooth" });
    setLB(k);
  };
  const closeLB = () => {
    lbEl.hidden = true;
    track.replaceChildren();
    if (lbFrom && lbFrom.isConnected) lbFrom.focus({ preventScroll: true });
    lbFrom = null;
  };
  track.addEventListener("scroll", () => {
    if (lbEl.hidden || !track.clientWidth) return;
    const k = Math.round(track.scrollLeft / track.clientWidth);
    if (k !== lbAt && lbImgs[k]) setLB(k);
  }, { passive: true });
  // An image's set: the sheet's figures, or a row's list of images.
  const imagesFor = (el) => {
    const th = el.closest(".th[data-imgs]");
    if (th) { try { return JSON.parse(th.dataset.imgs); } catch (_) { return []; } }
    return $$(".fig button[data-lb] img", el.closest(".sheet") || document).map((i) => ({ src: i.getAttribute("src"), name: i.alt }));
  };

  // ---- clicks -------------------------------------------------------------------------------------
  let suppress = false; // a swipe just ended: its click isn't one
  // A drag that starts in the sheet (selecting text) and ends on the
  // backdrop clicks the backdrop: only a press that starts there closes.
  let downOn = null;
  document.addEventListener("pointerdown", (e) => {
    downOn = e.target;
    pointer = true;
    // A press on the list (a row included: a click opens it, or acts on
    // it without moving a highlight to the next one) ends keyboard selection.
    if (sel && scrim.hidden && nscrim.hidden && lbEl.hidden && !e.target.closest("#toast")) clearSel();
  }, true);
  let skipped = false; // a refresh waited for a menu to close
  const closeMenus = (except) => {
    if (skipped && !except && $$(".menu").some((x) => !x.hidden)) { skipped = false; setTimeout(refresh, 0); }
    $$(".menu").forEach((m) => {
      if (m === except) return;
      m.hidden = true;
      m.parentElement.querySelector("[aria-haspopup]")?.setAttribute("aria-expanded", "false");
    });
  };
  document.addEventListener("click", (e) => {
    if (suppress) { suppress = false; e.preventDefault(); return; }
    const t = e.target;
    let el;
    if (!t.closest(".menu, [aria-haspopup]")) closeMenus();
    if ((el = t.closest("[aria-haspopup]"))) {
      const m = el.parentElement.querySelector(".menu");
      if (m) {
        closeMenus(m);
        m.hidden = !m.hidden;
        el.setAttribute("aria-expanded", String(!m.hidden));
        if (m.hidden && skipped && !$$(".menu").some((x) => !x.hidden)) { skipped = false; refresh(); }
      }
      return;
    }
    // The viewer and the note sit above everything else.
    if (!lbEl.hidden) {
      if (t.closest(".lb-x")) return closeLB();
      if (t.closest(".lb-p")) return goLB(-1);
      if (t.closest(".lb-n")) return goLB(1);
      if (!t.closest("img, a")) closeLB();
      return;
    }
    if (!nscrim.hidden) {
      if (t.closest("[data-note-skip]")) sendNote("");
      else if (t === nscrim && downOn === nscrim) closeNote();
      return;
    }
    if ((el = t.closest("[data-lb]"))) { e.preventDefault(); return openLB(imagesFor(el), +el.dataset.lb.split(":").pop(), el); }
    if ((el = t.closest("#toast-u"))) return undoLast();
    if ((el = t.closest("#toast-k"))) { const k = keepId; hideToast(); return k && act(k, "stash"); }
    if ((el = t.closest("[data-help]"))) return toggleHelp();
    if (t.closest("[data-help-close]") || (t.classList.contains("help") && downOn === t)) { $(".help").hidden = true; return; }
    if ((el = t.closest("[data-refresh]"))) return refresh(true);
    if ((el = t.closest("[data-copy]"))) return copy(el.dataset.copy);
    if ((el = t.closest("[data-collect]"))) {
      fetch("/api/collect", { method: "POST", headers: { "X-Requested-With": "fetch" } }).then(() => toast("Collecting…"));
      return;
    }
    if ((el = t.closest("[data-edit-project]"))) {
      const f = $(".project-edit");
      if (f) { f.hidden = !f.hidden; if (!f.hidden) $("input", f).focus(); }
      return;
    }
    // Inside the sheet.
    if (t === scrim) return downOn === scrim && closeSheet();
    if ((el = t.closest("[data-back]")) && !solo) { e.preventDefault(); return closeSheet(); }
    if ((el = t.closest("[data-nav]"))) return nav(+el.dataset.nav);
    if ((el = t.closest("[data-reply-open]"))) return openReply();
    if (t.closest(".sheet.busy")) return;
    if ((el = t.closest("[data-reply-x]"))) return closeReply();
    if ((el = t.closest(".doc-more"))) {
      const d = el.previousElementSibling, folded = d.classList.toggle("clamp");
      el.textContent = folded ? "Show the whole file" : "Show less";
      el.setAttribute("aria-expanded", String(!folded));
      return;
    }
    // Actions, on a row or in the dock.
    if ((el = t.closest("button[data-act], button[data-noise]"))) {
      e.preventDefault();
      const host = el.closest(".sheet");
      if (el.disabled || (host && !solo && host !== liveSheet())) return;
      const id = el.closest("[data-id]")?.dataset.id;
      if (el.hasAttribute("data-noise")) return noteItem(id);
      const extra = {};
      if (el.dataset.option !== undefined) extra.option = el.dataset.option;
      if (el.dataset.act === "snooze") extra.until = e.shiftKey ? "hour" : "tomorrow";
      // Words typed in the open reply box go along with an option, an
      // approval or a hand-off, never silently dropped.
      if (host && answers.includes(el.dataset.act)) {
        const f = $("[data-reply]", host), words = f && !f.hidden ? $("textarea", f).value.trim() : "";
        if (words) extra.text = words;
      }
      return act(id, el.dataset.act, extra);
    }
    // A progress line or a timeline entry opens its item over the page.
    if ((el = t.closest("[data-open]")) && !(e.metaKey || e.ctrlKey || e.shiftKey || e.button > 0)) {
      e.preventDefault();
      return openSheet(el.dataset.open);
    }
    // A row opens its item; a modified click on its title opens a tab.
    if ((el = t.closest(".row[data-id]")) && !t.closest("button, select, input, textarea, form")) {
      if (t.closest("a") && (e.metaKey || e.ctrlKey || e.shiftKey || e.button > 0)) return;
      e.preventDefault();
      return openSheet(el.dataset.id);
    }
  });
  const copy = async (text) => {
    try { await navigator.clipboard.writeText(text); toast("Copied"); }
    catch (_) { prompt("Copy this:", text); }
  };

  document.addEventListener("submit", (e) => {
    const f = e.target;
    if (f.id === "note") {
      e.preventDefault();
      return sendNote($("#note-ta").value.trim());
    }
    // The email reader's "Not worth it" asks why, then posts as it would.
    if (f.matches("[data-email-noise]")) {
      e.preventDefault();
      return openNote($(".rd-s")?.textContent, true, (note) => { f.elements.note.value = note; f.submit(); });
    }
    if (f.matches("[data-reply]")) {
      e.preventDefault();
      const text = $("textarea", f).value.trim();
      if (!text) { $("[data-reply-err]")?.removeAttribute("hidden"); return $("textarea", f).focus(); }
      act(f.closest("[data-id]").dataset.id, f.elements.action?.value || "answer", { text });
    }
  });
  document.addEventListener("input", (e) => {
    if (e.target.matches("[data-reply] textarea")) { $("[data-reply-err]")?.setAttribute("hidden", ""); grow(e.target); placeToast(); }
  });
  // Waiting folds stay as you left them.
  const folds = () => { try { return JSON.parse(ss.get("docket.wt") || "[]"); } catch (_) { return []; } };
  const applyFolds = (root = document) => { const open = folds(); $$("details[data-wt]", root).forEach((d) => { d.open = open.includes(d.dataset.wt); }); };
  document.addEventListener("toggle", (e) => {
    const d = e.target;
    if (!d.matches || !d.matches("details[data-wt]")) return;
    const open = new Set(folds());
    d.open ? open.add(d.dataset.wt) : open.delete(d.dataset.wt);
    ss.set("docket.wt", JSON.stringify([...open]));
    if (!d.open && sel && d.contains(rowOf(sel))) { sel = null; markSel(false); }
  }, true);

  // Moving an item: a pick with the mouse acts at once; from the keyboard
  // (where typing a letter changes a closed select), only Enter does.
  const reclassify = (s) => {
    let value = s.value;
    if (value === "__new") {
      value = (prompt("New project name (letters, digits, dashes):") || "").trim();
      if (!value) { s.value = [...s.options].find((o) => o.defaultSelected)?.value ?? s.value; return; }
    }
    act(s.closest("[data-id]").dataset.id, "reclassify", { [s.dataset.reclassify]: value });
  };
  document.addEventListener("pointerdown", (e) => { const s = e.target.closest("select[data-reclassify]"); if (s) s.dataset.pointer = "1"; });
  document.addEventListener("focusout", (e) => { if (e.target.matches && e.target.matches("select[data-reclassify]")) delete e.target.dataset.pointer; });
  document.addEventListener("change", (e) => {
    const s = e.target.closest("select[data-reclassify]");
    if (!s) return;
    if (s.dataset.pointer) { delete s.dataset.pointer; return reclassify(s); }
    toast("Press Enter to move it");
  });

  // ---- swipe a row (phone): right for done, left to dismiss ------------------------------
  let drag = null;
  document.addEventListener("pointerdown", (e) => {
    const r = e.target.closest(".row[data-id]");
    if (!r || !docket || e.pointerType === "mouse" || e.target.closest("button, a.thb") || !matchMedia("(max-width: 640px)").matches) return;
    drag = { r, rw: r.closest(".rw"), x: e.clientX, y: e.clientY, dx: 0, on: false, id: r.dataset.id, open: actionable(r.dataset.id) };
  });
  document.addEventListener("pointermove", (e) => {
    if (!drag) return;
    const dx = e.clientX - drag.x, dy = e.clientY - drag.y;
    if (!drag.on) {
      if (Math.abs(dx) > 10 && Math.abs(dx) > Math.abs(dy) * 1.3) { drag.on = true; drag.r.setPointerCapture(e.pointerId); drag.r.style.transition = "none"; }
      else if (Math.abs(dy) > 10) { drag = null; return; } else return;
    }
    drag.dx = dx;
    drag.r.style.transform = `translateX(${dx}px)`;
    drag.rw.classList.toggle("sw-r", dx > 0 && drag.open);
    drag.rw.classList.toggle("sw-l", dx < 0);
  });
  const endDrag = () => {
    if (!drag) return;
    const d = drag;
    drag = null;
    if (!d.on) return;
    suppress = true;
    setTimeout(() => { suppress = false; }, 60);
    d.r.style.transition = "transform .2s";
    if (Math.abs(d.dx) > 96 && !pending.has(d.id) && (d.dx < 0 || d.open)) {
      d.r.style.transform = `translateX(${d.dx > 0 ? 110 : -110}%)`;
      setTimeout(() => act(d.id, d.dx > 0 && d.open ? "done" : "dismiss").then((ok) => { if (!ok) { d.r.style.transform = ""; d.rw.classList.remove("sw-r", "sw-l"); } }), 160);
    } else {
      d.r.style.transform = "";
      setTimeout(() => d.rw.classList.remove("sw-r", "sw-l"), 200);
    }
  };
  document.addEventListener("pointerup", endDrag);
  document.addEventListener("pointercancel", endDrag);

  // ---- keyboard ------------------------------------------------------------------------------
  const toggleHelp = () => { const h = $(".help"); h.hidden = !h.hidden; };
  let g = false;
  document.addEventListener("keydown", (e) => {
    pointer = false;
    if (window.docketDictate && window.docketDictate.isOpen()) return; // the capture sheet has the keys
    const k = e.key, a = document.activeElement;
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) || a.isContentEditable;
    if (e.metaKey || e.ctrlKey || e.altKey) {
      // Cmd/Ctrl+Enter sends what you're writing.
      if (k === "Enter" && a.tagName === "TEXTAREA") { e.preventDefault(); a.form?.requestSubmit(); }
      return;
    }
    if (!nscrim.hidden) {
      if (k === "Escape") { e.preventDefault(); closeNote(); }
      else if (k === "Enter" && !e.shiftKey && !e.isComposing && a.id === "note-ta") { e.preventDefault(); $("#note").requestSubmit(); }
      return;
    }
    if (!lbEl.hidden) {
      if (k === "ArrowRight") goLB(1); else if (k === "ArrowLeft") goLB(-1); else if (k === "Escape") closeLB(); else return;
      return e.preventDefault();
    }
    if (!$(".help").hidden) { if (k === "Escape" || k === "?") { e.preventDefault(); toggleHelp(); } return; }
    if (k === "Escape" && $$(".menu").some((m) => !m.hidden)) return closeMenus();
    if (typing) {
      if (k === "Escape") { e.preventDefault(); if (a.closest("[data-reply]")) closeReply(); else a.blur(); }
      // Enter sends a reply; Shift+Enter is a new line.
      else if (k === "Enter" && !e.shiftKey && !e.isComposing && a.closest("[data-reply]")) { e.preventDefault(); a.form.requestSubmit(); }
      // Enter on a project or urgency select moves the item.
      else if (a.matches("select[data-reclassify]")) { delete a.dataset.pointer; if (k === "Enter") { e.preventDefault(); reclassify(a); } }
      return;
    }
    // A focused link or button keeps its own Enter and Space.
    if ((k === "Enter" || k === " ") && a && a !== document.body && a.matches("a, button, summary")) return;
    if (g) {
      g = false;
      const to = { d: "/", q: "/", t: "/stash", p: "/timeline", i: "/ideas", l: "/leads", e: "/evidence", b: "/brain", h: "/health", s: "/search" }[k];
      if (to) location.href = to;
      return;
    }
    if (k === "g") { g = true; setTimeout(() => (g = false), 800); return; }
    if (k === "?") return toggleHelp();
    if (k === "/") { e.preventDefault(); const q = $("#q"); return q ? q.focus() : (location.href = "/search"); }
    if (k === "u") { e.preventDefault(); return undoLast(); }
    if (k === "n" && window.docketDictate && !sheetEl()) { e.preventDefault(); return window.docketDictate.open(); }
    if (sheetEl()) {
      const sh = solo ? sheetEl() : liveSheet(), id = openId;
      if (k === "Escape") { e.preventDefault(); return solo ? $("[data-back]", sheetEl()).click() : closeSheet(); }
      // j and k move between items; the arrows scroll what you're reading.
      if (k === "j" || k === "k") { if (!solo) { e.preventDefault(); nav(k === "j" ? 1 : -1); } return; }
      if (!sh) return; // still loading: nothing to act on yet
      const dock = $(".dock", sh);
      const press = (sel) => { const b = $(sel, dock); if (b && !b.disabled) { b.click(); return true; } return false; };
      if (/^[1-9]$/.test(k)) { if (press(`.dk-a [data-key="${k}"]`)) e.preventDefault(); return; }
      if (k === "r" || k === "a") { e.preventDefault(); return openReply(); }
      if (k === "o" && sh.dataset.session) return window.open(sh.dataset.session, "_blank", "noopener");
      if (k === "w") { if (press("[data-noise]")) e.preventDefault(); return; }
      if (k === "s" || k === "S") {
        const b = $('[data-act="snooze"]', dock);
        if (b && !b.disabled) { e.preventDefault(); act(id, "snooze", { until: k === "S" ? "hour" : "tomorrow" }); }
        return;
      }
      const map = { d: "done", x: "dismiss", t: "stash", h: "handoff" };
      if (map[k] && press(`[data-act="${map[k]}"]`)) e.preventDefault();
      return;
    }
    // The list.
    if (k === "[" || k === "]") {
      const ls = $$(".side .sl");
      if (!ls.length) return;
      let i = ls.findIndex((l) => l.classList.contains("on"));
      i = k === "]" ? (i + 1) % ls.length : (i <= 0 ? ls.length - 1 : i - 1);
      location.href = ls[i].href;
      return;
    }
    const order = rowIds();
    if (k === "j" || k === "ArrowDown" || k === "k" || k === "ArrowUp") {
      if (!order.length) return;
      e.preventDefault();
      if (sel === null && order.includes(resume)) { sel = resume; resume = null; return markSel(true); } // where you were
      const i = order.indexOf(sel), down = k === "j" || k === "ArrowDown";
      sel = order[i < 0 ? (down ? 0 : order.length - 1) : Math.max(0, Math.min(order.length - 1, i + (down ? 1 : -1)))];
      return markSel(true);
    }
    // Nothing selected, or the selected row is gone or on its way out: do
    // nothing rather than act on something else.
    const r = rowOf(sel);
    if (!r || !order.includes(sel) || pending.has(sel)) return;
    if (k === "Enter") { e.preventDefault(); return openSheet(sel); }
    if (k === "o" && r.dataset.session) return window.open(r.dataset.session, "_blank", "noopener");
    const qa = (s) => $(`.qa ${s}`, r);
    if (k === "w" && qa("[data-noise]")) { e.preventDefault(); return noteItem(sel); }
    const map = { d: "done", x: "dismiss", h: "handoff" };
    if (map[k] && qa(`[data-act="${map[k]}"]`)) { e.preventDefault(); return act(sel, map[k]); }
    if (k === "t" && !r.hasAttribute("data-nostash") && (qa('[data-act="stash"]') || actionable(sel) || r.classList.contains("is-wait"))) { e.preventDefault(); return act(sel, "stash"); }
    if ((k === "s" || k === "S") && actionable(sel)) { e.preventDefault(); return act(sel, "snooze", { until: k === "S" ? "hour" : "tomorrow" }); }
  });

  // ---- refresh in place --------------------------------------------------------------------------
  // Swap in a fresh copy of the list instead of reloading: scroll, the
  // selection, the folds and an open item all stay put. Never while you're
  // typing in the list or editing the project; the pill offers it instead.
  let v0 = document.body.dataset.version;
  const busyList = () => {
    const a = document.activeElement;
    if (a.id === "q" && a.value === a.defaultValue) return !!$(".project-edit:not([hidden])");
    if (window.docketDictate && window.docketDictate.isOpen()) return true;
    // Don't cut off a recording you're listening to.
    if (a.tagName === "AUDIO" || [...document.querySelectorAll("audio")].some((x) => !x.paused)) return true;
    return (/^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && !a.closest("#scrim, #nscrim")) || !!$(".project-edit:not([hidden])");
  };
  let refreshing = false, again = false;
  async function refresh(force = false) {
    if (solo) return;
    if (refreshing) { again = true; return; }
    refreshing = true;
    try {
      do {
        again = false;
        const res = await fetch(listURL, { signal: timeout(10000) });
        if (!res.ok) throw new Error(String(res.status));
        const doc = new DOMParser().parseFromString(await res.text(), "text/html");
        if (!force && $$(".menu").some((m) => !m.hidden)) { skipped = true; break; } // runs when the menu closes
        if (!force && busyList()) { showPill(true); break; }
        const where = [".body", "main#main"].find((s) => $(s) && $(s, doc));
        if (!where) break;
        const side = $(".side"), sideAt = side && [side.scrollLeft, side.scrollTop];
        const q0 = document.activeElement && document.activeElement.id === "q" ? document.activeElement : null;
        const caret = q0 && [q0.selectionStart, q0.selectionEnd, q0.selectionDirection, q0.scrollLeft];
        // Folds open as you left them before the swap: measured closed for a
        // moment, a page that only scrolls with a fold open would jump to the top.
        applyFolds(doc);
        const y = window.scrollY;
        $(where).replaceWith($(where, doc));
        if (window.scrollY !== y) window.scrollTo(0, y);
        $("header.bar").replaceWith($("header.bar", doc));
        document.title = openId ? document.title : doc.title;
        v0 = document.body.dataset.version = doc.body.dataset.version;
        if ($(".side") && sideAt) [$(".side").scrollLeft, $(".side").scrollTop] = sideAt;
        applyFolds();
        observeDigest();
        const q1 = q0 && $("#q");
        if (q1) {
          q1.focus({ preventScroll: true });
          try { q1.setSelectionRange(caret[0], caret[1], caret[2]); q1.scrollLeft = caret[3]; } catch (_) {}
        }
        // Pending actions keep their rows faded until they settle.
        pending.forEach((id) => $(`.rw[data-id="${escId(id)}"]`)?.classList.add("leaving"));
        if (sel && !pending.has(sel) && !rowIds().includes(sel)) sel = null;
        markSel(false);
        if (openId) { const sh = sheetEl(); if (sh) setupSheet(sh); }
        showPill(false);
        force = false;
      } while (again);
    } catch (_) {
      // Server unreachable or mid-restart: the pill offers a reload, and the
      // next check tries again.
      showPill(true);
    } finally {
      refreshing = false;
    }
  }
  setInterval(async () => {
    if (document.hidden || refreshing || solo) return;
    try {
      const { v } = await (await fetch("/api/version", { signal: timeout(8000) })).json();
      if (v !== v0) refresh();
    } catch (_) {}
  }, 5000);
  addEventListener("pageshow", (e) => {
    if (!e.persisted) return;
    // Restored from the back-forward cache: it may be stale, and an item
    // page must take actions again.
    pending.clear();
    if (!solo) refresh(true);
  });

  // For the capture sheet (dictate.js).
  window.docketToast = toast;
  window.docketRefresh = () => refresh(true);

  // ---- email ------------------------------------------------------------------------------------
  // The digest: a line counts as seen once it has been on screen for a
  // moment, and seen lines fold into "Earlier" when the next check brings
  // new ones. Its buttons, Check now and Mark all read act in place.
  const seenQ = new Set(), seenTimers = new Map();
  let seenFlush = null;
  const flushSeen = () => {
    seenFlush = null;
    if (!seenQ.size) return;
    const body = new URLSearchParams();
    seenQ.forEach((id) => body.append("id", id));
    seenQ.clear();
    fetch("/email/seen", { method: "POST", body, headers: { "X-Requested-With": "fetch" } }).catch(() => {});
  };
  const seenObs = "IntersectionObserver" in window ? new IntersectionObserver((entries) => {
    for (const e of entries) {
      const id = e.target.dataset.thread;
      if (e.isIntersecting && !document.hidden) {
        if (!seenTimers.has(id)) seenTimers.set(id, setTimeout(() => {
          seenTimers.delete(id);
          seenObs.unobserve(e.target);
          seenQ.add(id);
          if (!seenFlush) seenFlush = setTimeout(flushSeen, 1500);
        }, 1200));
      } else if (seenTimers.has(id)) { clearTimeout(seenTimers.get(id)); seenTimers.delete(id); }
    }
  }, { threshold: 0.6 }) : null;
  const observeDigest = () => { if (seenObs && docket) $$(".esec .dg[data-thread]").forEach((li) => seenObs.observe(li)); };
  addEventListener("pagehide", flushSeen);
  const digestAct = async (b, note = "") => {
    if (b.disabled) return;
    b.disabled = true;
    let url, msg;
    const li = b.closest("[data-thread]");
    if (b.hasAttribute("data-email-check")) { url = "/email/check"; msg = "Checking your mail. New email appears here in a minute or two."; }
    else if (b.hasAttribute("data-email-readall")) {
      url = "/email/read-all?" + new URLSearchParams($$(".esec.dig .dg[data-thread]").map((x) => ["id", x.dataset.thread])).toString();
      msg = "Marked all read";
    }
    else {
      const a = b.dataset.emailAct;
      url = `/email/${encodeURIComponent(li.dataset.thread)}/${a}`;
      msg = { read: "Read", reply: "Moved to Email · needs you", noise: note ? "Off the digest. Triage reads your note from now on." : "Off the digest. Triage learns from it.", digest: "On the digest" }[a] || "Done";
      li.classList.add("leaving");
    }
    try {
      const res = await fetch(url, { method: "POST", body: new URLSearchParams(note ? { note } : {}), headers: { "X-Requested-With": "fetch" }, signal: timeout(15000) });
      if (!res.ok) throw new Error(String(res.status));
      toast(msg);
    } catch (_) {
      toast("That didn't work. Try again.", { err: true });
      if (li) li.classList.remove("leaving");
    }
    b.disabled = false;
    refresh(true);
  };
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-email-act], [data-email-check], [data-email-readall]");
    if (!b) return;
    e.preventDefault();
    e.stopPropagation();
    if (b.disabled || !nscrim.hidden) return; // the note is open: it has the page
    // "Not worth it" asks why first.
    if (b.dataset.emailAct === "noise") return openNote($(".dg-t", b.closest("[data-thread]"))?.textContent, true, (note) => digestAct(b, note));
    digestAct(b);
  }, true);

  // An email reply's dock and draft. Every button posts to /email/{thread}/…
  // and then reloads the open item, so it shows what the server now holds.
  const emailPost = async (thread, action, fields = {}) => {
    const body = new URLSearchParams(fields);
    const res = await fetch(`/email/${encodeURIComponent(thread)}/${action}`, { method: "POST", body, headers: { "X-Requested-With": "fetch" }, signal: timeout(150000) });
    const j = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(j.error || `failed (${res.status})`);
    return j;
  };
  const reloadSheet = async () => {
    if (solo) {
      // An item's own page: swap in a fresh sheet, so a toast (and its
      // Undo) stays on screen.
      try {
        const res = await fetch(location.href, { signal: timeout(15000) });
        const sh = res.ok && $(".sheet", new DOMParser().parseFromString(await res.text(), "text/html"));
        const cur = $(".sheet");
        if (sh && cur) { cur.replaceWith(sh); setupSheet(sh); return; }
      } catch (_) {}
      location.reload();
      return;
    }
    if (openId) openSheet(openId, "none");
    refresh(true);
  };
  const draftOf = (el) => { const sh = el.closest(".sheet") || document; return $("[data-email-draft]", sh); };
  const threadOf = (el) => { const d = draftOf(el); if (d) return d.dataset.thread; const p = $("[data-thread]", el.closest(".sheet") || document); return p && p.dataset.thread; };
  document.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-email-do], [data-email-undo], [data-email-revise-open], [data-email-revise-x], [data-email-edit-open], [data-email-edit-cancel]");
    if (!b) return;
    e.preventDefault();
    e.stopPropagation();
    const sh = b.closest(".sheet") || document;
    const d = draftOf(b);
    if (b.hasAttribute("data-email-revise-open")) { const f = $("[data-email-revise]", sh); if (f) { f.hidden = false; $("textarea", f).focus(); } return; }
    if (b.hasAttribute("data-email-revise-x")) { const f = $("[data-email-revise]", sh); if (f) f.hidden = true; return; }
    if (b.hasAttribute("data-email-edit-open") || b.hasAttribute("data-email-edit-cancel")) {
      const f = d && $("[data-email-edit]", d), view = d && $(":scope > .dr-b", d);
      if (!f) return;
      const opening = b.hasAttribute("data-email-edit-open");
      if (opening) { const ed = $(".dr-ed", f); ed.innerHTML = ed.dataset.html; }
      f.hidden = !opening;
      if (view) view.hidden = opening;
      // While you edit, Send would send the saved version, not your text.
      $$("[data-email-do], [data-email-edit-open], [data-email-revise-open]", sh).forEach((x) => { if (x.dataset.emailDo !== "redraft" && x.dataset.emailDo !== "retry") x.disabled = opening; });
      if (opening) $(".dr-ed", f).focus();
      return;
    }
    const thread = threadOf(b);
    if (!thread || b.disabled) return;
    b.disabled = true;
    try {
      if (b.hasAttribute("data-email-undo")) {
        await emailPost(thread, "undo-send", { outbox: b.dataset.emailUndo });
        toast("Not sent");
      } else {
        const a = b.dataset.emailDo;
        const j = await emailPost(thread, a, { version: b.dataset.version || (d && d.dataset.version) || "" });
        if (a === "send" || a === "holding") {
          const secs = Math.round((j.hold_ms || 30000) / 1000);
          toast(`${a === "holding" ? "Holding reply" : "Reply"} goes out in ${secs} s`, {
            ms: j.hold_ms || 30000,
            onUndo: async () => { try { await emailPost(thread, "undo-send", { outbox: j.outbox }); toast("Not sent"); } catch (err) { toast(err.message, { err: true }); } reloadSheet(); },
          });
        } else if (a === "save-draft") toast("Saved to your drafts");
        else if (a === "redraft") toast("Writing a new draft");
        else if (a === "retry") toast("Trying again");
      }
    } catch (err) { toast(err.message, { err: true }); }
    b.disabled = false;
    reloadSheet();
  }, true);
  document.addEventListener("submit", async (e) => {
    const f = e.target.closest("[data-email-revise], [data-email-edit]");
    if (!f) return;
    e.preventDefault();
    e.stopPropagation();
    const thread = threadOf(f), d = draftOf(f);
    if (!thread) return;
    const btn = $("button[type=submit]", f);
    if (btn) btn.disabled = true;
    try {
      if (f.hasAttribute("data-email-revise")) {
        const text = $("textarea", f).value.trim();
        if (!text) { toast("Say what should change", { err: true }); return; }
        await emailPost(thread, "revise", { text });
        toast("Asked for the changes. The new version appears here.");
      } else {
        await emailPost(thread, "edit", { version: d.dataset.version, seen: d.dataset.seen, html: $(".dr-ed", f).innerHTML, to: f.to.value, cc: f.cc.value });
        toast("Saved your edit");
      }
      reloadSheet();
    } catch (err) { toast(err.message, { err: true }); }
    finally { if (btn) btn.disabled = false; }
  }, true);
  // The Undo countdown, and a reload when a draft that was being written
  // (or a send in its Undo window) finishes.
  let emailV = null;
  setInterval(async () => {
    const sh = solo ? document : sheetEl();
    if (!sh || document.hidden) return;
    const p = $("[data-email-pending]", sh);
    if (p) {
      const left = $("[data-left]", p);
      if (left) { const n = Math.max(0, +left.textContent - 1); left.textContent = n; if (n === 0) setTimeout(reloadSheet, 1500); }
    }
    if (!$("[data-email-writing]", sh) || $("[data-email-edit]:not([hidden])", sh) || $("[data-email-revise]:not([hidden])", sh)) return;
    try {
      const { v } = await (await fetch("/api/version", { signal: timeout(8000) })).json();
      if (emailV !== null && v !== emailV) reloadSheet();
      emailV = v;
    } catch (_) {}
  }, 1000);

  // A question to the agent: the open item reloads when its answer arrives.
  // The baseline is the version the sheet was drawn at, so an answer that
  // lands before the first check still counts; the sheet keeps its scroll.
  setInterval(async () => {
    const sh = solo ? document : sheetEl();
    const w = sh && $("[data-asking]", sh);
    if (!w || document.hidden) return;
    const f = $("[data-reply] textarea", sh);
    if (f && f.value.trim()) return; // don't swap the sheet under a question being typed
    try {
      const { v } = await (await fetch("/api/version", { signal: timeout(8000) })).json();
      if (w.dataset.v && v !== w.dataset.v) {
        const box = solo ? document.scrollingElement : sheetScroller(sh);
        const top = box ? box.scrollTop : 0;
        if (solo) await reloadSheet();
        else if (openId) { await openSheet(openId, "none"); refresh(true); }
        const nb = solo ? document.scrollingElement : sheetScroller(sheetEl());
        if (nb) nb.scrollTop = top;
      }
    } catch (_) {}
  }, 5000);

  // ---- start --------------------------------------------------------------------------------------
  observeDigest();
  applyFolds();
  if (solo) { const sh = $(".sheet"); if (sh) setupSheet(sh); }
  // The toast an action on the last page left for this one.
  try {
    const c = JSON.parse(ss.get("docket.toast") || "null");
    ss.set("docket.toast", "");
    if (c && Date.now() - c.at < 5000) {
      const [u, d] = (c.undo || "").split(" ");
      // A held action's Undo window began on the last page: offer only what's left of it.
      const left = c.ms ? c.at + c.ms - Date.now() : 0;
      if (u && (!c.ms || left > 500)) toast(c.msg, { undo: u, decision: d || "", ms: c.ms ? left : 0 });
      else toast(c.msg);
    }
  } catch (_) {}
})();
