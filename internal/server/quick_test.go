package server

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// quickHTML is a row's one-tap actions, or "" when it has none.
func quickHTML(row string) string {
	return regexp.MustCompile(`(?s)<div class="qa".*?</div>`).FindString(row)
}

// Every open row can be closed from the list without opening it: Done,
// "Not worth it" and Dismiss, the same on every kind. An answered one waits
// on its agent, folded under "Waiting on agents", and can only be dismissed.
func TestRowsOfferOneTapCloses(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	for _, it := range []store.Item{
		{ID: "dk-q0000001", Kind: "issue", Title: "flaky CI", Project: "w"},
		{ID: "dk-q0000002", Kind: "review", Title: "results are in", Project: "w"},
		{ID: "dk-q0000003", Kind: "approval", Title: "push to main?", Project: "w", SessionID: "s", Machine: "box", Source: "cli"},
		{ID: "dk-q0000004", Kind: "ask", Title: "which GPU?", Project: "w", SessionID: "s", Machine: "box", Source: "cli"},
	} {
		st.UpsertItem(it)
	}
	if err := e.s.Decide("dk-q0000004", Action{Kind: "answer", Text: "the A100"}); err != nil {
		t.Fatal(err)
	}
	page := get(e, "/?p=w")
	every := []string{`data-act="done"`, `data-noise`, `data-act="dismiss"`}
	want := map[string][]string{
		"dk-q0000001": every,
		"dk-q0000002": every,
		"dk-q0000003": every,
		"dk-q0000004": {`data-act="dismiss"`},
	}
	for id, has := range want {
		q := quickHTML(rowHTML(t, page, id))
		if q == "" {
			t.Fatalf("%s: no quick actions", id)
		}
		for _, a := range every {
			in, should := strings.Contains(q, a), false
			for _, h := range has {
				should = should || h == a
			}
			if in != should {
				t.Errorf("%s: quick action %s present=%v, want %v", id, a, in, should)
			}
		}
	}
	if !strings.Contains(rowHTML(t, page, "dk-q0000004"), "You replied <b>the A100</b>") {
		t.Error("the waiting row doesn't say what you answered")
	}
	// The dock has the same five actions on every open item, d included.
	for _, id := range []string{"dk-q0000001", "dk-q0000002", "dk-q0000003"} {
		d := dockHTML(t, e, id)
		for _, a := range []string{`data-act="done"`, `data-noise`, `data-act="dismiss"`, `data-act="stash"`, `data-act="snooze"`} {
			if !strings.Contains(d, a) {
				t.Errorf("%s: the dock lacks %s", id, a)
			}
		}
	}
	// An approval with no options gets Approve and Decline in the dock; a
	// review has no "Looks good", only a reply box.
	if d := dockHTML(t, e, "dk-q0000003"); !strings.Contains(d, `data-act="approve"`) || !strings.Contains(d, `data-act="decline"`) {
		t.Errorf("an approval's dock: %s", d)
	}
	if d := dockHTML(t, e, "dk-q0000002"); strings.Contains(d, "approve") || strings.Contains(d, "Looks good") {
		t.Errorf("a review still offers Looks good: %s", d)
	}
}

// A delivered answer waits for its agent: a follow-up box and the closes,
// but no second answer or snooze. A rule offers Approve and Reject, never
// Done or a note. A snoozed row sits in its fold and keeps its note.
func TestQuickActionsEdgeRows(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-q0000011", Kind: "ask", Title: "which GPU?", Project: "w", SessionID: "s", Machine: "box", Source: "cli", Options: []string{"A100", "H100"}})
	if err := e.s.Decide("dk-q0000011", Action{Kind: "choose", Option: 1}); err != nil {
		t.Fatal(err)
	}
	st.DB.Exec(`UPDATE items SET status='delivered' WHERE id='dk-q0000011'`)
	st.UpsertItem(store.Item{ID: "dk-q0000012", Kind: "rule", Title: "Don't file merged-PR reports", Project: "w"})
	st.UpsertItem(store.Item{ID: "dk-q0000013", Kind: "issue", Title: "flaky CI", Project: "w"})
	st.Snooze("dk-q0000013", time.Now().Add(time.Hour).Unix())

	d := dockHTML(t, e, "dk-q0000011")
	if !strings.Contains(d, "data-reply") || strings.Contains(d, `data-act="choose"`) || !strings.Contains(d, `data-act="done"`) {
		t.Errorf("a delivered item's dock: %s", d)
	}
	if !strings.Contains(sheetHTML(t, e, "dk-q0000011"), "You chose “H100”") {
		t.Error("the delivered item's sheet doesn't say what you chose")
	}
	rule := dockHTML(t, e, "dk-q0000012")
	if strings.Contains(rule, `data-act="done"`) || strings.Contains(rule, "data-noise") || !strings.Contains(rule, `data-act="approve"`) {
		t.Errorf("a rule's dock: %s", rule)
	}
	page := get(e, "/?p=w")
	snoozed := rowHTML(t, page, "dk-q0000013")
	if !strings.Contains(quickHTML(snoozed), "data-noise") || strings.Index(page, `data-wt="snoozed"`) > strings.Index(page, `data-id="dk-q0000013"`) {
		t.Errorf("a snoozed row lost its note, or its fold: %s", snoozed)
	}
	if !strings.Contains(rowHTML(t, page, "dk-q0000011"), "Delivered, no reply yet") {
		t.Error("a delivered answer isn't waiting on the docket")
	}
	if onPage(e, "/done", "dk-q0000011") {
		t.Error("an answer still waiting on its agent is in Done too")
	}
	// Once the agent has had it for a day, it leaves the docket.
	st.DB.Exec(`UPDATE items SET updated_at=? WHERE id='dk-q0000011'`, time.Now().Add(-25*time.Hour).Unix())
	if strings.Contains(get(e, "/?p=w"), `data-id="dk-q0000011"`) || !onPage(e, "/done", "dk-q0000011") {
		t.Error("a day-old delivered answer should have moved from the docket to Done")
	}
}

