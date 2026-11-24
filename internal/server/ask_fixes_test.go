package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

const pushCfg = `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[machine]]
name = "laptop"
mode = "push"
`

func syncLaptop(t *testing.T, e *env) SyncReply {
	t.Helper()
	reply, err := e.s.Receive("laptop", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: "-", Context: "-"})
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

// A question about an item a laptop session filed, after that session
// ended, resumes the session there with the question (the laptop can't be
// reached, so its next sync hands it a start); if resuming keeps failing,
// the question waits in the mailbox.
func TestAskResumesAnEndedLaptopSession(t *testing.T) {
	e := newEnv(t, pushCfg)
	st := e.s.st
	st.PatchSession("s-lap0001", store.SessionPatch{Machine: store.Str("laptop"), State: store.Str("ended"), HasPlugin: store.Bool(true), Cwd: store.Str("/Users/x/vlm"), Name: store.Str("vlm-agent")})
	st.UpsertItem(store.Item{ID: "dk-lap00001", Kind: "review", Title: "Laptop result", Status: "done", Source: "cli", SessionID: "s-lap0001", Machine: "laptop", Project: "vlm"})
	if err := e.s.Decide("dk-lap00001", Action{Kind: "ask", Text: "Which run?"}); err != nil {
		t.Fatal(err)
	}
	reply := syncLaptop(t, e)
	if len(reply.Deliveries) != 0 || len(reply.Starts) != 1 {
		t.Fatalf("reply: %d deliveries, starts %+v", len(reply.Deliveries), reply.Starts)
	}
	sc := reply.Starts[0]
	if sc.Resume != "s-lap0001" || sc.Cwd != "/Users/x/vlm" || sc.Name != "vlm-agent" || sc.Idea != "" || !strings.Contains(sc.Prompt, "Which run?") || !strings.Contains(sc.Prompt, "docket add dk-lap00001") {
		t.Fatalf("start = %+v", sc)
	}
	ds, _ := st.ItemDeliveries("dk-lap00001")
	if len(ds) != 1 || ds[0].Status != "sent" || ds[0].Method != "resume" {
		t.Fatalf("delivery = %+v", ds)
	}
	if again := syncLaptop(t, e); len(again.Starts)+len(again.Deliveries) != 0 {
		t.Fatalf("handed out twice: %+v", again)
	}

	// The laptop couldn't start it: the question waits again, saying why.
	fail := func(id string) {
		t.Helper()
		p, _ := json.Marshal(map[string]any{"start_id": id, "ok": false, "error": "a tmux session named vlm-agent is already running"})
		e.sendFrom(t, "laptop", record.Record{Event: "session.started", Payload: p})
	}
	fail(sc.ID)
	ds, _ = st.ItemDeliveries("dk-lap00001")
	if ds[0].Status != "pending" || ds[0].Attempts != 1 || !strings.Contains(ds[0].Error, "couldn't resume the session on laptop") {
		t.Fatalf("after a failed start: %+v", ds[0])
	}
	if b := getOK(t, e, "/item/dk-lap00001"); !strings.Contains(b, "data-asking") {
		t.Fatal("the sheet stopped waiting after one failed start")
	}
	for i := 0; i < askResumeTries-1; i++ {
		r := syncLaptop(t, e)
		if len(r.Starts) != 1 {
			t.Fatalf("try %d: starts %+v", i+2, r.Starts)
		}
		fail(r.Starts[0].ID)
	}
	// Out of tries: the mailbox, where its next resume finds it.
	r := syncLaptop(t, e)
	if len(r.Starts) != 0 || len(r.Deliveries) != 1 || !strings.Contains(r.Deliveries[0].Text, "Which run?") {
		t.Fatalf("after %d failed starts: %+v", askResumeTries, r)
	}
}

// Text an agent adds answers only the questions it received; a done with a
// note answers too.
func TestAnswerLabelsOnlyReceivedQuestions(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("s-x0000001", store.SessionPatch{Machine: store.Str("box"), State: store.Str("idle"), HasPlugin: store.Bool(true), Cwd: store.Str("/w/vlm")})
	st.UpsertItem(store.Item{ID: "dk-lab00001", Kind: "review", Title: "Result", Status: "done", Source: "cli", SessionID: "s-x0000001", Machine: "box", Project: "vlm"})
	e.s.Decide("dk-lab00001", Action{Kind: "ask", Text: "Which seed?"})
	add := func(text string) {
		t.Helper()
		p, _ := json.Marshal(map[string]any{"item_id": "dk-lab00001", "text": text})
		e.send(t, record.Record{Event: "cli.add", SessionID: "s-x0000001", Payload: p}, ingest.Options{})
	}
	add("Unrelated: pushed the logs.") // the question hasn't reached it yet
	it, _, _ := st.GetItem("dk-lab00001")
	if strings.Contains(it.Body, "You asked") {
		t.Fatalf("an undelivered question was answered: %q", it.Body)
	}
	ds, _ := st.ItemDeliveries("dk-lab00001")
	st.DB.Exec(`UPDATE deliveries SET status='confirmed' WHERE id=?`, ds[0].ID)
	// It answers with a done and a note.
	e.send(t, record.Record{Event: "cli.done", SessionID: "s-x0000001", Done: &record.Done{ItemID: "dk-lab00001", Note: "Seed 1004."}}, ingest.Options{})
	it, _, _ = st.GetItem("dk-lab00001")
	if !strings.HasSuffix(it.Body, "**You asked:** Which seed?\n\nSeed 1004.") || it.Status != "done" {
		t.Fatalf("after done --why: %s %q", it.Status, it.Body)
	}
	if left, _ := st.UnansweredAsks("dk-lab00001"); len(left) != 0 {
		t.Fatalf("still unanswered: %+v", left)
	}
}

