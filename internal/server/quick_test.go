package server

import (
	"regexp"
	"strings"
	"testing"

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
	// A review's card offers Done too, so d works on it.
	if !strings.Contains(rowHTML(t, page, "dk-q0000002"), `data-act="done" data-key="d"`) {
		t.Error("a review's card doesn't offer Done")
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
