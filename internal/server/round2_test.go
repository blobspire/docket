package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// fileDelivered files a keyed ask from a session, answers it and confirms
// the delivery.
func fileDelivered(t *testing.T, e *env, sid, id, project, key string) {
	t.Helper()
	e.file(t, sid, record.Item{ID: id, Kind: "ask", Title: "q " + id, Project: project, Key: key, Options: []string{"a", "b"}})
	if err := e.s.Decide(id, Action{Kind: "choose", Option: 0}); err != nil {
		t.Fatal(err)
	}
	confirm(t, e.s.st, id)
}

// `docket done --key` closes the caller's own delivered item, never another
// session's, another project's, or an old one.
func TestDoneByKeyClosesTheCallersOwn(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	fileDelivered(t, e, "sA", "dk-r2k00001", "w", "deploy")
	fileDelivered(t, e, "sB", "dk-r2k00002", "w", "deploy")
	st.DB.Exec(`UPDATE items SET updated_at=updated_at+60 WHERE id='dk-r2k00001'`) // A's is the most recent
	e.send(t, record.Record{Event: "cli.done", SessionID: "sB", Done: &record.Done{Key: "deploy", Project: "w", Explicit: true, Note: "deployed B"}}, ingest.Options{})
	if a, b := status(t, st, "dk-r2k00001"), status(t, st, "dk-r2k00002"); a.Status != "delivered" || b.Status != "done" {
		t.Fatalf("A %s, B %s: B's done should close B's item only", a.Status, b.Status)
	}

	// Another project's delivered item with the same agent key, from another
	// session: never matched by agent key.
	fileDelivered(t, e, "sC", "dk-r2k00003", "proj-a", "tests")
	e.send(t, record.Record{Event: "cli.done", SessionID: "sD", Done: &record.Done{Key: "tests", Note: "ran them"}}, ingest.Options{})
	if c := status(t, st, "dk-r2k00003"); c.Status != "delivered" {
		t.Fatalf("another session's item in another project was closed: %s", c.Status)
	}

	// A session's own delivered item wins over another session's open one.
	fileDelivered(t, e, "sE", "dk-r2k00004", "w", "build")
	e.file(t, "sF", record.Item{ID: "dk-r2k00005", Kind: "ask", Title: "build?", Project: "other", Key: "build"})
	e.send(t, record.Record{Event: "cli.done", SessionID: "sE", Done: &record.Done{Key: "build", Note: "built"}}, ingest.Options{})
	if own, other := status(t, st, "dk-r2k00004"), status(t, st, "dk-r2k00005"); own.Status != "done" || other.Status != "open" {
		t.Fatalf("own %s, other %s", own.Status, other.Status)
	}
}

// An open item with a stash time an older docket left behind is an open
// item: the full dock, Stash works, Remove doesn't apply; startup clears it.
func TestLeftoverStashTimeOnAnOpenItem(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-r2l00001")
	st.DB.Exec(`UPDATE items SET stashed_at=? WHERE id='dk-r2l00001'`, time.Now().Unix())
	d := dockHTML(t, e, "dk-r2l00001")
	if !strings.Contains(d, `data-act="choose"`) || !strings.Contains(d, `data-act="done"`) || strings.Contains(d, `data-act="unstash"`) {
		t.Fatalf("dock for an open item with a leftover stash time: %s", d)
	}
	if err := e.s.Decide("dk-r2l00001", Action{Kind: "unstash"}); err == nil {
		t.Error("Remove closed an open item")
	}
	if err := e.s.Decide("dk-r2l00001", Action{Kind: "stash"}); err != nil {
		t.Errorf("stash refused: %v", err)
	}
	ask(e, "dk-r2l00002")
	st.DB.Exec(`UPDATE items SET stashed_at=? WHERE id='dk-r2l00002'`, time.Now().Unix())
	if err := st.ClearStaleStash(); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-r2l00002"); it.StashedAt != 0 {
		t.Fatal("startup cleanup kept the stash time")
	}
}

func sentNotes(t *testing.T, st *store.Store, id, want string) int {
	t.Helper()
	n := 0
	ds, _ := st.ItemDeliveries(id)
	for _, d := range ds {
		if strings.Contains(d.Text, want) {
			n++
		}
	}
	return n
}

