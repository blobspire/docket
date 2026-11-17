package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// A session folder with control characters gets no resume command: quoting
// stops the shell, not a terminal reading escape codes from the paste.
func TestResumeCommandRefusesControlCharacters(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.DB.Exec(`INSERT INTO sessions(id, machine, cwd, state) VALUES ('44444444-dddd-4ddd-8ddd-000000000004','box',?,'ended')`, "/tmp/x\x1b[201~\x15touch PWNED\r")
	st.UpsertItem(store.Item{ID: "dk-r3r00001", Kind: "ask", Title: "q", Project: "w", SessionID: "44444444-dddd-4ddd-8ddd-000000000004", Machine: "box", Source: "cli"})
	if s := sheetHTML(t, e, "dk-r3r00001"); strings.Contains(s, "data-copy") {
		t.Fatal("a resume command was offered for a folder with control characters")
	}
}

// A very wide markdown table can't blow the page up.
func TestWideMarkdownTableIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("|" + strings.Repeat("h|", 2700) + "\n|" + strings.Repeat("-|", 2700) + "\n")
	for b.Len() < 23<<10 {
		b.WriteString("x\n")
	}
	start := time.Now()
	out := renderMarkdown(b.String())
	if d := time.Since(start); d > 2*time.Second || len(out) > 4<<20 {
		t.Fatalf("rendered %d bytes in %v", len(out), d)
	}
	if !strings.Contains(string(renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |")), "<table>") {
		t.Fatal("an ordinary table no longer renders")
	}
}

