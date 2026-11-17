package server

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// rowHTML is one item's <li> from a rendered page.
func rowHTML(t *testing.T, page, id string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<li class="rw" data-id="` + id + `">.*?</li>`).FindString(page)
	if m == "" {
		t.Fatalf("no row for %s", id)
	}
	return m
}

// sheetHTML is an item's sheet (its page), dock included.
func sheetHTML(t *testing.T, e *env, id string) string {
	t.Helper()
	page := get(e, "/item/"+id)
	i := strings.Index(page, `<div class="sheet"`)
	if i < 0 {
		t.Fatalf("no sheet for %s", id)
	}
	return page[i:]
}

// dockHTML is the answer dock of an item's sheet.
func dockHTML(t *testing.T, e *env, id string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<footer class="dock">.*?</footer>`).FindString(sheetHTML(t, e, id))
	if m == "" {
		t.Fatalf("no dock for %s", id)
	}
	return m
}

func get(e *env, path string) string {
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Body.String()
}

// A move between a close and its Reopen would hide the close from the
// Reopen, leaving its "marked done" queued for an item that's open again.
func TestClosedItemCantBeMoved(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("ops", "")
	st.UpsertItem(store.Item{ID: "dk-r4c00001", Kind: "human_task", Title: "plug in", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	if err := e.s.Decide("dk-r4c00001", Action{Kind: "done"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Action{{Kind: "reclassify", Urgency: "soon"}, {Kind: "reclassify", Project: "ops"}} {
		if err := e.s.Decide("dk-r4c00001", a); err == nil {
			t.Fatalf("a closed item was moved (%+v)", a)
		}
	}
	if strings.Contains(sheetHTML(t, e, "dk-r4c00001"), "data-reclassify") {
		t.Error("a closed item's card still offers the move selects")
	}
	if err := e.s.Decide("dk-r4c00001", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := st.GetItem("dk-r4c00001")
	if ds, _ := st.ItemDeliveries("dk-r4c00001"); it.Status != "open" || len(ds) != 0 {
		t.Fatalf("after reopen: status %s, %d deliveries still queued", it.Status, len(ds))
	}
}

// Reopening what an agent closed is itself logged, so it can be undone,
// and the item lands back among the closed with a close time.
func TestReopenOfAnAgentCloseCanBeUndone(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s", record.Item{ID: "dk-r4o00001", Kind: "issue", Title: "flaky CI", Project: "w"})
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{ItemID: "dk-r4o00001", Note: "fixed it"}}, ingest.Options{})
	if it, _, _ := st.GetItem("dk-r4o00001"); it.Status != "done" {
		t.Fatalf("the agent's done didn't land: %+v", it)
	}
	if err := e.s.Decide("dk-r4o00001", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-r4o00001"); it.Status != "open" || it.ResolvedAt != 0 {
		t.Fatalf("reopened item = status %s, resolved_at %d", it.Status, it.ResolvedAt)
	}
	if err := e.s.Decide("dk-r4o00001", Action{Kind: "undo"}); err != nil {
		t.Fatalf("the reopen can't be undone: %v", err)
	}
	if it, _, _ := st.GetItem("dk-r4o00001"); it.Status != "done" || it.ResolvedAt == 0 {
		t.Fatalf("after undoing the reopen: status %s, resolved_at %d", it.Status, it.ResolvedAt)
	}
}

// The agent's expiry was its guess; once the human reopens the item, the
// next tick mustn't close it again.
func TestReopenClearsAnExpiry(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	past := time.Now().Add(-time.Hour).Unix()
	st.UpsertItem(store.Item{ID: "dk-r4e00001", Kind: "ask", Title: "still need the GPU?", Project: "w", ExpiresAt: past})
	if err := st.SetStatus("dk-r4e00001", "done", "", ""); err != nil { // closed by its agent before the tick
		t.Fatal(err)
	}
	if err := e.s.Decide("dk-r4e00001", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	st.Expire(time.Now().Unix())
	if it, _, _ := st.GetItem("dk-r4e00001"); it.Status != "open" {
		t.Fatalf("the reopened item expired again: %s", it.Status)
	}
}

// Merging an archived project into a live one mustn't archive the live one
// just because the merge form's box was ticked for the old project.
func TestMergeKeepsTheTargetsArchiveFlag(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("live", "")
	st.EnsureProject("old", "")
	st.SetProject("old", "", true)
	st.UpsertItem(store.Item{ID: "dk-r4m00001", Kind: "issue", Title: "x", Project: "old"})
	req := httptest.NewRequest("POST", "/project/old", strings.NewReader("rename=live&archived=1&old_archived=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	e.s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if it, _, _ := st.GetItem("dk-r4m00001"); it.Project != "live" {
		t.Fatalf("not merged: %+v", it)
	}
	if p, _ := st.GetProject("live"); p.Archived {
		t.Fatal("merging an archived project archived the target")
	}
}

// "unsorted" is where items without a project land, never somewhere to
// move one.
func TestMoveListLeavesOutUnsorted(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r4l00001", Kind: "issue", Title: "placed", Project: "w"})
	st.UpsertItem(store.Item{ID: "dk-r4l00002", Kind: "issue", Title: "loose", Project: store.Unsorted})
	row := sheetHTML(t, e, "dk-r4l00001")
	if !strings.Contains(row, "data-reclassify") {
		t.Fatal("no move select on an open item (test can't see the list)")
	}
	if strings.Contains(row, ">unsorted</option>") {
		t.Error(`"unsorted" offered as a place to move an item`)
	}
}

// The Done page reopens (undoing a human close, or reopening an agent's)
// rather than undoing whatever was logged last.
func TestDonePageOffersReopen(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r4d00001", Kind: "issue", Title: "x", Project: "w"})
	st.SetStatus("dk-r4d00001", "done", "", "")
	row := rowHTML(t, get(e, "/done"), "dk-r4d00001")
	if !strings.Contains(row, `data-act="reopen"`) || strings.Contains(row, `data-act="undo"`) {
		t.Fatalf("Done row actions: %s", row)
	}
}

// An answer whose session never comes back would otherwise sit on the
// dashboard for good: it can be dismissed, and the answer stays queued.
func TestAnAnsweredItemCanBeDismissed(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r4a00001", Kind: "ask", Title: "which GPU?", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	if err := e.s.Decide("dk-r4a00001", Action{Kind: "answer", Text: "the A100"}); err != nil {
		t.Fatal(err)
	}
	page := get(e, "/?p=w")
	row := rowHTML(t, page, "dk-r4a00001")
	if !strings.Contains(row, `data-act="dismiss"`) || strings.Contains(row, `data-act="done"`) || !strings.Contains(row, "Waiting") {
		t.Fatalf("answered row actions: %s", row)
	}
	if w := strings.Index(page, `class="wt"`); w < 0 || w > strings.Index(page, `data-id="dk-r4a00001"`) {
		t.Fatal("the answered item isn't under Waiting on agents")
	}
	if err := e.s.Decide("dk-r4a00001", Action{Kind: "dismiss"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := st.GetItem("dk-r4a00001")
	ds, _ := st.ItemDeliveries("dk-r4a00001")
	if it.Status != "dismissed" || len(ds) != 1 || ds[0].Status != "pending" {
		t.Fatalf("after dismiss: status %s, deliveries %+v", it.Status, ds)
	}
}

// A blob its machine no longer has is asked for once, not on every pull.
func TestAGoneBlobIsNotAskedForAgain(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r4b00001", Kind: "review", Title: "see the plot", Project: "w", Machine: "box"})
	sha := strings.Repeat("ab", 32)
	st.AddEvidence(store.Evidence{SHA256: sha, ItemID: "dk-r4b00001", Name: "plot.png", Mime: "image/png"})
	m, _ := e.s.cfg.Machine("box")
	e.s.fetchBlobs(context.Background(), m)
	if missing, _ := st.MissingBlobs("box"); len(missing) != 0 {
		t.Fatalf("still asking for a blob that's gone: %v", missing)
	}
	if !strings.Contains(get(e, "/item/dk-r4b00001"), "gone from its machine") {
		t.Error("the item page still says it's being copied")
	}
}