// Undoing (or reopening) a rule decision, one click from the toast or the
// Done page, makes it a proposal again: agents stop getting it, and the
// machines are told (the rules version moves).
func TestUndoingARuleDecisionRetractsIt(t *testing.T) {
	for _, c := range []struct{ decide, back string }{
		{"approve", "undo"}, {"decline", "undo"}, {"approve", "reopen"}, {"decline", "reopen"},
	} {
		t.Run(c.decide+"/"+c.back, func(t *testing.T) {
			e := newEnv(t, "")
			st := e.s.st
			st.UpsertItem(store.Item{ID: "dk-q0000021", Kind: "rule", Title: "Don't file merged-PR reports", Project: "w"})
			if err := e.s.Decide("dk-q0000021", Action{Kind: c.decide}); err != nil {
				t.Fatal(err)
			}
			if rs, _ := st.Rules(""); len(rs) != 1 {
				t.Fatalf("the decision didn't land: %+v", rs)
			}
			v := st.Get("rules:version")
			if err := e.s.Decide("dk-q0000021", Action{Kind: c.back}); err != nil {
				t.Fatal(err)
			}
			if rs, _ := st.Rules(""); len(rs) != 0 {
				t.Fatalf("the taken-back rule is still recorded: %+v", rs)
			}
			if it, _, _ := st.GetItem("dk-q0000021"); it.Status != "open" {
				t.Fatalf("the rule isn't a proposal again: %s", it.Status)
			}
			if md := e.s.RulesMarkdown(); strings.Contains(md, "merged-PR") {
				t.Fatalf("rules.md still carries it:\n%s", md)
			}
			if st.Get("rules:version") == v {
				t.Fatal("the rules version didn't move, so machines keep the old rules.md")
			}
		})
	}
}

// An answer still on its way can't be sent again (a double press, a second
// tab); undo is how it changes.
func TestASecondAnswerIsRefused(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-q0000031", Kind: "approval", Title: "push to main?", Project: "w", SessionID: "s", Machine: "box", Source: "cli", Options: []string{"Yes", "No"}})
	if err := e.s.Decide("dk-q0000031", Action{Kind: "approve"}); err != nil {
		t.Fatal(err)
	}
	// A snooze would wake it as open, asking again for an answer it has.
	for _, a := range []Action{{Kind: "approve"}, {Kind: "decline"}, {Kind: "choose", Option: 1}, {Kind: "answer", Text: "yes"}, {Kind: "snooze", Until: time.Now().Add(time.Hour)}} {
		if err := e.s.Decide("dk-q0000031", a); err == nil {
			t.Errorf("a %s on an answered item was accepted", a.Kind)
		}
	}
	if ds, _ := st.ItemDeliveries("dk-q0000031"); len(ds) != 1 {
		t.Fatalf("%d answers queued for the agent, want 1", len(ds))
	}
	// Undo, then a changed answer, still works.
	if err := e.s.Decide("dk-q0000031", Action{Kind: "undo"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Decide("dk-q0000031", Action{Kind: "decline"}); err != nil {
		t.Fatalf("a changed answer after undo: %v", err)
	}
}

// A closed item's sheet offers Back to docket and Stash, never a close.
func TestClosedRowHasNoQuickActions(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-q0000009", Kind: "issue", Title: "x", Project: "w"})
	if err := e.s.Decide("dk-q0000009", Action{Kind: "done"}); err != nil {
		t.Fatal(err)
	}
	d := dockHTML(t, e, "dk-q0000009")
	if strings.Contains(d, `data-act="done"`) || strings.Contains(d, "data-noise") || !strings.Contains(d, `data-act="reopen"`) || !strings.Contains(d, `data-act="stash"`) {
		t.Fatalf("a closed item's dock: %s", d)
	}
	row := rowHTML(t, get(e, "/done"), "dk-q0000009")
	if q := quickHTML(row); strings.Contains(q, `data-act="done"`) || !strings.Contains(q, `data-act="reopen"`) {
		t.Fatalf("a Done row's actions: %s", q)
	}
}
