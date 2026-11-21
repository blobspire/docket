// docket: dictate (or type) an idea. The page records 16 kHz mono 16-bit PCM
// and uploads it in 3-second chunks while you talk. Every chunk waits in
// IndexedDB until the server has it, so a reload, a locked screen or a dead
// signal never loses what you said: whatever is left is sent on the next load.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const scrim = $("#dscrim"), form = $("#dsheet");
  if (!scrim || !form) return;
  const timeout = (ms) => (AbortSignal.timeout ? AbortSignal.timeout(ms) : undefined);
  const ls = {
    get: (k) => { try { return localStorage.getItem(k); } catch (_) { return null; } },
    set: (k, v) => { try { localStorage.setItem(k, v); } catch (_) {} },
  };
  const ver = (() => { try { return new URL(document.currentScript.src).searchParams.get("v") || ""; } catch (_) { return ""; } })();
  const toast = (m, o) => (window.docketToast ? window.docketToast(m, o) : console.log(m));
  const refresh = () => window.docketRefresh && window.docketRefresh();
  const recBtn = $("#ds-rec"), msg = $("#ds-msg"), timeEl = $("#ds-time"), bars = $$("#ds-lv i");
  const auto = $("#ds-auto"), text = $("#ds-text"), proj = $("#ds-p"), err = $("#ds-err"), sendBtn = $("#ds-send");
  const canRecord = form.dataset.mic === "1" && !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia) && !!(window.AudioContext || window.webkitAudioContext);
  if (recBtn && !canRecord) { recBtn.disabled = true; }

  const RATE = 16000, CHUNK = RATE * 3; // samples per upload
  const newID = () => {
    const a = "0123456789abcdefghjkmnpqrstvwxyz", b = new Uint8Array(8);
    crypto.getRandomValues(b);
    return "dict-" + [...b].map((x) => a[x % 32]).join("");
  };
  const post = (url, body, ms = 30000) => fetch(url, { method: "POST", headers: { "X-Requested-With": "fetch" }, body, signal: timeout(ms) });
  const form2 = (o) => { const p = new URLSearchParams(); for (const [k, v] of Object.entries(o)) if (v !== undefined && v !== null) p.set(k, v); return p; };

  // ---- the chunk store: IndexedDB, or memory where it isn't available -------------------------
  let dbp = null;
  const db = () => dbp || (dbp = new Promise((res) => {
    try {
      const r = indexedDB.open("docket-dictation", 1);
      r.onupgradeneeded = () => {
        r.result.createObjectStore("chunks", { keyPath: ["id", "seq"] });
        r.result.createObjectStore("dicts", { keyPath: "id" });
      };
      r.onsuccess = () => res(r.result);
      r.onerror = () => res(null);
    } catch (_) { res(null); }
  }));
  const mem = { chunks: new Map(), dicts: new Map() };
  const tx = async (store, mode, fn) => {
    const d = await db();
    if (!d) return fn(null);
    return new Promise((res) => {
      try {
        const t = d.transaction(store, mode), s = t.objectStore(store);
        const out = fn(s);
        t.oncomplete = () => res(out && out.result !== undefined ? out.result : out);
        t.onerror = t.onabort = () => res(null);
      } catch (_) { res(null); }
    });
  };
  const putChunk = (c) => tx("chunks", "readwrite", (s) => (s ? s.put(c) : mem.chunks.set(c.id + ":" + c.seq, c)));
  const dropChunk = (id, seq) => tx("chunks", "readwrite", (s) => (s ? s.delete([id, seq]) : mem.chunks.delete(id + ":" + seq)));
  const chunksOf = async (id) => {
    const all = await tx("chunks", "readonly", (s) => (s ? s.getAll(IDBKeyRange.bound([id, 0], [id, Infinity])) : null));
    const list = all || [...mem.chunks.values()].filter((c) => c.id === id);
    return list.sort((a, b) => a.seq - b.seq);
  };
  const putDict = (d) => tx("dicts", "readwrite", (s) => (s ? s.put(d) : mem.dicts.set(d.id, d)));
  const dropDict = (id) => tx("dicts", "readwrite", (s) => (s ? s.delete(id) : mem.dicts.delete(id)));
  const allDicts = async () => (await tx("dicts", "readonly", (s) => (s ? s.getAll() : null))) || [...mem.dicts.values()];

  // ---- uploading --------------------------------------------------------------------------------
  // One dictation's chunks go up in order; failures retry with a growing wait.
  const pumps = new Map(); // id -> promise of its upload finishing
  const created = new Set();
  const ensureCreated = async (d) => {
    if (created.has(d.id)) return;
    const res = await post("/dictation", form2({ id: d.id, source: "voice", auto_send: d.auto ? "1" : "0", project: d.project || "" }));
    if (!res.ok && res.status !== 409) throw new Error((await res.json().catch(() => ({}))).error || "HTTP " + res.status);
    if (res.status === 409) throw Object.assign(new Error((await res.json().catch(() => ({}))).error || "refused"), { fatal: true });
    created.add(d.id);
  };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  // The server is missing a chunk this page no longer has (memory-only
  // storage across a reload): close the gap rather than stall forever.
  const renumber = async (id, from) => {
    const list = await chunksOf(id);
    for (const c of list) await dropChunk(id, c.seq);
    let seq = from;
    for (const c of list) await putChunk({ id, seq: seq++, data: c.data });
  };
  // upload sends every stored chunk of d; finish=true also finishes it.
  // The server already finished this dictation (the page was away too
  // long, or another tab finished it): move what's left to a new one.
  const carryOn = async (d) => {
    const list = await chunksOf(d.id);
    const old = d.id;
    d.id = newID(); d.seq = list.length; d.cont = old;
    for (const c of list) await dropChunk(old, c.seq);
    let seq = 0;
    for (const c of list) await putChunk({ id: d.id, seq: seq++, data: c.data });
    await dropDict(old);
    await putDict(d);
    if (rec && rec.d === d) rec.seq = d.seq;
  };
  const upload = (d, finish) => {
    const prev = pumps.get(d.id) || Promise.resolve();
    const p = prev.then(async () => {
      let wait = 1000;
      for (;;) {
        try {
          await ensureCreated(d);
          d.beat = Date.now(); await putDict(d);
          for (const c of await chunksOf(d.id)) {
            const res = await post(`/dictation/${d.id}/audio?seq=${c.seq}`, c.data);
            const j = await res.json().catch(() => ({}));
            if (res.status === 410) { await carryOn(d); created.delete(d.id); break; }
            if (res.ok || (res.status === 409 && j.next > c.seq)) { await dropChunk(d.id, c.seq); continue; }
            if (res.status === 409 && typeof j.next === "number") {
              // An earlier chunk is missing: send it if this page has it, else close the gap.
              const have = (await chunksOf(d.id)).some((x) => x.seq === j.next);
              if (!have) await renumber(d.id, j.next);
              break;
            }
            throw Object.assign(new Error(j.error || "HTTP " + res.status), { fatal: res.status === 413 || res.status === 404 });
          }
          if ((await chunksOf(d.id)).length) { await sleep(200); continue; }
          if (!finish) return;
          const cur = (await allDicts()).find((x) => x.id === d.id) || d;
          const res = await post(`/dictation/${d.id}/finish`, form2({ auto_send: cur.auto ? "1" : "0", project: cur.project || "", text: cur.text || "" }), 60000);
          if (res.status === 410) { await carryOn(d); continue; } // typed words it didn't have: send them as a new one
          if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || "HTTP " + res.status);
          await dropDict(d.id);
          return;
        } catch (e) {
          if (e.fatal) { await dropDict(d.id); throw e; }
          await sleep(wait);
          wait = Math.min(wait * 2, 15000);
        }
      }
    });
    pumps.set(d.id, p.catch(() => {}));
    return p;
  };

  // ---- recording ----------------------------------------------------------------------------------
  let rec = null; // the recording in progress: {d, stream, ctx, node, pcm, n, seq, started, ...}
  let paused = null; // a dictation the locked screen interrupted, waiting to continue or send
  let review = null; // a dictation transcribing for the human to check
  let tick = 0;

  const setMsg = (t, bad = false) => { if (msg) { msg.textContent = t; msg.classList.toggle("bad", bad); } };
  const showTime = (ms) => { if (timeEl) { const s = Math.floor(ms / 1000); timeEl.textContent = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`; } };
  const level = (rms) => {
    const v = Math.min(1, rms * 9);
    bars.forEach((b, i) => { const w = 1 - Math.abs(i - 3) / 4; b.style.height = `${4 + Math.round(18 * v * (0.45 + w * 0.55) * (0.75 + Math.random() * 0.5))}px`; });
  };

  // A resampler from the device rate to 16 kHz: each output sample is the
  // average of the input samples it covers (enough of a low-pass for speech).
  const resampler = (from) => {
    const step = from / RATE;
    let acc = 0, cnt = 0, pos = 0;
    return (input, out) => {
      for (let i = 0; i < input.length; i++) {
        acc += input[i]; cnt++; pos += 1;
        if (pos >= step) {
          pos -= step;
          const v = Math.max(-1, Math.min(1, acc / cnt));
          out.push(v < 0 ? v * 0x8000 : v * 0x7fff);
          acc = 0; cnt = 0;
        }
      }
    };
  };

  // flush stores full chunks (all=true: what's left too) and uploads them.
  // Flushes run one after another, so finishing waits for every chunk.
  const flush = (r, all) => (r.q = r.q.then(async () => {
    while (r.pcm.length >= CHUNK || (all && r.pcm.length)) {
      const part = r.pcm.splice(0, CHUNK);
      await putChunk({ id: r.d.id, seq: r.seq++, data: new Int16Array(part).buffer });
      r.d.seq = r.seq;
      await putDict(r.d);
    }
    upload(r.d, false).catch(() => {});
  }));

  let starting = false;
  async function startRec(d, stream) {
    if (rec || starting) return;
    starting = true;
    err.textContent = "";
    // Made (and resumed) inside the tap: Safari only lets audio start from one.
    const AC = window.AudioContext || window.webkitAudioContext;
    const ctx = new AC();
    ctx.resume().catch(() => {});
    try {
      stream = stream || await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 } });
    } catch (e) {
      starting = false;
      ctx.close().catch(() => {});
      setMsg(e && e.name === "NotAllowedError" ? "The microphone is blocked for this site. Allow it in your browser's settings, or type instead." : "Couldn't open the microphone: " + (e && e.message || e), true);
      return;
    }
    if (!isOpen()) { // closed while the microphone was being opened
      starting = false;
      stream.getTracks().forEach((t) => t.stop());
      ctx.close().catch(() => {});
      return;
    }
    await ctx.resume().catch(() => {});
    const src = ctx.createMediaStreamSource(stream);
    const r = { d, stream, ctx, src, pcm: [], seq: d.seq || 0, started: Date.now(), before: d.ms || 0, q: Promise.resolve() };
    const resample = resampler(ctx.sampleRate);
    const onFrames = (f) => {
      if (rec !== r) return;
      resample(f, r.pcm);
      let sum = 0; for (let i = 0; i < f.length; i += 4) sum += f[i] * f[i];
      level(Math.sqrt(sum / (f.length / 4)));
      if (r.pcm.length >= CHUNK) flush(r, false);
    };
    const mute = ctx.createGain(); mute.gain.value = 0; mute.connect(ctx.destination);
    try {
      await ctx.audioWorklet.addModule("/static/pcm-worklet.js?v=" + ver);
      r.node = new AudioWorkletNode(ctx, "pcm-tap");
      r.node.port.onmessage = (e) => onFrames(e.data);
    } catch (_) {
      // Older browsers: the deprecated ScriptProcessor does the same job.
      r.node = ctx.createScriptProcessor(4096, 1, 1);
      r.node.onaudioprocess = (e) => onFrames(new Float32Array(e.inputBuffer.getChannelData(0)));
    }
    if (!isOpen()) { // closed while the recorder was being set up
      starting = false;
      stream.getTracks().forEach((t) => t.stop());
      ctx.close().catch(() => {});
      return;
    }
    src.connect(r.node); r.node.connect(mute);
    rec = r; paused = null; starting = false;
    await putDict(d);
    upload(d, false).catch(() => {}); // creates it on the server now
    try { r.lock = await navigator.wakeLock?.request("screen"); } catch (_) {}
    form.classList.add("rec");
    recBtn.classList.add("on");
    recBtn.setAttribute("aria-label", "Stop recording");
    setMsg("Listening… tap to stop.");
    clearInterval(tick);
    let lastBeat = 0;
    tick = setInterval(() => {
      const ms = r.before + Date.now() - r.started;
      if (Date.now() - lastBeat > 5000) { lastBeat = Date.now(); r.d.beat = lastBeat; putDict(r.d); }
      showTime(ms);
      if (ms > 59.5 * 60 * 1000) stopRec(true); // an hour per dictation
      if (r.stream.getAudioTracks().every((t) => t.readyState === "ended")) interrupted();
    }, 250);
  }

  // stopTracks ends the microphone and saves what's left; the dictation
  // itself stays open (to continue it, or finish it).
  async function stopTracks() {
    const r = rec;
    if (!r) return null;
    rec = null;
    clearInterval(tick);
    r.stream.getTracks().forEach((t) => t.stop());
    try { r.src.disconnect(); r.node.disconnect(); } catch (_) {}
    r.ctx.close().catch(() => {});
    r.lock?.release?.().catch(() => {});
    r.d.ms = r.before + Date.now() - r.started;
    await flush(r, true);
    form.classList.remove("rec");
    recBtn.classList.remove("on");
    recBtn.setAttribute("aria-label", "Start recording");
    level(0);
    return r.d;
  }

  // The screen locked or the page went to the background: browsers stop the
  // microphone. Keep what was said; offer to continue or send.
  async function interrupted() {
    const d = await stopTracks();
    if (!d) return;
    paused = d;
    setMsg("Paused when the screen locked. Tap to keep going, or Send.", true);
  }

  // stopRec ends the recording and finishes the dictation. send forces it to
  // go to the router whatever the toggle says; closing (the sheet is going
  // away) skips the review, leaving a draft on the Ideas page.
  async function stopRec(send = false, closing = false) {
    const t = text.value.trim(), p = proj.value;
    const d = (await stopTracks()) || paused;
    if (!d) return;
    paused = null;
    d.auto = send || !auto || auto.checked;
    d.project = p;
    d.text = t;
    d.done = true;
    await putDict(d);
    if (d.auto || closing) {
      if (!closing) close(true);
      toast(d.auto ? "Got it: transcribing, then an agent picks it up." : "Saved: it's on the Ideas page to check and send.");
      upload(d, true).then(refresh, (e) => toast("Your recording couldn't be sent: " + e.message, { err: true }));
      return;
    }
    review = d;
    setMsg("Transcribing…");
    recBtn.disabled = true;
    try {
      await upload(d, true);
      for (let i = 0; i < 600 && review === d; i++) {
        const j = await (await fetch(`/dictation/${d.id}`, { signal: timeout(8000) })).json().catch(() => ({}));
        if (j.status === "draft" || j.status === "sent" || j.status === "routed" || j.status === "routing") {
          text.value = j.text || text.value;
          setMsg(j.status === "draft" ? "Check it, fix anything misheard, then Send." : "Sent.");
          text.focus();
          break;
        }
        if (j.status === "failed") { setMsg(j.error || "Transcribing failed; it's on the Ideas page to retry.", true); break; }
        await sleep(1000);
      }
    } catch (e) {
      setMsg("Couldn't send the recording: " + e.message, true);
    } finally {
      recBtn.disabled = !canRecord;
    }
  }

  // ---- the sheet -----------------------------------------------------------------------------------
  const isOpen = () => !scrim.hidden;
  function open(record = false) {
    if (isOpen()) return;
    err.textContent = "";
    scrim.hidden = false;
    document.body.classList.add("ds-open");
    if (auto) auto.checked = ls.get("docket.autosend") !== "0";
    // On a project's page, the idea is probably for that project.
    const p = new URLSearchParams(location.search).get("p");
    proj.value = p && [...proj.options].some((o) => o.value === p) ? p : proj.value;
    showTime(0); level(0);
    setMsg(canRecord ? "Tap to talk. Say which project and what you want; ramble freely." : (form.dataset.mic ? "This browser can't record here; type it instead." : ""));
    if (record && canRecord) startRec({ id: newID(), seq: 0, ms: 0, auto: true, project: proj.value, created: Date.now() });
    else if (canRecord && recBtn) recBtn.focus();
    else text.focus();
  }
  // close hides the sheet. A recording in progress is finished per the
  // toggle (never thrown away); a draft under review stays on the Ideas page.
  function close(quiet = false) {
    if (!isOpen()) return;
    if (rec || paused || starting) stopRec(false, true);
    else if (review && !quiet) toast("Saved as a draft on the Ideas page.");
    review = null;
    scrim.hidden = true;
    document.body.classList.remove("ds-open");
    text.value = "";
    setMsg("");
  }

  async function sendTyped() {
    const t = text.value.trim();
    if (rec || paused) return stopRec(true);
    if (review) {
      const res = await post(`/dictation/${review.id}/do/send`, form2({ text: t, project: proj.value }));
      if (!res.ok) { err.textContent = (await res.json().catch(() => ({}))).error || "Couldn't send it"; return; }
      review = null;
      close(true);
      toast("Sent: an agent is on it.");
      return refresh();
    }
    if (!t) { err.textContent = canRecord ? "Say or type something first." : "Type the idea first."; return; }
    sendBtn.disabled = true;
    try {
      const res = await post("/dictation", form2({ id: newID(), source: "text", text: t, project: proj.value }));
      if (!res.ok) { err.textContent = (await res.json().catch(() => ({}))).error || "Couldn't send it"; return; }
      close(true);
      toast("Sent: an agent is on it.");
      refresh();
    } catch (_) {
      err.textContent = "docket isn't reachable right now; try again in a moment.";
    } finally {
      sendBtn.disabled = false;
    }
  }

  form.addEventListener("submit", (e) => { e.preventDefault(); sendTyped(); });
  auto?.addEventListener("change", () => ls.set("docket.autosend", auto.checked ? "1" : "0"));
  recBtn?.addEventListener("click", () => {
    if (rec) return stopRec(false);
    if (paused) return startRec(paused);
    if (review) return;
    startRec({ id: newID(), seq: 0, ms: 0, auto: !auto || auto.checked, project: proj.value, created: Date.now() });
  });
  let downOn = null;
  scrim.addEventListener("pointerdown", (e) => { downOn = e.target; });
  scrim.addEventListener("click", (e) => { if (e.target === scrim && downOn === scrim) close(); });
  $("[data-ds-close]", form)?.addEventListener("click", () => close());
  document.addEventListener("keydown", (e) => {
    if (!isOpen()) return;
    if (e.key === "Escape") { e.preventDefault(); close(); }
    else if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); sendTyped(); }
  });
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-dictate]");
    if (!b) return;
    e.preventDefault();
    open(b.classList.contains("fab"));
  });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden && rec) flush(rec, true);
    if (!document.hidden && rec && rec.stream.getAudioTracks().every((t) => t.readyState === "ended")) interrupted();
  });

  // ---- the Ideas page's buttons ----------------------------------------------------------------------
  document.addEventListener("submit", async (e) => {
    const f = e.target;
    if (!f.matches || !f.matches("form[data-ia]")) return;
    e.preventDefault();
    // A button's own formaction, else the form's (button.formAction alone
    // returns the page's URL when the attribute is absent).
    const btn = e.submitter, url = (btn && btn.hasAttribute("formaction") && btn.formAction) || f.action;
    const body = new URLSearchParams(new FormData(f));
    $$("button", f).forEach((b) => { b.disabled = true; });
    try {
      const res = await post(url, body);
      const j = await res.json().catch(() => ({}));
      if (!res.ok) toast(j.error || "That didn't work", { err: true });
      else toast(({ send: "Sent: an agent is on it.", retry: "Trying again.", delete: "Deleted.", research: "An agent will research it.", build: "An agent will build it.", save: "Saved for later.", dismiss: "Dismissed." })[url.split("/").pop()] || "Done.");
    } catch (_) {
      toast("docket isn't reachable right now", { err: true });
    }
    refresh();
  });
  // A link to one idea opens its card.
  const openHash = () => { const d = location.hash && document.getElementById(location.hash.slice(1)); if (d && d.tagName === "DETAILS") { d.open = true; d.scrollIntoView({ block: "center" }); } };
  addEventListener("hashchange", openHash);
  openHash();

  // ---- recordings an earlier page left unsent ---------------------------------------------------------
  (async () => {
    // Only what no open tab is still recording or sending (each beats every few seconds).
    const left = (await allDicts()).filter((d) => (!rec || d.id !== rec.d.id) && Date.now() - (d.beat || d.created || 0) > 30000);
    for (const d of left) {
      if (!d.done) d.auto = d.auto !== false; // the page went away mid-recording: finish it as it was set up
      upload(d, true).then(() => { toast("A recording from earlier reached docket."); refresh(); }, () => {});
    }
  })();

  window.docketDictate = { open, close, isOpen, start: (stream) => { open(false); return startRec({ id: newID(), seq: 0, ms: 0, auto: !auto || auto.checked, project: proj.value, created: Date.now() }, stream); }, stop: () => stopRec(false) };
})();