// Undoing a loud done (a task) that reached the agent tells it loudly; a
// dropped done note (the session was gone) isn't followed by a reopened one;
// undoing a stash whose done note was sent tells it too.
func TestReopenedNoteMatchesItsDoneNote(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	markSent := func(id string) { st.DB.Exec(`UPDATE deliveries SET status='sent' WHERE item_id=?`, id) }
	reopened := func(id string) []store.Delivery {
		var out []store.Delivery
		ds, _ := st.ItemDeliveries(id)
		for _, d := range ds {
			if strings.Contains(d.Text, "isn't done after all") {
				out = append(out, d)
			}
		}
		return out
	}
	st.UpsertItem(store.Item{ID: "dk-r3n00001", Kind: "human_task", Title: "power-cycle it", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r3n00001", Action{Kind: "done"})
	markSent("dk-r3n00001")
	e.s.Decide("dk-r3n00001", Action{Kind: "undo"})
	if r := reopened("dk-r3n00001"); len(r) != 1 || r[0].LiveOnly {
		t.Fatalf("after a loud done: reopened notes %+v, want one loud", r)
	}

	st.UpsertItem(store.Item{ID: "dk-r3n00002", Kind: "review", Title: "results", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r3n00002", Action{Kind: "done"})
	st.DB.Exec(`UPDATE deliveries SET status='dropped' WHERE item_id='dk-r3n00002'`)
	e.s.Decide("dk-r3n00002", Action{Kind: "undo"})
	if r := reopened("dk-r3n00002"); len(r) != 0 {
		t.Fatalf("a dropped done note was followed by %d reopened notes", len(r))
	}

	st.UpsertItem(store.Item{ID: "dk-r3n00003", Kind: "review", Title: "ablation", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r3n00003", Action{Kind: "stash"})
	markSent("dk-r3n00003")
	e.s.Decide("dk-r3n00003", Action{Kind: "undo"})
	if r := reopened("dk-r3n00003"); len(r) != 1 || !r[0].LiveOnly {
		t.Fatalf("after a stash's quiet done: reopened notes %+v, want one quiet", r)
	}
}

// Attachments show their real size; .markdown renders; a text document opens
// as text; a PDF opens in the browser's viewer; everything else is sandboxed.
func TestAttachmentDetails(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-r3a00001", Kind: "review", Title: "report", Project: "w"})
	putBlob(t, e, "dk-r3a00001", "notes.markdown", "", []byte("**bold**"), 999999999)
	s := sheetHTML(t, e, "dk-r3a00001")
	if !strings.Contains(s, "<strong>bold</strong>") || strings.Contains(s, "953.7 MB") || !strings.Contains(s, ">8 B<") {
		t.Fatalf("attachment: %s", s[strings.Index(s, "Attachments"):])
	}
	if !strings.Contains(s, "type=text%2fplain") {
		t.Error("a text document's open link doesn't serve it as text")
	}
	sha := putBlob(t, e, "dk-r3a00001", "paper.pdf", "application/pdf", []byte("%PDF-1.4"), 8)
	for typ, sandboxed := range map[string]bool{"application%2Fpdf": false, "APPLICATION%2FPDF": false, "text%2Fhtml": true, "": true} {
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/blob/"+sha+"?type="+typ, nil))
		if got := rec.Header().Get("Content-Security-Policy") == "sandbox"; got != sandboxed {
			t.Errorf("type %q: sandboxed=%v, want %v", typ, got, sandboxed)
		}
	}
	// Bigger than the inline cap, whatever it declares: a link, not inline.
	putBlob(t, e, "dk-r3a00001", "huge.txt", "text/plain", []byte(strings.Repeat("y", 300<<10)), 10)
	if s := sheetHTML(t, e, "dk-r3a00001"); strings.Contains(s, strings.Repeat("y", 1000)) {
		t.Error("a 300 KB text file declared as 10 bytes was shown inline")
	}
}

// docket closes its own watchdog items when they clear: they can't be stashed.
func TestWatchdogItemsCantBeStashed(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-r3w00001", Kind: "health", Title: "lead cwant is down", Project: "admin", Source: ingest.SrcWatchdog})
	if err := e.s.Decide("dk-r3w00001", Action{Kind: "stash"}); err == nil {
		t.Fatal("a watchdog item was stashed")
	}
	if d := dockHTML(t, e, "dk-r3w00001"); strings.Contains(d, `data-act="stash"`) {
		t.Error("the dock offers Stash on a watchdog item")
	}
}

// Crafted ids that start like real ones are replaced too.
func TestCraftedIDWithAPlainPrefix(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s", record.Item{ID: "dk-abc/../blob/x?type=text/html", Kind: "review", Title: "crafted2", Project: "w"})
	items, _ := e.s.st.Items()
	for _, it := range items {
		if it.Title == "crafted2" && strings.ContainsAny(it.ID, "/?.") {
			t.Fatalf("crafted id kept: %q", it.ID)
		}
	}
}

// A stashed answer that was moved (a reclassify on top of the stash) still
// comes back to the docket, off the Stash.
func TestBackToDocketAfterAMove(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("other", "")
	ask(e, "dk-r3m00001")
	e.s.Decide("dk-r3m00001", Action{Kind: "choose", Option: 0})
	confirm(t, st, "dk-r3m00001")
	e.s.Decide("dk-r3m00001", Action{Kind: "stash"})
	st.DB.Exec(`UPDATE items SET updated_at=? WHERE id='dk-r3m00001'`, time.Now().Add(-48*time.Hour).Unix())
	if err := e.s.Decide("dk-r3m00001", Action{Kind: "reclassify", Project: "other"}); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`UPDATE items SET updated_at=? WHERE id='dk-r3m00001'`, time.Now().Add(-48*time.Hour).Unix())
	if err := e.s.Decide("dk-r3m00001", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-r3m00001"); it.StashedAt != 0 || !onPage(e, "/", it.ID) {
		t.Fatalf("after Back to docket: stashed %d, on docket %v", it.StashedAt, onPage(e, "/", it.ID))
	}
}

// A named project rules out every other project's item for done --key, and
// the agent key has to match.
func TestDoneByKeyRespectsTheProjectAndKey(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	fileDelivered(t, e, "s", "dk-r3k00001", "w", "gpu")
	fileDelivered(t, e, "s", "dk-r3k00002", "w", "cpu")
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{Key: "gpu", Project: "other", Explicit: true}}, ingest.Options{})
	if it := status(t, st, "dk-r3k00001"); it.Status != "delivered" {
		t.Fatalf("done --key with another project closed it: %s", it.Status)
	}
	st.DB.Exec(`UPDATE items SET updated_at=updated_at+60 WHERE id='dk-r3k00001'`) // the wrong one is the most recent
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{Key: "cpu"}}, ingest.Options{})
	if a, b := status(t, st, "dk-r3k00001"), status(t, st, "dk-r3k00002"); a.Status != "delivered" || b.Status != "done" {
		t.Fatalf("done --key cpu: gpu %s, cpu %s", a.Status, b.Status)
	}
}

// A snoozed item stashed is marked done, and its agent told.
func TestStashASnoozedItem(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r3s00001", Kind: "review", Title: "results", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	st.Snooze("dk-r3s00001", time.Now().Add(time.Hour).Unix())
	if err := e.s.Decide("dk-r3s00001", Action{Kind: "stash"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-r3s00001"); it.Status != "done" || it.StashedAt == 0 || len(quietNotes(t, st, it.ID)) != 1 {
		t.Fatalf("stashed snoozed item: status %s stashed %d notes %d", it.Status, it.StashedAt, len(quietNotes(t, st, it.ID)))
	}
}

// Done rows say both what you chose and how the agent closed it; folds are
// keyed per view; no page loads anything from another site.
func TestDoneRowsFoldsAndNoThirdParty(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s", record.Item{ID: "dk-r3d00001", Kind: "ask", Title: "deploy?", Project: "w", Options: []string{"Tonight", "Tomorrow"}})
	e.s.Decide("dk-r3d00001", Action{Kind: "choose", Option: 0})
	confirm(t, st, "dk-r3d00001")
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{ItemID: "dk-r3d00001", Note: "deployed tonight"}}, ingest.Options{})
	if row := rowHTML(t, get(e, "/done"), "dk-r3d00001"); !strings.Contains(row, "You chose “Tonight”; the agent closed it: deployed tonight") {
		t.Fatalf("Done row: %s", row)
	}
	ask(e, "dk-r3d00002")
	e.s.Decide("dk-r3d00002", Action{Kind: "choose", Option: 0})
	if !strings.Contains(get(e, "/"), `data-wt="p:w"`) || !strings.Contains(get(e, "/?p=w"), `data-wt="view:w"`) {
		t.Error("fold keys aren't per view")
	}
	for _, path := range []string{"/", "/item/dk-r3d00002", "/stash", "/done", "/health"} {
		if p := get(e, path); strings.Contains(p, "googleapis") || strings.Contains(p, "gstatic") {
			t.Errorf("%s loads from a third party", path)
		}
	}
}

