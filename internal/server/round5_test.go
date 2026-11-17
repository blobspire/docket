package server

import (
	"runtime"
	"strings"
	"testing"

	"github.com/blobspire/docket/internal/store"
)

func allocs(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// Table shapes markdown builds that a naive scan would miss (a line of
// non-breaking spaces isn't blank; a table inside a blockquote with lazy
// rows) stay within the budget.
func TestTableBudgetBypassesAreCaught(t *testing.T) {
	head := "|" + strings.Repeat("h|", 64) + "\n|" + strings.Repeat("-|", 64) + "\n"
	rows := strings.Repeat("x\n", 9000)
	for name, s := range map[string]string{
		"nbsp":       head + " \n" + rows,
		"em space":   head + " \n" + rows,
		"blockquote": "> " + strings.ReplaceAll(strings.TrimSuffix(head, "\n"), "\n", "\n> ") + "\n" + rows,
	} {
		if a := allocs(func() { renderMarkdown(s) }); a > 40<<20 {
			t.Errorf("%s: %d MB allocated", name, a>>20)
		}
	}
}

// Ordinary reports keep their tables: up to 64 columns, lists and dashes
// around them, long documents of short lines.
func TestOrdinaryTablesStay(t *testing.T) {
	table := func(cols int) string {
		return "|" + strings.Repeat("c|", cols) + "\n|" + strings.Repeat("-|", cols) + "\n|" + strings.Repeat("1|", cols) + "\n"
	}
	if !strings.Contains(string(renderMarkdown(table(64))), "<table>") {
		t.Error("a 64-column table didn't render")
	}
	if strings.Contains(string(renderMarkdown(table(66))), "<table>") {
		t.Error("a 66-column table rendered")
	}
	var b strings.Builder
	b.WriteString(table(12) + "\n")
	for b.Len() < 20<<10 {
		b.WriteString("- item | with a pipe - and dashes\n")
	}
	if !strings.Contains(string(renderMarkdown(b.String())), "<table>") {
		t.Error("a long report of list lines lost its table")
	}
}

// The waiting row's delivery is the answer, never a done or reopened note.
func TestLatestDeliveryIsTheAnswer(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r6l00001", Kind: "ask", Title: "deploy?", Project: "w", SessionID: "s", Machine: "box", Source: "cli", Blocking: true, Options: []string{"a", "b"}})
	e.s.Decide("dk-r6l00001", Action{Kind: "choose", Option: 0})
	st.DB.Exec(`INSERT INTO deliveries(id, item_id, machine, session_id, text, nonce, status, created_at, live_only, quiet, kind) VALUES ('dl-note','dk-r6l00001','box','s','note','nn','pending',strftime('%s','now')+5,0,0,'note')`)
	if d := st.LatestDeliveries([]string{"dk-r6l00001"})["dk-r6l00001"]; d.Kind != "answer" {
		t.Fatalf("latest = %+v, want the answer", d)
	}
}

// A noise note queued by an older docket (no kind) is withdrawn by the undo.
func TestLegacyNoiseNoteWithdrawn(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r6n00001", Kind: "review", Title: "flaky", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-r6n00001", Action{Kind: "noise"})
	st.DB.Exec(`UPDATE deliveries SET kind='' WHERE item_id='dk-r6n00001'`)
	e.s.Decide("dk-r6n00001", Action{Kind: "undo"})
	if ds, _ := st.ItemDeliveries("dk-r6n00001"); len(ds) != 0 {
		t.Fatalf("the old noise note is still queued: %+v", ds)
	}
}

// Brain's rule rows offer no Stash; an item's own page goes back to "All";
// an older docket's done note is labelled as one.
func TestTemplateLabels(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r6t00001", Kind: "rule", Title: "Don't file merged-PR reports", Project: "w"})
	if row := rowHTML(t, get(e, "/brain"), "dk-r6t00001"); !strings.Contains(row, "data-nostash") {
		t.Error("Brain's rule row would offer Stash")
	}
	st.UpsertItem(store.Item{ID: "dk-r6t00002", Kind: "human_task", Title: "plug in", Project: "w", SessionID: "s", Machine: "box", Source: "cli"})
	if s := sheetHTML(t, e, "dk-r6t00002"); !strings.Contains(s, "<span>All</span>") {
		t.Error("the standalone Back isn't labelled All")
	}
	e.s.Decide("dk-r6t00002", Action{Kind: "done"})
	st.DB.Exec(`UPDATE deliveries SET kind='', text='[docket dk-r6t00002 #x] Cole marked this done: "plug in".' WHERE item_id='dk-r6t00002'`)
	if s := sheetHTML(t, e, "dk-r6t00002"); !strings.Contains(s, "done note pending") {
		t.Error("an older done note isn't labelled as one")
	}
}

// Each guard on its own: the table scan sees the bypass shapes, and the
// floor bounds a wide line times a long text.
func TestTableGuardsSeparately(t *testing.T) {
	head := "|" + strings.Repeat("h|", 64) + "\n|" + strings.Repeat("-|", 64) + "\n"
	rows := strings.Repeat("x\n", 9000)
	for name, s := range map[string]string{
		"nbsp":       head + " \n" + rows,
		"blockquote": "> " + strings.ReplaceAll(strings.TrimSuffix(head, "\n"), "\n", "\n> ") + "\n" + rows,
	} {
		if c := tableCells(s); c <= maxTableCells {
			t.Errorf("%s: tableCells = %d, want over %d", name, c, maxTableCells)
		}
	}
	if f := tableFloor("|" + strings.Repeat("x|", 60) + "\n" + rows); f <= maxTableFloor {
		t.Errorf("floor = %d, want over %d", f, maxTableFloor)
	}
	if strings.Contains(string(renderMarkdown("|"+strings.Repeat("c|", 65)+"\n|"+strings.Repeat("-|", 65)+"\n|"+strings.Repeat("1|", 65)+"\n")), "<table>") {
		t.Error("a 65-column table rendered")
	}
}
