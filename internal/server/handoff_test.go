package server

import (
	"bytes"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blobspire/docket/internal/cli"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/store"
)

// "Have the agent do it" sends a task back to the agent that filed it, with
// the go-ahead, by the same path as any answer; the agent does it and closes
// the item, and the docket says what happened.
func TestHandOffReachesTheAgentAndItClosesTheItem(t *testing.T) {
	e := newEnv(t, "")
	agent := getenv(map[string]string{"DOCKET_SESSION": "sess-1"})
	hook.Run("SessionStart", strings.NewReader(`{"session_id":"sess-1","cwd":"/w/cwant","source":"startup"}`), io.Discard, io.Discard, agent)
	var out bytes.Buffer
	if err := cli.File("task", []string{"Remove the hourly wake-up schedule in docket", "--project", "cwant", "--new-project", "trading"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	hook.Run("Stop", strings.NewReader(`{"session_id":"sess-1","last_assistant_message":"Filed it."}`), io.Discard, io.Discard, agent)
	e.cycle()
	now, _, _, _ := e.s.Queue()
	if len(now) != 1 {
		t.Fatalf("queue = %+v", now)
	}
	id := now[0].ID

	if err := e.s.Decide(id, Action{Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := e.s.st.GetItem(id)
	if it.Status != "answered" || !strings.HasPrefix(it.Answer, handOffMark) {
		t.Fatalf("after hand-off: status %s, answer %q", it.Status, it.Answer)
	}
	if _, action, _, _ := e.s.st.TopDecision(id); action != "approve" {
		t.Fatalf("logged as %q, want approve", action)
	}
	// A second press (another tab) doesn't send it twice.
	if err := e.s.Decide(id, Action{Kind: "handoff"}); err == nil {
		t.Fatal("a second hand-off was accepted")
	}

	e.s.deliverAll(e.ctx)
	var hookOut bytes.Buffer
	hook.Run("PostToolUse", strings.NewReader(`{"session_id":"sess-1"}`), &hookOut, io.Discard, agent)
	got := hookOut.String()
	for _, want := range []string{"Cole's answer to", "Handed back to you: do it yourself", "Cole's go-ahead",
		"including steps outside this repository",                           // the go-ahead reaches past the repo
		"If one step truly needs Cole", "file just that step as a new item", // the limit of it
		"Either way, close this one when you're through: docket done " + id + " --why", // and the item doesn't rot
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("delivered text lacks %q:\n%s", want, got)
		}
	}
	e.cycle() // the receipt
	if it, _, _ = e.s.st.GetItem(id); it.Status != "delivered" {
		t.Fatalf("status = %s", it.Status)
	}
	v := itemView{Ranked: Ranked{Item: it}}
	if verb, what := v.Answered(); verb != "You handed it back" || what != "" {
		t.Fatalf("Answered() = %q, %q", verb, what)
	}
	if v.CanHandOff() {
		t.Fatal("a handed-off item still offers the hand-off")
	}

	// The agent does it and closes the item.
	if err := cli.Done([]string{id, "--why", "commented out the schedule and restarted docket"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	it, _, _ = e.s.st.GetItem(id)
	_, last, _, _ := e.s.st.TopDecision(id)
	v = itemView{Ranked: Ranked{Item: it}, LastAction: last}
	if it.Status != "done" || v.ClosedHow() != "You handed it back; the agent closed it: commented out the schedule and restarted docket" {
		t.Fatalf("status %s, ClosedHow %q", it.Status, v.ClosedHow())
	}
}

// Only a task or an issue an agent filed can be handed back.
func TestHandOffRefusals(t *testing.T) {
	e := newEnv(t, "")
	cases := []store.Item{
		{ID: "dk-aaaaaaa1", Kind: "ask", Title: "Ship it?", Machine: "box", SessionID: "s", Source: "cli", Options: []string{"yes", "no"}},
		{ID: "dk-aaaaaaa2", Kind: "review", Title: "Results are in", Machine: "box", SessionID: "s", Source: "cli"},
		{ID: "dk-aaaaaaa3", Kind: "human_task", Title: "Imported task", Machine: "box", Source: "cli"},                           // no session
		{ID: "dk-aaaaaaa4", Kind: "blocker", Title: "Stuck on a dialog", Machine: "box", SessionID: "s", Source: "notification"}, // docket filed it
		{ID: "dk-aaaaaaa5", Kind: "rule", Title: "Don't file merged PRs", Source: "watchdog"},
		{ID: "dk-aaaaaaa6", Kind: "human_task", Title: "Plug in the drive", Machine: "box", SessionID: "s", Source: "cli", Place: "home"}, // in person
		{ID: "dk-aaaaaaa7", Kind: "human_task", Title: "Reseat the GPU", Machine: "box", SessionID: "s", Source: "cli", Physical: true},   // filed --physical
	}
	for _, it := range cases {
		e.s.st.UpsertItem(it)
		if err := e.s.Decide(it.ID, Action{Kind: "handoff"}); err == nil {
			t.Errorf("%s (%s) was handed off", it.ID, it.Kind)
		}
		if got, _, _ := e.s.st.GetItem(it.ID); got.Status != "open" {
			t.Errorf("%s: status %s after a refused hand-off", it.ID, got.Status)
		}
		if ds, _ := e.s.st.ItemDeliveries(it.ID); len(ds) != 0 {
			t.Errorf("%s: queued %+v", it.ID, ds)
		}
	}
}

// Like any answer, a hand-off can be undone until it reaches the agent.
func TestHandOffUndo(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-bbbbbbb1", Kind: "issue", Title: "Disk is 95% full", Machine: "box", SessionID: "s", Source: "cli"})
	if err := e.s.Decide("dk-bbbbbbb1", Action{Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Decide("dk-bbbbbbb1", Action{Kind: "undo"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := e.s.st.GetItem("dk-bbbbbbb1")
	if it.Status != "open" || it.Answer != "" {
		t.Fatalf("after undo: status %s, answer %q", it.Status, it.Answer)
	}
	e.s.deliverAll(e.ctx)
	for _, d := range mustDeliveries(t, e, "dk-bbbbbbb1") {
		if d.Status == "sent" || d.Status == "confirmed" {
			t.Fatalf("an undone hand-off went out: %+v", d)
		}
	}
}

func mustDeliveries(t *testing.T, e *env, id string) []store.Delivery {
	t.Helper()
	ds, err := e.s.st.ItemDeliveries(id)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// The dock and the row offer it where it applies, and only there; the web
// action works.
func TestHandOffOnThePage(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-ccccccc1", Kind: "human_task", Title: "Edit the server config", Machine: "box", SessionID: "s", Source: "cli"})
	e.s.st.UpsertItem(store.Item{ID: "dk-ccccccc2", Kind: "ask", Title: "Ship it?", Machine: "box", SessionID: "s", Source: "cli", Options: []string{"yes", "no"}})
	e.s.st.UpsertItem(store.Item{ID: "dk-ccccccc3", Kind: "human_task", Title: "Plug in the drive", Machine: "box", Source: "cli"})
	e.s.st.UpsertItem(store.Item{ID: "dk-ccccccc4", Kind: "human_task", Title: "Pick a cert and install it", Machine: "box", SessionID: "s", Source: "cli", Options: []string{"staging", "prod"}})
	h := e.s.Handler()
	get := func(p string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
		return rec.Body.String()
	}
	const btn = `data-act="handoff"`
	if page := get("/item/dk-ccccccc1"); !strings.Contains(page, btn) || !strings.Contains(page, "Have the agent do it") {
		t.Fatal("the task's dock doesn't offer the hand-off")
	}
	for _, id := range []string{"dk-ccccccc2", "dk-ccccccc3"} {
		if strings.Contains(get("/item/"+id), btn) {
			t.Fatalf("%s offers the hand-off", id)
		}
	}
	// A task with options keeps them, and offers the hand-off beside them,
	// as its row does.
	if page := get("/item/dk-ccccccc4"); !strings.Contains(page, btn) || !strings.Contains(page, `data-act="choose"`) {
		t.Fatal("a task with options should offer its options and the hand-off")
	}
	if list := get("/"); strings.Count(list, btn) != 2 {
		t.Fatalf("the list has %d hand-off buttons, want 2 (the two tasks' rows)", strings.Count(list, btn))
	}

	req := httptest.NewRequest("POST", "/item/dk-ccccccc1/act", strings.NewReader(url.Values{"action": {"handoff"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"decision"`) {
		t.Fatalf("POST handoff = %d %s", rec.Code, rec.Body.String())
	}
	page := get("/item/dk-ccccccc1")
	if strings.Contains(page, btn) || !strings.Contains(page, "You handed it back") {
		t.Fatal("after the hand-off the sheet should show it waiting on the agent")
	}
}

// Words typed with the hand-off reach the agent ahead of the go-ahead they
// qualify, and the docket shows them.
func TestHandOffCarriesTheNote(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-eeeeeee1", Kind: "human_task", Title: "Renew the TLS cert", Machine: "box", SessionID: "s", Source: "cli"})
	if err := e.s.Decide("dk-eeeeeee1", Action{Kind: "handoff", Text: "  use the staging cert,\n not prod "}); err != nil {
		t.Fatal(err)
	}
	ds := mustDeliveries(t, e, "dk-eeeeeee1")
	if len(ds) != 1 {
		t.Fatalf("deliveries = %+v", ds)
	}
	want := `Handed back to you: do it yourself, as Cole says here: “use the staging cert, not prod”. This is Cole's go-ahead for every step it takes within that, including`
	if !strings.Contains(ds[0].Text, want) {
		t.Fatalf("delivered %q\nwant it to contain %q", ds[0].Text, want)
	}
	it, _, _ := e.s.st.GetItem("dk-eeeeeee1")
	if verb, what := (itemView{Ranked: Ranked{Item: it}}).Answered(); verb != "You handed it back" || what != "use the staging cert, not prod" {
		t.Fatalf("Answered() = %q, %q", verb, what)
	}
}
