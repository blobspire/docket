package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// Tall tables under a header just under the column cap, and a long body
// built from many additions, render in bounded time and size.
func TestTallTablesAndLongBodiesAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("|" + strings.Repeat("h|", 64) + "\n|" + strings.Repeat("-|", 64) + "\n")
	for b.Len() < 20<<10 {
		b.WriteString("x\n")
	}
	tall := b.String()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	renderMarkdown(tall)
	runtime.ReadMemStats(&m1)
	if a := m1.TotalAlloc - m0.TotalAlloc; a > 40<<20 {
		t.Fatalf("a 20 KB tall table allocated %d MB", a>>20)
	}
	for _, s := range []string{tall, strings.Repeat(tall+"\n\n", 50)} {
		start := time.Now()
		out := renderMarkdown(s)
		if d := time.Since(start); d > 2*time.Second || len(out) > 8*len(s)+(2<<20) {
			t.Fatalf("%d bytes rendered to %d in %v", len(s), len(out), d)
		}
	}
	if !strings.Contains(string(renderMarkdown("|"+strings.Repeat("h|", 64)+"\n|"+strings.Repeat("-|", 64)+"\n|"+strings.Repeat("1|", 64))), "<table>") {
		t.Error("a 64-column table with a row no longer renders")
	}
	if strings.Contains(string(renderMarkdown("|"+strings.Repeat("h|", 70)+"\n|"+strings.Repeat("-|", 70)+"\n")), "<table>") {
		t.Error("a 70-column table rendered as a table")
	}
	long := strings.Repeat("word ", 20000) // 100 KB: the start renders, the rest is text
	if out := string(renderMarkdown("**start** " + long)); !strings.Contains(out, "<strong>start</strong>") || !strings.Contains(out, "<pre>") {
		t.Error("a long body should render its start and show the rest as text")
	}
}

// A pending answer can be undone even after an earlier note reached the
// agent; an answer queued in the same second as a Done survives its undo; a
// note's receipt doesn't make the item "delivered".
func TestAnswersSurviveNotes(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-r4a00001")
	e.s.Decide("dk-r4a00001", Action{Kind: "done"})
	st.DB.Exec(`UPDATE deliveries SET status='sent', created_at=created_at-5 WHERE item_id='dk-r4a00001'`)
	e.s.Decide("dk-r4a00001", Action{Kind: "undo"})
	st.DB.Exec(`UPDATE deliveries SET status='sent', created_at=created_at-5 WHERE item_id='dk-r4a00001'`)
	e.s.Decide("dk-r4a00001", Action{Kind: "choose", Option: 0})
	if err := e.s.Decide("dk-r4a00001", Action{Kind: "undo"}); err != nil {
		t.Fatalf("undoing a pending answer after an earlier note: %v", err)
	}

	ask(e, "dk-r4a00002")
	e.s.Decide("dk-r4a00002", Action{Kind: "choose", Option: 0})
	e.s.Decide("dk-r4a00002", Action{Kind: "done"}) // same second
	e.s.Decide("dk-r4a00002", Action{Kind: "undo"})
	ds, _ := st.ItemDeliveries("dk-r4a00002")
	if len(ds) != 1 || !strings.Contains(ds[0].Text, store.AnswerMark) {
		t.Fatalf("after undoing a Done in the answer's second: %+v", ds)
	}

	st.UpsertItem(store.Item{ID: "dk-r4a00003", Kind: "ask", Title: "deploy?", Project: "w", SessionID: "s", Machine: "box", Source: "cli", Blocking: true, Options: []string{"a", "b"}})
	e.s.Decide("dk-r4a00003", Action{Kind: "done"})
	st.DB.Exec(`UPDATE deliveries SET status='sent' WHERE item_id='dk-r4a00003'`)
	e.s.Decide("dk-r4a00003", Action{Kind: "undo"}) // a loud "reopened" note
	e.s.Decide("dk-r4a00003", Action{Kind: "choose", Option: 1})
	ds, _ = st.ItemDeliveries("dk-r4a00003")
	confirmed := 0
	for _, d := range ds {
		if strings.Contains(d.Text, "isn't done after all") {
			st.ConfirmDelivery(d.Nonce)
			confirmed++
		}
	}
	if confirmed != 1 {
		t.Fatalf("test setup: %d reopened notes", confirmed)
	}
	if it := status(t, st, "dk-r4a00003"); it.Status != "answered" {
		t.Fatalf("a note's receipt made the item %s", it.Status)
	}
}