// "It isn't done after all" goes only when the undone decision's own done
// note reached the agent, loud or quiet.
func TestReopenedNoteOnlyAfterItsDoneNote(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	markSent := func(id string) { st.DB.Exec(`UPDATE deliveries SET status='sent' WHERE item_id=?`, id) }
	back := func(id string) { st.DB.Exec(`UPDATE deliveries SET created_at=created_at-5 WHERE item_id=?`, id) }

	// A noise note isn't a done note.
	st.UpsertItem(store.Item{ID: "dk-r2n00001", Kind: "review", Title: "flaky", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r2n00001", Action{Kind: "noise"})
	markSent("dk-r2n00001")
	e.s.Decide("dk-r2n00001", Action{Kind: "undo"})
	back("dk-r2n00001")
	e.s.Decide("dk-r2n00001", Action{Kind: "done"})
	e.s.Decide("dk-r2n00001", Action{Kind: "undo"}) // its note never left
	if n := sentNotes(t, st, "dk-r2n00001", "isn't done after all"); n != 0 {
		t.Fatalf("%d reopened notes after a noise note", n)
	}

	// An earlier done's note doesn't count for a later one.
	st.UpsertItem(store.Item{ID: "dk-r2n00002", Kind: "review", Title: "results", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r2n00002", Action{Kind: "done"})
	markSent("dk-r2n00002")
	e.s.Decide("dk-r2n00002", Action{Kind: "undo"}) // one reopened note
	back("dk-r2n00002")
	e.s.Decide("dk-r2n00002", Action{Kind: "stash"})
	e.s.Decide("dk-r2n00002", Action{Kind: "undo"}) // its note was withdrawn
	if n := sentNotes(t, st, "dk-r2n00002", "isn't done after all"); n != 1 {
		t.Fatalf("%d reopened notes, want 1", n)
	}

	// A loud done (a task) that was sent: the agent hears it's open again.
	st.UpsertItem(store.Item{ID: "dk-r2n00003", Kind: "human_task", Title: "plug in", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r2n00003", Action{Kind: "done"})
	markSent("dk-r2n00003")
	e.s.Decide("dk-r2n00003", Action{Kind: "undo"})
	if n := sentNotes(t, st, "dk-r2n00003", "isn't done after all"); n != 1 {
		t.Fatalf("%d reopened notes after a sent task done, want 1", n)
	}
}

// Two Back to docket clicks at once (two tabs) take it off the Stash once;
// the answer underneath stays.
func TestDoubleBackToDocketKeepsTheAnswer(t *testing.T) {
	for i := 0; i < 20; i++ {
		e := newEnv(t, "")
		st := e.s.st
		ask(e, "dk-r2d00001")
		e.s.Decide("dk-r2d00001", Action{Kind: "choose", Option: 0})
		e.s.Decide("dk-r2d00001", Action{Kind: "stash"})
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); e.s.Decide("dk-r2d00001", Action{Kind: "reopen"}) }()
		}
		wg.Wait()
		if it := status(t, st, "dk-r2d00001"); it.Status != "answered" || it.Answer == "" || it.StashedAt != 0 {
			t.Fatalf("after two Back to docket: status %s answer %q stashed %d", it.Status, it.Answer, it.StashedAt)
		}
	}
}

// The docket refreshes when an answer turns out never to have been confirmed.
func TestDigestSeesAnUnconfirmedAnswer(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-r2g00001")
	e.s.Decide("dk-r2g00001", Action{Kind: "choose", Option: 0})
	st.DB.Exec(`UPDATE deliveries SET status='sent', attempts=4, sent_at=? WHERE item_id='dk-r2g00001'`, time.Now().Add(-10*time.Minute).Unix())
	before := st.QueueDigest()
	st.DB.Exec(`UPDATE deliveries SET sent_at=? WHERE item_id='dk-r2g00001'`, time.Now().Add(-20*time.Minute).Unix())
	if st.QueueDigest() == before {
		t.Fatal("the digest didn't change when the answer went unconfirmed")
	}
}

// A crafted id is replaced the same way every time, so a resent filing is
// the same item.
func TestReplacedIDIsStable(t *testing.T) {
	e := newEnv(t, "")
	for i := 0; i < 2; i++ {
		e.file(t, "s", record.Item{ID: "bad id/../x", Kind: "review", Title: "crafted", Project: "w"})
	}
	items, _ := e.s.st.Items()
	n := 0
	for _, it := range items {
		if it.Title == "crafted" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d items from one filing sent twice", n)
	}
}