// Reopening something the agent closed tells it it still needs something.
func TestReopeningAnAgentCloseTellsTheAgent(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s", record.Item{ID: "dk-r3c00001", Kind: "issue", Title: "flaky CI", Project: "w"})
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{ItemID: "dk-r3c00001", Note: "fixed"}}, ingest.Options{})
	if err := e.s.Decide("dk-r3c00001", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	qs := quietNotes(t, st, "dk-r3c00001")
	if len(qs) != 1 || !strings.Contains(qs[0].Text, "which you closed") {
		t.Fatalf("notes = %+v", qs)
	}
}

// Static files: cached for good only at the current version; no listings,
// no template sources.
func TestStaticFiles(t *testing.T) {
	e := newEnv(t, "")
	h := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Header().Get("Cache-Control")
	}
	if code, cc := h("/static/app.css?v=" + staticVer); code != 200 || !strings.Contains(cc, "immutable") {
		t.Errorf("current css: %d %q", code, cc)
	}
	if _, cc := h("/static/app.css?v=old"); strings.Contains(cc, "immutable") {
		t.Error("a stale version is cached for good")
	}
	if code, cc := h("/static/inter.woff2?v=" + fontVer); code != 200 || !strings.Contains(cc, "immutable") {
		t.Errorf("font: %d %q", code, cc)
	}
	for _, p := range []string{"/static/", "/static/layout.html"} {
		if code, _ := h(p); code != 404 {
			t.Errorf("%s = %d, want 404", p, code)
		}
	}
	if page := get(e, "/"); !strings.Contains(page, "/static/app.css?v="+staticVer) || !strings.Contains(page, "inter.woff2?v="+fontVer) {
		t.Error("the page doesn't link the current versions")
	}
}

// Nowhere offers to stash an item docket closes itself.
func TestNoStashOfferForWatchdogItems(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r3x00001", Kind: "health", Title: "lead down", Project: "admin", Source: ingest.SrcWatchdog})
	e.s.Decide("dk-r3x00001", Action{Kind: "done"})
	if q := quickHTML(rowHTML(t, get(e, "/done"), "dk-r3x00001")); strings.Contains(q, `data-act="stash"`) {
		t.Error("the Done row offers Stash")
	}
	if d := dockHTML(t, e, "dk-r3x00001"); strings.Contains(d, `data-act="stash"`) {
		t.Error("the closed item's dock offers Stash")
	}
	if !strings.Contains(sheetHTML(t, e, "dk-r3x00001"), "data-nostash") {
		t.Error("the sheet doesn't say not to offer Stash after an action")
	}
}