// The toast's Undo takes back the decision it reported, not whatever another
// tab did since.
func TestUndoNamesItsDecision(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("other", "")
	st.UpsertItem(store.Item{ID: "dk-r4u00001", Kind: "issue", Title: "x", Project: "w"})
	post := func(form url.Values) map[string]string {
		req := httptest.NewRequest("POST", "/item/dk-r4u00001/act", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "fetch")
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, req)
		var out map[string]string
		json.NewDecoder(rec.Body).Decode(&out)
		return out
	}
	moved := post(url.Values{"action": {"reclassify"}, "project": {"other"}})
	if moved["decision"] == "" {
		t.Fatalf("no decision id: %v", moved)
	}
	post(url.Values{"action": {"done"}}) // another tab, since
	if out := post(url.Values{"action": {"undo"}, "decision": {moved["decision"]}}); out["error"] == "" {
		t.Fatal("a stale Undo undid the newer decision")
	}
	if it := status(t, st, "dk-r4u00001"); it.Status != "done" || it.Project != "other" {
		t.Fatalf("after the refused undo: %s in %s", it.Status, it.Project)
	}
}

// A PDF is served as exactly application/pdf, never as a raw type a browser
// might read another way; anything else is sandboxed.
func TestPDFIsServedExactly(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-r4p00001", Kind: "review", Title: "paper", Project: "w"})
	sha := putBlob(t, e, "dk-r4p00001", "paper.pdf", "application/pdf", []byte("%PDF-1.4"), 8)
	for typ, wantPDF := range map[string]bool{"application/pdf": true, " application/pdf": true, "\u000bapplication/pdf": true, "application/pdfx": false, "application/pdf;;x": false} {
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/blob/"+sha+"?type="+url.QueryEscape(typ), nil))
		ct, csp := rec.Header().Get("Content-Type"), rec.Header().Get("Content-Security-Policy")
		if wantPDF && (ct != "application/pdf" || csp != "") || !wantPDF && csp != "sandbox" {
			t.Errorf("type %q: Content-Type %q, CSP %q", typ, ct, csp)
		}
	}
}

// A closed watchdog item can be kept; a login-failure item, like a watchdog
// one, can't be stashed while open.
func TestStashOfDocketsOwnItems(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r4w00001", Kind: "health", Title: "lead down", Project: "admin", Source: ingest.SrcWatchdog})
	e.s.Decide("dk-r4w00001", Action{Kind: "done"})
	if err := e.s.Decide("dk-r4w00001", Action{Kind: "stash"}); err != nil {
		t.Errorf("stashing a closed watchdog item: %v", err)
	}
	st.UpsertItem(store.Item{ID: "dk-r4w00002", Kind: "health", Title: "login failed", Project: "admin", Source: ingest.SrcFailure})
	if err := e.s.Decide("dk-r4w00002", Action{Kind: "stash"}); err == nil {
		t.Error("an open login-failure item was stashed")
	}
	if d := dockHTML(t, e, "dk-r4w00002"); strings.Contains(d, `data-act="stash"`) {
		t.Error("the dock offers Stash on a login-failure item")
	}
}

