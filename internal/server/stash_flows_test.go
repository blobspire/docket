package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// confirm marks an item's latest answer delivery as received, as the hook
// does when the agent sees it.
func confirm(t *testing.T, st *store.Store, id string) {
	t.Helper()
	ds, _ := st.ItemDeliveries(id)
	for _, d := range ds {
		if !d.LiveOnly {
			if ok, err := st.ConfirmDelivery(d.Nonce); err != nil || !ok {
				t.Fatalf("confirm %s: ok=%v err=%v", id, ok, err)
			}
		}
	}
}

func status(t *testing.T, st *store.Store, id string) store.Item {
	t.Helper()
	it, ok, _ := st.GetItem(id)
	if !ok {
		t.Fatalf("no item %s", id)
	}
	return it
}

func ask(e *env, id string) {
	e.s.st.UpsertItem(store.Item{ID: id, Kind: "ask", Title: "which GPU? " + id, Project: "w", SessionID: "s", Machine: "box", Source: "cli", Options: []string{"A100", "H100"}})
}

// A stash of an answered item doesn't change its status, so undoing it (by
// Back to docket) mustn't undo the delivery that confirmed in between.
func TestUnstashKeepsAConfirmedDelivery(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-sf000001")
	e.s.Decide("dk-sf000001", Action{Kind: "choose", Option: 0})
	if err := e.s.Decide("dk-sf000001", Action{Kind: "stash"}); err != nil {
		t.Fatal(err)
	}
	confirm(t, st, "dk-sf000001")
	if err := e.s.Decide("dk-sf000001", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-sf000001"); it.Status != "delivered" || it.StashedAt != 0 || !onPage(e, "/", it.ID) {
		t.Fatalf("Back to docket after the delivery confirmed: status %s stashed %d", it.Status, it.StashedAt)
	}

	// Remove, a confirmation, then Undo: back on the Stash, still delivered.
	ask(e, "dk-sf000002")
	e.s.Decide("dk-sf000002", Action{Kind: "choose", Option: 0})
	e.s.Decide("dk-sf000002", Action{Kind: "stash"})
	e.s.Decide("dk-sf000002", Action{Kind: "unstash"})
	confirm(t, st, "dk-sf000002")
	if err := e.s.Decide("dk-sf000002", Action{Kind: "undo"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-sf000002"); it.Status != "delivered" || it.StashedAt == 0 {
		t.Fatalf("undoing Remove after the delivery confirmed: status %s stashed %d", it.Status, it.StashedAt)
	}
}

// What the agent did after a stash (closing it) isn't undone by taking it
// off the Stash: the item reopens cleanly, without the agent's note.
func TestBackToDocketAfterTheAgentClosedIt(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s", record.Item{ID: "dk-sf000003", Kind: "ask", Title: "which GPU?", Project: "w", Options: []string{"A100", "H100"}})
	e.s.Decide("dk-sf000003", Action{Kind: "choose", Option: 0})
	confirm(t, st, "dk-sf000003")
	e.s.Decide("dk-sf000003", Action{Kind: "stash"})
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{ItemID: "dk-sf000003", Note: "used the A100"}}, ingest.Options{})
	if it := status(t, st, "dk-sf000003"); it.Status != "done" {
		t.Fatalf("the agent's done didn't land: %s", it.Status)
	}
	if err := e.s.Decide("dk-sf000003", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-sf000003"); it.Status != "open" || it.Resolution != "" || it.StashedAt != 0 {
		t.Fatalf("after Back to docket: status %s resolution %q stashed %d", it.Status, it.Resolution, it.StashedAt)
	}
}