func TestAskWordingAndRefusals(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("s-y0000001", store.SessionPatch{Machine: store.Str("box"), State: store.Str("idle"), HasPlugin: store.Bool(true), Cwd: store.Str("/w/vlm")})
	// A delivered item: their answer reached the agent; it closes it.
	st.UpsertItem(store.Item{ID: "dk-del00001", Kind: "ask", Title: "Retrain?", Status: "delivered", Source: "cli", SessionID: "s-y0000001", Machine: "box", Project: "vlm"})
	v := st.Version()
	if err := e.s.Decide("dk-del00001", Action{Kind: "ask", Text: "Why that option?"}); err != nil {
		t.Fatal(err)
	}
	if st.Version() == v {
		t.Fatal("asking didn't change the page version")
	}
	ds, _ := st.ItemDeliveries("dk-del00001")
	if len(ds) != 1 || !strings.Contains(ds[0].Text, "it's yours to close when it's done") || strings.Contains(ds[0].Text, "off their docket") {
		t.Fatalf("delivered wording: %+v", ds)
	}
	st.UpsertItem(store.Item{ID: "dk-ans00001", Kind: "ask", Title: "Ship?", Status: "answered", Source: "cli", SessionID: "s-y0000001", Machine: "box", Project: "vlm"})
	if err := e.s.Decide("dk-ans00001", Action{Kind: "ask", Text: "x"}); err == nil || !strings.Contains(err.Error(), "on its way") {
		t.Fatalf("answered: %v", err)
	}
	if err := e.s.Decide("dk-del00001", Action{Kind: "ask", Text: strings.Repeat("é", maxQuestion)}); err != nil {
		t.Fatalf("%d characters refused: %v", maxQuestion, err)
	}

	// A re-filed item: the message names the id this session was told.
	e.file(t, "s-y0000001", record.Item{ID: "dk-ref00001", Kind: "review", Title: "Nightly", Key: "nightly", Project: "vlm"})
	e.file(t, "s-y0000001", record.Item{ID: "dk-ref00002", Kind: "review", Title: "Nightly v2", Key: "nightly", Project: "vlm"})
	e.s.Decide("dk-ref00001", Action{Kind: "done"})
	if err := e.s.Decide("dk-ref00001", Action{Kind: "ask", Text: "Which night?"}); err != nil {
		t.Fatal(err)
	}
	ds, _ = st.ItemDeliveries("dk-ref00001")
	last := ds[len(ds)-1]
	if last.Kind != "ask" || !strings.Contains(last.Text, "(this is the item you filed as dk-ref00002)") {
		t.Fatalf("re-filed ask: %+v", last)
	}
}

// rules.md says which "not worth it" items were progress entries.
func TestRulesLabelNoisedProgress(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s-z0000001", record.Item{ID: "dk-pnoise01", Kind: "progress", Title: "Ran the linter", Project: "vlm"})
	if err := e.s.Decide("dk-pnoise01", Action{Kind: "noise"}); err != nil {
		t.Fatal(err)
	}
	if md := e.s.RulesMarkdown(); !strings.Contains(md, "- Ran the linter (vlm) [a progress entry: post progress only for major stages]") {
		t.Fatalf("rules.md:\n%s", md)
	}
}

