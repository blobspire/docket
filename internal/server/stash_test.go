package server

import (
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

func quietNotes(t *testing.T, st *store.Store, id string) []store.Delivery {
	t.Helper()
	ds, _ := st.ItemDeliveries(id)
	var out []store.Delivery
	for _, d := range ds {
		if d.LiveOnly && d.Quiet {
			out = append(out, d)
		}
	}
	return out
}

// Done means one thing on every item: it closes it, and the agent that
// filed it hears so quietly at its next turn, in words that fit what it
// asked. An agent paused on a task hears straight away. Nothing goes to an
// agent that already has the human's answer, or to docket's own items.
func TestDoneTellsTheAgentQuietly(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	for _, c := range []struct {
		id, kind, want string
	}{
		{"dk-dn000001", "review", "they've looked at it"},
		{"dk-dn000002", "ask", "don't take it as a yes"},
		{"dk-dn000003", "approval", "don't take it as a yes"},
		{"dk-dn000004", "issue", "marked your docket item"},
	} {
		st.UpsertItem(store.Item{ID: c.id, Kind: c.kind, Title: "t " + c.id, Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
		if err := e.s.Decide(c.id, Action{Kind: "done", Text: "nice"}); err != nil {
			t.Fatal(err)
		}
		qs := quietNotes(t, st, c.id)
		if len(qs) != 1 || !strings.Contains(qs[0].Text, c.want) || !strings.Contains(qs[0].Text, "Their note: nice") {
			t.Errorf("%s (%s): quiet notes = %+v", c.id, c.kind, qs)
		}
	}
	// A task the agent is paused on: a regular delivery, which resumes it.
	st.UpsertItem(store.Item{ID: "dk-dn000005", Kind: "human_task", Title: "plug it in", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-dn000005", Action{Kind: "done"})
	if ds, _ := st.ItemDeliveries("dk-dn000005"); len(ds) != 1 || ds[0].LiveOnly || !strings.Contains(ds[0].Text, "marked your docket item done:") {
		t.Errorf("task deliveries = %+v", ds)
	}
	// Already answered: the agent has what it needs.
	st.UpsertItem(store.Item{ID: "dk-dn000006", Kind: "ask", Title: "which?", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-dn000006", Action{Kind: "answer", Text: "the first"})
	st.DB.Exec(`UPDATE items SET status='delivered' WHERE id='dk-dn000006'`)
	if err := e.s.Decide("dk-dn000006", Action{Kind: "done"}); err != nil {
		t.Fatalf("done on a delivered item: %v", err)
	}
	if ds, _ := st.ItemDeliveries("dk-dn000006"); len(ds) != 1 {
		t.Errorf("done after an answer sent the agent more: %+v", ds)
	}
	// docket's own item: no agent to tell.
	st.UpsertItem(store.Item{ID: "dk-dn000007", Kind: "health", Title: "box unreachable", Project: "w", Source: "watchdog"})
	e.s.Decide("dk-dn000007", Action{Kind: "done"})
	if ds, _ := st.ItemDeliveries("dk-dn000007"); len(ds) != 0 {
		t.Errorf("a health item's done was sent somewhere: %+v", ds)
	}
	// Undoing the done withdraws the note before it goes.
	e.s.Decide("dk-dn000001", Action{Kind: "undo"})
	if qs := quietNotes(t, st, "dk-dn000001"); len(qs) != 0 {
		t.Errorf("the undone done's note is still queued: %+v", qs)
	}
}

func onPage(e *env, path, id string) bool { return strings.Contains(get(e, path), `data-id="`+id+`"`) }

// Stashing an open item closes it as done (telling its agent, as Done
// does) and keeps it on the Stash, off the docket and out of Done. Undo
// brings back exactly what was there, and takes back the note.
func TestStashAnOpenItem(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-st000001", Kind: "review", Title: "Model A results", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	if err := e.s.Decide("dk-st000001", Action{Kind: "stash"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := st.GetItem("dk-st000001")
	if it.Status != "done" || it.StashedAt == 0 || len(quietNotes(t, st, it.ID)) != 1 {
		t.Fatalf("stashed: status %s stashed_at %d notes %d", it.Status, it.StashedAt, len(quietNotes(t, st, it.ID)))
	}
	if !onPage(e, "/stash", it.ID) || onPage(e, "/done", it.ID) || onPage(e, "/", it.ID) {
		t.Fatal("a stashed item should be on the Stash only")
	}
	if !strings.Contains(get(e, "/stash"), "You stashed it") {
		t.Error("the Stash row doesn't say how it got there")
	}
	if err := e.s.Decide(it.ID, Action{Kind: "stash"}); err == nil {
		t.Error("stashed twice")
	}
	if err := e.s.Decide(it.ID, Action{Kind: "undo"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ = st.GetItem(it.ID)
	if it.Status != "open" || it.StashedAt != 0 || it.Resolution != "" || len(quietNotes(t, st, it.ID)) != 0 {
		t.Fatalf("after undo: status %s stashed_at %d resolution %q notes %d", it.Status, it.StashedAt, it.Resolution, len(quietNotes(t, st, it.ID)))
	}
	if !onPage(e, "/", it.ID) {
		t.Fatal("the unstashed item isn't back on the docket")
	}
}

// The toast's Stash after a Done keeps the done (and its note to the agent);
// undoing the stash only takes it off the Stash.
func TestStashAfterDone(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-st000002", Kind: "review", Title: "Model A results", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-st000002", Action{Kind: "done"})
	st.DB.Exec(`UPDATE deliveries SET created_at=created_at-5 WHERE item_id='dk-st000002'`) // the done came first
	if err := e.s.Decide("dk-st000002", Action{Kind: "stash"}); err != nil {
		t.Fatalf("stash after done: %v", err)
	}
	if it, _, _ := st.GetItem("dk-st000002"); it.Status != "done" || it.StashedAt == 0 {
		t.Fatalf("after stash: %+v", it)
	}
	e.s.Decide("dk-st000002", Action{Kind: "undo"})
	it, _, _ := st.GetItem("dk-st000002")
	if it.Status != "done" || it.StashedAt != 0 || it.ResolvedAt == 0 || len(quietNotes(t, st, it.ID)) != 1 {
		t.Fatalf("after undoing the stash: status %s stashed_at %d resolved %d notes %d", it.Status, it.StashedAt, it.ResolvedAt, len(quietNotes(t, st, it.ID)))
	}
	if !onPage(e, "/done", it.ID) {
		t.Fatal("it isn't back in Done")
	}
}

// An answered item can be stashed without touching its answer; Remove moves
// it to Done; Back to docket from the Stash puts a stashed open item back.
func TestStashRemoveAndBackToDocket(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-st000003", Kind: "ask", Title: "order the gripper?", Project: "w", SessionID: "s", Machine: "box", Source: "cli", Options: []string{"Order it", "Wait"}})
	e.s.Decide("dk-st000003", Action{Kind: "choose", Option: 0})
	if err := e.s.Decide("dk-st000003", Action{Kind: "stash"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-st000003"); it.Status != "answered" || it.StashedAt == 0 {
		t.Fatalf("stashing an answered item changed it: %+v", it)
	}
	if ds, _ := st.ItemDeliveries("dk-st000003"); len(ds) != 1 || ds[0].Status != "pending" {
		t.Fatalf("the answer isn't still on its way: %+v", ds)
	}
	if onPage(e, "/", "dk-st000003") || !strings.Contains(get(e, "/stash"), "You chose “Order it”") {
		t.Fatal("the stashed answer should show on the Stash, with the choice")
	}
	if err := e.s.Decide("dk-st000003", Action{Kind: "unstash"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-st000003"); it.Status != "done" || it.StashedAt != 0 || !onPage(e, "/done", it.ID) {
		t.Fatalf("Remove didn't move it to Done: %+v", it)
	}

	st.UpsertItem(store.Item{ID: "dk-st000004", Kind: "review", Title: "ablation", Project: "w"})
	e.s.Decide("dk-st000004", Action{Kind: "stash"})
	if err := e.s.Decide("dk-st000004", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-st000004"); it.Status != "open" || it.StashedAt != 0 || !onPage(e, "/", it.ID) {
		t.Fatalf("Back to docket from the Stash: %+v", it)
	}
	// Done, then stashed: Back to docket undoes both.
	e.s.Decide("dk-st000004", Action{Kind: "done"})
	e.s.Decide("dk-st000004", Action{Kind: "stash"})
	if err := e.s.Decide("dk-st000004", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-st000004"); it.Status != "open" || it.StashedAt != 0 {
		t.Fatalf("Back to docket after done and stash: %+v", it)
	}
}

// After 30 days an item leaves the Stash for Done, by itself.
func TestStashLastsThirtyDays(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-st000005", Kind: "review", Title: "old results", Project: "w"})
	e.s.Decide("dk-st000005", Action{Kind: "stash"})
	old := time.Now().Add(-31 * 24 * time.Hour).Unix()
	st.DB.Exec(`UPDATE items SET stashed_at=?, resolved_at=? WHERE id='dk-st000005'`, old, old)
	if onPage(e, "/stash", "dk-st000005") || !onPage(e, "/done", "dk-st000005") {
		t.Fatal("a 31-day-old stash should have moved to Done")
	}
	if strings.Contains(get(e, "/"), `class="tc"`) {
		t.Error("the nav still counts it on the Stash")
	}
}

// An agent closing an item whose answer reached it takes it off the
// docket's "Waiting on agents".
func TestAgentDoneClosesADeliveredItem(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s", record.Item{ID: "dk-ad000001", Kind: "ask", Title: "which GPU?", Project: "w", Options: []string{"A100", "H100"}})
	if err := e.s.Decide("dk-ad000001", Action{Kind: "choose", Option: 0}); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`UPDATE items SET status='delivered' WHERE id='dk-ad000001'`)
	if !onPage(e, "/?p=w", "dk-ad000001") {
		t.Fatal("the delivered answer isn't waiting on the docket")
	}
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{ItemID: "dk-ad000001", Note: "using the A100"}}, ingest.Options{})
	if it, _, _ := st.GetItem("dk-ad000001"); it.Status != "done" || onPage(e, "/?p=w", "dk-ad000001") {
		t.Fatalf("the agent's done didn't close it: %+v", it)
	}
}