// Back to docket undoes the human's close even with a Stash and a Remove
// logged on top of it: the queued "marked done" goes, and a "not worth it"
// stops feeding rules.md.
func TestBackToDocketLooksPastTheStash(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	pending := func(id string) int {
		n := 0
		ds, _ := st.ItemDeliveries(id)
		for _, d := range ds {
			if d.Status == "pending" {
				n++
			}
		}
		return n
	}
	st.UpsertItem(store.Item{ID: "dk-sf000004", Kind: "human_task", Title: "plug in the drive", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-sf000004", Action{Kind: "stash"}) // closes it, telling the agent
	e.s.Decide("dk-sf000004", Action{Kind: "unstash"})
	if pending("dk-sf000004") != 1 {
		t.Fatal("test setup: no queued done note")
	}
	if err := e.s.Decide("dk-sf000004", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-sf000004"); it.Status != "open" || pending("dk-sf000004") != 0 {
		t.Fatalf("stash, Remove, Back to docket: status %s, %d notes still queued", it.Status, pending("dk-sf000004"))
	}

	st.UpsertItem(store.Item{ID: "dk-sf000005", Kind: "review", Title: "flaky results", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-sf000005", Action{Kind: "noise", Text: "don't send these"})
	e.s.Decide("dk-sf000005", Action{Kind: "stash"})
	e.s.Decide("dk-sf000005", Action{Kind: "unstash"})
	if err := e.s.Decide("dk-sf000005", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	ns, _ := st.RecentNoise(10, false)
	if it := status(t, st, "dk-sf000005"); it.Status != "open" || len(ns) != 0 || pending("dk-sf000005") != 0 {
		t.Fatalf("noise, stash, Remove, Back: status %s, noise marks %d, notes queued %d", it.Status, len(ns), pending("dk-sf000005"))
	}
	if strings.Contains(e.s.RulesMarkdown(), "flaky results") {
		t.Fatal("rules.md still carries the taken-back mark")
	}

	// An answered item that expired while stashed: back to answered, not open.
	ask(e, "dk-sf000006")
	e.s.Decide("dk-sf000006", Action{Kind: "choose", Option: 1})
	st.DB.Exec(`UPDATE items SET expires_at=? WHERE id='dk-sf000006'`, time.Now().Add(-time.Minute).Unix())
	e.s.Decide("dk-sf000006", Action{Kind: "stash"})
	st.Expire(time.Now().Unix())
	if it := status(t, st, "dk-sf000006"); it.Status != "expired" {
		t.Fatalf("test setup: expire didn't land: %s", it.Status)
	}
	if err := e.s.Decide("dk-sf000006", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it := status(t, st, "dk-sf000006"); it.Status != "answered" {
		t.Fatalf("an expired stashed answer came back as %s", it.Status)
	}
}

// An agent closes a delivered answer by key, too.
func TestAgentDoneByKeyClosesADeliveredItem(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		e := newEnv(t, "")
		st := e.s.st
		e.file(t, "s", record.Item{ID: "dk-sf000007", Kind: "ask", Title: "which GPU?", Project: "w", Key: "gpu", Options: []string{"A100", "H100"}})
		e.s.Decide("dk-sf000007", Action{Kind: "choose", Option: 0})
		confirm(t, st, "dk-sf000007")
		d := &record.Done{Key: "gpu"}
		if explicit {
			d.Project, d.Explicit = "w", true
		}
		e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: d}, ingest.Options{})
		if it := status(t, st, "dk-sf000007"); it.Status != "done" {
			t.Errorf("done --key (project named: %v) left it %s", explicit, it.Status)
		}
	}
}

// A delivered answer waits a day from its delivery, however late that was;
// one brought back from the Stash waits another day.
func TestWaitingCountsFromDelivery(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-sf000008")
	e.s.Decide("dk-sf000008", Action{Kind: "choose", Option: 0})
	st.DB.Exec(`UPDATE items SET answered_at=?, updated_at=? WHERE id='dk-sf000008'`, time.Now().Add(-30*time.Hour).Unix(), time.Now().Add(-30*time.Hour).Unix())
	confirm(t, st, "dk-sf000008") // delivered only now (a laptop that slept)
	if !onPage(e, "/", "dk-sf000008") || onPage(e, "/done", "dk-sf000008") {
		t.Fatal("a late delivery should wait on the docket first")
	}
	st.DB.Exec(`UPDATE items SET updated_at=? WHERE id='dk-sf000008'`, time.Now().Add(-48*time.Hour).Unix())
	if onPage(e, "/", "dk-sf000008") || !onPage(e, "/done", "dk-sf000008") {
		t.Fatal("two days after delivery it belongs in Done")
	}
	e.s.Decide("dk-sf000008", Action{Kind: "stash"})
	if err := e.s.Decide("dk-sf000008", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if !onPage(e, "/", "dk-sf000008") {
		t.Fatal("Back to docket put an old delivered answer somewhere other than the docket")
	}
}

// An open item never hides on the Stash (an older docket's Reopen leaves its
// stash time behind), and an agent escalating a stashed item brings it back.
func TestOpenItemsAreNeverStashed(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-sf000009", Kind: "issue", Title: "flaky CI", Project: "w"})
	st.DB.Exec(`UPDATE items SET stashed_at=? WHERE id='dk-sf000009'`, time.Now().Unix())
	if !onPage(e, "/", "dk-sf000009") || onPage(e, "/stash", "dk-sf000009") {
		t.Fatal("an open item with a stash time should be on the docket, not the Stash")
	}
	if strings.Contains(get(e, "/"), `class="tc"`) {
		t.Error("the nav counts it on the Stash")
	}

	e.file(t, "s", record.Item{ID: "dk-sf000010", Kind: "ask", Title: "which GPU?", Project: "w", Key: "gpu2", Options: []string{"A100", "H100"}})
	e.s.Decide("dk-sf000010", Action{Kind: "choose", Option: 0})
	e.s.Decide("dk-sf000010", Action{Kind: "stash"})
	e.file(t, "s", record.Item{ID: "dk-sf000011", Kind: "ask", Title: "GPUs are gone: which now?", Project: "w", Key: "gpu2", Urgency: "now"})
	if it := status(t, st, "dk-sf000010"); it.StashedAt != 0 || !onPage(e, "/", it.ID) {
		t.Fatalf("an escalated item stayed hidden on the Stash: %+v", it)
	}
}

// Undo after Stash keeps the time the item was closed.
func TestUndoKeepsTheCloseTime(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-sf000012", Kind: "issue", Title: "x", Project: "w"})
	e.s.Decide("dk-sf000012", Action{Kind: "done"})
	then := time.Now().Add(-3 * time.Hour).Unix()
	st.DB.Exec(`UPDATE items SET resolved_at=? WHERE id='dk-sf000012'`, then)
	e.s.Decide("dk-sf000012", Action{Kind: "stash"})
	e.s.Decide("dk-sf000012", Action{Kind: "undo"})
	if it := status(t, st, "dk-sf000012"); it.ResolvedAt != then {
		t.Fatalf("resolved_at %d, want %d", it.ResolvedAt, then)
	}
}

// The docket refreshes when what it shows changes: a stash, a stuck
// delivery, the agent picking up an answer.
func TestDigestSeesStashDeliveryAndAgent(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-sf000013")
	st.UpsertItem(store.Item{ID: "dk-sf000014", Kind: "review", Title: "results", Project: "w"})
	e.s.Decide("dk-sf000013", Action{Kind: "choose", Option: 0})
	step := func(what string, f func()) {
		t.Helper()
		before := st.QueueDigest()
		f()
		if st.QueueDigest() == before {
			t.Errorf("the digest didn't change on %s", what)
		}
	}
	ds, _ := st.ItemDeliveries("dk-sf000013")
	step("a stuck delivery", func() { st.DeliveryFailed(ds[0].ID, "machine box isn't reachable") })
	st.DB.Exec(`INSERT INTO sessions(id, machine, state) VALUES ('s','box','idle')`)
	step("the agent starting on the answer", func() { st.DB.Exec(`UPDATE sessions SET state='busy' WHERE id='s'`) })
	e.s.Decide("dk-sf000014", Action{Kind: "done"}) // closed: only the stash puts it in view
	step("a stash", func() { e.s.Decide("dk-sf000014", Action{Kind: "stash"}) })
}

// A rule is approved or rejected, never stashed.
func TestRulesCantBeStashed(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-sf000015", Kind: "rule", Title: "Don't file merged-PR reports", Project: "w"})
	if err := e.s.Decide("dk-sf000015", Action{Kind: "stash"}); err == nil {
		t.Fatal("a rule was stashed")
	}
}

// The quiet done note names the id this agent was given when it re-filed.
func TestDoneNoteNamesTheAgentsID(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s1", record.Item{ID: "dk-sf000016", Kind: "review", Title: "results", Project: "w", Key: "res"})
	e.file(t, "s2", record.Item{ID: "dk-sf000017", Kind: "review", Title: "results v2", Project: "w", Key: "res"})
	e.s.Decide("dk-sf000016", Action{Kind: "done"})
	qs := quietNotes(t, st, "dk-sf000016")
	if len(qs) != 1 || !strings.Contains(qs[0].Text, "dk-sf000017") {
		t.Fatalf("notes = %+v", qs)
	}
}

// Done on an ask the agent is paused on wakes it (a regular delivery).
func TestDoneOnABlockingAskWakesTheAgent(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-sf000020", Kind: "ask", Title: "deploy?", Project: "w", SessionID: "s", Machine: "box", Source: "cli", Blocking: true})
	e.s.Decide("dk-sf000020", Action{Kind: "done"})
	ds, _ := st.ItemDeliveries("dk-sf000020")
	if len(ds) != 1 || ds[0].LiveOnly || ds[0].Quiet || !strings.Contains(ds[0].Text, "don't take it as a yes") {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// An answer sent and retried that the agent never confirmed is stuck.
func TestUnconfirmedAnswerIsStuck(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	ask(e, "dk-sf000021")
	e.s.Decide("dk-sf000021", Action{Kind: "choose", Option: 0})
	st.DB.Exec(`UPDATE deliveries SET status='sent', attempts=4, sent_at=? WHERE item_id='dk-sf000021'`, time.Now().Add(-2*time.Hour).Unix())
	if row := rowHTML(t, get(e, "/?p=w"), "dk-sf000021"); !strings.Contains(row, `class="stuck"`) {
		t.Fatalf("an answer never confirmed isn't flagged: %s", row)
	}
}

// Config keys docket doesn't use are named at start, the old robot guard
// with why.
func TestIgnoredConfigKeys(t *testing.T) {
	p := t.TempDir() + "/docket.toml"
	os.WriteFile(p, []byte("data_dir = \""+t.TempDir()+"\"\n[[machine]]\nname = \"lab\"\nmode = \"local\"\nphysical_keywords = [\"arm\"]\nremote_approve = false\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Ignored, ","); !strings.Contains(got, "physical_keywords") || !strings.Contains(got, "remote_approve") {
		t.Fatalf("ignored = %v", c.Ignored)
	}
}