func TestFirstLineKeepsBoldWords(t *testing.T) {
	for in, want := range map[string]string{
		"Summary: **3 of 5** seeds diverge.": "Summary: 3 of 5 seeds diverge.",
		"*Model A* loses _13_ points":        "Model A loses 13 points",
		"`results/x` is in":                  "results/x is in",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A resume that started is never resumed again, even with no receipt (the
// question may have reached the agent, or arrive in the same sync): its
// sheet says it went to a resumed session.
func TestAskResumesOnceWithoutReceipt(t *testing.T) {
	e := newEnv(t, pushCfg)
	st := e.s.st
	st.PatchSession("s-lap0002", store.SessionPatch{Machine: store.Str("laptop"), State: store.Str("ended"), HasPlugin: store.Bool(true), Cwd: store.Str("/Users/x/vlm")})
	st.UpsertItem(store.Item{ID: "dk-lap00002", Kind: "review", Title: "Laptop result", Status: "done", Source: "cli", SessionID: "s-lap0002", Machine: "laptop", Project: "vlm"})
	e.s.Decide("dk-lap00002", Action{Kind: "ask", Text: "Which run?"})
	if r := syncLaptop(t, e); len(r.Starts) != 1 {
		t.Fatalf("first sync: %+v", r)
	}
	st.DB.Exec(`UPDATE deliveries SET sent_at=sent_at-86400 WHERE item_id='dk-lap00002'`)
	st.PatchSession("s-lap0002", store.SessionPatch{State: store.Str("ended")})
	for i := 0; i < 3; i++ {
		if r := syncLaptop(t, e); len(r.Starts)+len(r.Deliveries) != 0 {
			t.Fatalf("handed out again: %+v", r)
		}
	}
	e.s.deliverAll(e.ctx)
	ds, _ := st.ItemDeliveries("dk-lap00002")
	if len(ds) != 1 || ds[0].Status != "sent" || ds[0].Attempts != 1 {
		t.Fatalf("delivery = %+v", ds)
	}
	if b := getOK(t, e, "/item/dk-lap00002"); !strings.Contains(b, "resumed on laptop") {
		t.Fatal("the sheet doesn't say the question went to a resumed session")
	}
}

// Two overlapping syncs hand a question out once.
func TestOverlappingSyncsHandOutOnce(t *testing.T) {
	e := newEnv(t, pushCfg)
	st := e.s.st
	st.PatchSession("s-lap0004", store.SessionPatch{Machine: store.Str("laptop"), State: store.Str("ended"), HasPlugin: store.Bool(true), Cwd: store.Str("/Users/x/vlm")})
	st.PatchSession("s-lap0005", store.SessionPatch{Machine: store.Str("laptop"), State: store.Str("idle"), HasPlugin: store.Bool(true), Cwd: store.Str("/Users/x/vlm")})
	st.UpsertItem(store.Item{ID: "dk-lap00004", Kind: "review", Title: "Ended", Status: "done", Source: "cli", SessionID: "s-lap0004", Machine: "laptop", Project: "vlm"})
	st.UpsertItem(store.Item{ID: "dk-lap00005", Kind: "review", Title: "Running", Status: "done", Source: "cli", SessionID: "s-lap0005", Machine: "laptop", Project: "vlm"})
	e.s.Decide("dk-lap00004", Action{Kind: "ask", Text: "Q1"})
	e.s.Decide("dk-lap00005", Action{Kind: "ask", Text: "Q2"})
	replies := make(chan SyncReply, 8)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			r, _ := e.s.Receive("laptop", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: "-", Context: "-"})
			replies <- r
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	close(replies)
	starts, boxed := 0, 0
	for r := range replies {
		starts += len(r.Starts)
		boxed += len(r.Deliveries)
	}
	if starts != 1 || boxed != 1 {
		t.Fatalf("8 overlapping syncs handed out %d starts and %d mailbox deliveries, want 1 and 1", starts, boxed)
	}
}

// An idea's session resumes under its idea and run name, as on a machine
// the server reaches.
func TestAskResumeKeepsAnIdeaSession(t *testing.T) {
	e := newEnv(t, pushCfg)
	st := e.s.st
	st.PatchSession("s-idea0001", store.SessionPatch{Machine: store.Str("laptop"), State: store.Str("ended"), HasPlugin: store.Bool(true), Cwd: store.Str("/Users/x/docket"), Idea: store.Str("idea-abcdefgh")})
	st.AddIdea(store.Idea{ID: "idea-abcdefgh", Project: "docket", Title: "x", Status: store.IdeaBuilt, SessionID: "s-idea0001", SessionName: "idea-abcdefgh-2"})
	st.UpsertItem(store.Item{ID: "dk-idea0001", Kind: "review", Title: "Shipped", Status: "done", Source: "cli", SessionID: "s-idea0001", Machine: "laptop", Project: "docket"})
	e.s.Decide("dk-idea0001", Action{Kind: "ask", Text: "Which PR?"})
	r := syncLaptop(t, e)
	if len(r.Starts) != 1 || r.Starts[0].Idea != "idea-abcdefgh" || r.Starts[0].Name != "idea-abcdefgh-2" {
		t.Fatalf("start = %+v", r.Starts)
	}
}

// A crowded second with nothing older: no Older link to an empty page.
func TestTimelineNoEmptyOlderPage(t *testing.T) {
	e := newEnv(t, "")
	at := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < timelineLimit+5; i++ {
		e.s.st.UpsertItem(store.Item{ID: fmt.Sprintf("dk-e%07d", i), Kind: "progress", Status: store.StatusLogged, Title: "bulk", Project: "vlm", CreatedAt: at})
	}
	if entries, more, _ := e.s.st.Timeline("vlm", "", 0, timelineLimit); len(entries) != timelineLimit+5 || more {
		t.Fatalf("%d entries, more=%v", len(entries), more)
	}
	if b := getOK(t, e, "/timeline?p=vlm"); strings.Contains(b, "before=") {
		t.Fatal("an Older link with nothing older")
	}
}
