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
	return regexp.MustCompile(`(?s)<div class="quick".*?</div>`).FindString(row)
}

// Every open row can be closed from the list without expanding it: Done
// where done makes sense, "Not worth my time", and Dismiss.
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
	want := map[string][]string{
		"dk-q0000001": {`data-act="done"`, `data-noise`, `data-act="dismiss"`},
		"dk-q0000002": {`data-act="done"`, `data-noise`, `data-act="dismiss"`},
		// An approval needs a yes or no, not a silent "done".
		"dk-q0000003": {`data-noise`, `data-act="dismiss"`},
		// Answered: the answer is on its way; it can only be taken off the list.
		"dk-q0000004": {`data-act="dismiss"`},
	}
	all := []string{`data-act="done"`, `data-noise`, `data-act="dismiss"`}
	for id, has := range want {
		row := rowHTML(t, page, id)
		q := quickHTML(row)
		if q == "" {
			t.Fatalf("%s: no quick actions in %s", id, row)
		}
		for _, a := range all {
			in := strings.Contains(q, a)
			should := false
			for _, h := range has {
				should = should || h == a
			}
			if in != should {
				t.Errorf("%s: quick action %s present=%v, want %v", id, a, in, should)
			}
		}
		// The note box must open under a collapsed row, so it can't live in
		// the hidden detail.
		if strings.Contains(q, "data-noise") {
			nf, det := strings.Index(row, `class="noise-form"`), strings.Index(row, `class="detail"`)
			if nf < 0 || nf > det {
				t.Errorf("%s: the note box is inside the collapsed detail (or missing)", id)
			}
		}
	}
	// A review's card offers Done too, so d works on it; an approval's never
	// does (d would close it without the yes or no its agent waits on).
	if !strings.Contains(rowHTML(t, page, "dk-q0000002"), `data-act="done" data-key="d"`) {
		t.Error("a review's card doesn't offer Done")
	}
	if strings.Contains(rowHTML(t, page, "dk-q0000003"), `data-act="done"`) {
		t.Error("an approval offers Done")
	}
}

// Delivered answers and rules get no one-tap closes; a snoozed row keeps
// its note box.
func TestQuickActionsEdgeRows(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-q0000011", Kind: "ask", Title: "which GPU?", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	st.SetStatus("dk-q0000011", "delivered", "", "")
	st.UpsertItem(store.Item{ID: "dk-q0000012", Kind: "rule", Title: "Don't file merged-PR reports", Project: "w"})
	st.UpsertItem(store.Item{ID: "dk-q0000013", Kind: "issue", Title: "flaky CI", Project: "w"})
	st.Snooze("dk-q0000013", time.Now().Add(time.Hour).Unix())

	if q := quickHTML(rowHTML(t, get(e, "/item/dk-q0000011"), "dk-q0000011")); q != "" {
		t.Errorf("a delivered row offers quick actions: %s", q)
	}
	rule := rowHTML(t, get(e, "/item/dk-q0000012"), "dk-q0000012")
	if q := quickHTML(rule); strings.Contains(q, `data-act="done"`) || strings.Contains(q, "data-noise") || strings.Contains(rule, "noise-form") {
		t.Errorf("a rule offers Done or a note: %s", rule)
	}
	snoozed := rowHTML(t, get(e, "/?p=w"), "dk-q0000013")
	if !strings.Contains(quickHTML(snoozed), "data-noise") || !strings.Contains(snoozed, `class="noise-form"`) {
		t.Errorf("a snoozed row lost its note box: %s", snoozed)
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
	for _, a := range []Action{{Kind: "approve"}, {Kind: "decline"}, {Kind: "choose", Option: 1}, {Kind: "answer", Text: "yes"}} {
		if err := e.s.Decide("dk-q0000031", a); err == nil {
			t.Errorf("a second %s was accepted", a.Kind)
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

// A closed item's row offers Reopen, never a one-tap close.
func TestClosedRowHasNoQuickActions(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-q0000009", Kind: "issue", Title: "x", Project: "w"})
	if err := e.s.Decide("dk-q0000009", Action{Kind: "done"}); err != nil {
		t.Fatal(err)
	}
	row := rowHTML(t, get(e, "/item/dk-q0000009"), "dk-q0000009")
	if quickHTML(row) != "" || strings.Contains(row, "noise-form") {
		t.Fatalf("a closed row offers quick actions: %s", row)
	}
}