// A cleared or resumed session (a new id) on the same machine closes its
// delivered item by the exact key; another machine, or a month-old item,
// doesn't match.
func TestDoneByKeyFromANewSession(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	fileDelivered(t, e, "old", "dk-r4k00001", "q", "export")
	e.send(t, record.Record{Event: "cli.done", SessionID: "new", Done: &record.Done{Key: "export", Project: "q", Explicit: true}}, ingest.Options{})
	if it := status(t, st, "dk-r4k00001"); it.Status != "done" {
		t.Fatalf("a new session on the same machine didn't close it: %s", it.Status)
	}
	fileDelivered(t, e, "old", "dk-r4k00002", "q", "import")
	e.sendFrom(t, "elsewhere", record.Record{Event: "cli.done", SessionID: "x", Done: &record.Done{Key: "import", Project: "q", Explicit: true}})
	if it := status(t, st, "dk-r4k00002"); it.Status != "delivered" {
		t.Fatalf("another machine closed it: %s", it.Status)
	}
	st.DB.Exec(`UPDATE items SET updated_at=? WHERE id='dk-r4k00002'`, time.Now().Add(-30*24*time.Hour).Unix())
	e.send(t, record.Record{Event: "cli.done", SessionID: "new2", Done: &record.Done{Key: "import", Project: "q", Explicit: true}}, ingest.Options{})
	if it := status(t, st, "dk-r4k00002"); it.Status != "delivered" {
		t.Fatalf("a month-old item was closed: %s", it.Status)
	}
}

// Opening the server clears stash times an older docket left on open items.
func TestOpenClearsLeftoverStashTimes(t *testing.T) {
	e := newEnv(t, "")
	ask(e, "dk-r4o00001")
	e.s.st.DB.Exec(`UPDATE items SET stashed_at=? WHERE id='dk-r4o00001'`, time.Now().Unix())
	s2, err := Open(e.s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if it, _, _ := s2.st.GetItem("dk-r4o00001"); it.StashedAt != 0 {
		t.Fatal("server start kept the stash time on an open item")
	}
}

// What a folder name may hold before the resume command is withheld.
func TestPrintable(t *testing.T) {
	for s, want := range map[string]bool{
		"/home/cole/code/vlm teacher": true, "/tmp/é": true,
		"/tmp/a\x1b[2J": false, "/tmp/a\u009b2J": false, "/tmp/a\x7f": false, "/tmp/a b": false,
		"/tmp/a\xff": false, "/tmp/a\tb": false,
	} {
		if printable(s) != want {
			t.Errorf("printable(%q) = %v", s, !want)
		}
	}
}

// "Never confirmed" needs a retried send that's been out a while, and the
// docket refreshes on each retry.
func TestStuckNeedsRetriesAndTime(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-r4s00001")
	e.s.Decide("dk-r4s00001", Action{Kind: "choose", Option: 0})
	set := func(attempts int, ago time.Duration) {
		st.DB.Exec(`UPDATE deliveries SET status='sent', attempts=?, sent_at=? WHERE item_id='dk-r4s00001'`, attempts, time.Now().Add(-ago).Unix())
	}
	stuck := func() bool { return strings.Contains(rowHTML(t, get(e, "/?p=w"), "dk-r4s00001"), `class="stuck"`) }
	set(1, 2*time.Hour)
	if stuck() {
		t.Error("stuck after one attempt")
	}
	set(4, 5*time.Minute)
	if stuck() {
		t.Error("stuck five minutes after sending")
	}
	before := st.QueueDigest()
	set(3, 5*time.Minute)
	if st.QueueDigest() == before {
		t.Error("the digest didn't change with the attempts")
	}
}

// With two deliveries made in the same second, the later one is the latest.
func TestLatestDeliveryBreaksTies(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-r4t00001")
	at := time.Now().Unix()
	st.AddDelivery(store.Delivery{ID: "dl-zzz", ItemID: "dk-r4t00001", Machine: "box", SessionID: "s", Text: "first " + store.AnswerMark, Nonce: "n1"})
	st.AddDelivery(store.Delivery{ID: "dl-aaa", ItemID: "dk-r4t00001", Machine: "box", SessionID: "s", Text: "second " + store.AnswerMark, Nonce: "n2"})
	st.DB.Exec(`UPDATE deliveries SET created_at=? WHERE item_id='dk-r4t00001'`, at)
	if d := st.LatestDeliveries([]string{"dk-r4t00001"})["dk-r4t00001"]; d.Nonce != "n2" {
		t.Fatalf("latest = %s, want the second", d.Nonce)
	}
}
