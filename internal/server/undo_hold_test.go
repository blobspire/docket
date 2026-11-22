package server

import (
	"bytes"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/cli"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/store"
)

// fileTask has an agent in session sess-1 file a task, and returns its id and
// the agent's environment.
func fileTask(t *testing.T, e *env) (string, func(string) string) {
	t.Helper()
	agent := getenv(map[string]string{"DOCKET_SESSION": "sess-1"})
	hook.Run("SessionStart", strings.NewReader(`{"session_id":"sess-1","cwd":"/w/robot","source":"startup"}`), io.Discard, io.Discard, agent)
	var out bytes.Buffer
	if err := cli.File("task", []string{"Fix the collision guard at folded-elbow poses", "--project", "robot"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	hook.Run("Stop", strings.NewReader(`{"session_id":"sess-1","last_assistant_message":"Filed it."}`), io.Discard, io.Discard, agent)
	e.cycle()
	now, _, _, _ := e.s.Queue()
	if len(now) != 1 {
		t.Fatalf("queue = %+v", now)
	}
	return now[0].ID, agent
}

// heard is what the agent's next tool call hands it from docket.
func heard(agent func(string) string) string {
	var out bytes.Buffer
	hook.Run("PostToolUse", strings.NewReader(`{"session_id":"sess-1"}`), &out, io.Discard, agent)
	return out.String()
}

// A misclick undone straight away never reaches the agent: the decision looks
// sent at once, but its delivery waits out the hold, and the Undo withdraws it
// (with nothing sent in its place, like a "reopened" note).
func TestMisclickUndoneWithinTheHoldNeverReachesTheAgent(t *testing.T) {
	for _, c := range []struct {
		name   string
		a      Action
		status string // what the docket shows straight after
	}{
		{"hand-off", Action{Kind: "handoff"}, "answered"},
		{"answer", Action{Kind: "answer", Text: "go ahead"}, "answered"},
		{"done", Action{Kind: "done"}, "done"},
		{"not worth my time", Action{Kind: "noise", Text: "file fewer of these"}, "dismissed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, "")
			e.s.hold = time.Hour
			id, agent := fileTask(t, e)
			if err := e.s.Decide(id, c.a); err != nil {
				t.Fatal(err)
			}
			if it, _, _ := e.s.st.GetItem(id); it.Status != c.status {
				t.Fatalf("status = %s, want %s", it.Status, c.status)
			}
			e.s.deliverAll(e.ctx)
			if got := heard(agent); strings.Contains(got, "[docket") {
				t.Fatalf("the agent heard it during the hold:\n%s", got)
			}
			did, _, _, _ := e.s.st.TopDecision(id)
			if err := e.s.Decide(id, Action{Kind: "undo", Decision: did}); err != nil {
				t.Fatalf("undo during the hold: %v", err)
			}
			if it, _, _ := e.s.st.GetItem(id); it.Status != "open" {
				t.Fatalf("after undo: status %s", it.Status)
			}
			if ds, _ := e.s.st.ItemDeliveries(id); len(ds) != 0 {
				t.Fatalf("deliveries left after undo: %+v", ds)
			}
			e.s.hold = 0
			e.s.deliverAll(e.ctx)
			if got := heard(agent); strings.Contains(got, "[docket") {
				t.Fatalf("the agent heard the undone decision:\n%s", got)
			}
		})
	}
}

// Once the hold is over, the deliverer is woken and the answer goes out as
// before; from then on it can't be undone.
func TestHeldAnswerGoesOutWhenTheHoldEnds(t *testing.T) {
	e := newEnv(t, "")
	id, agent := fileTask(t, e)
	e.s.hold = 300 * time.Millisecond
	for len(e.s.deliverKick) > 0 {
		<-e.s.deliverKick
	}
	start := time.Now()
	if err := e.s.Decide(id, Action{Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	e.s.deliverAll(e.ctx)
	if got := heard(agent); strings.Contains(got, "[docket") {
		t.Fatalf("delivered during the hold:\n%s", got)
	}
	ds, _ := e.s.st.ItemDeliveries(id)
	if len(ds) != 1 || ds[0].Status != "pending" || ds[0].HoldUntil < start.Add(e.s.hold).UnixMilli() {
		t.Fatalf("deliveries = %+v", ds)
	}
	select {
	case <-e.s.deliverKick:
		if waited := time.Since(start); waited < e.s.hold {
			t.Fatalf("the deliverer was woken after %v, before the hold ended", waited)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing woke the deliverer when the hold ended")
	}
	e.s.deliverAll(e.ctx)
	if got := heard(agent); !strings.Contains(got, "Handed back to you: do it yourself") {
		t.Fatalf("after the hold the agent heard %q", got)
	}
	did, _, _, _ := e.s.st.TopDecision(id)
	if err := e.s.Decide(id, Action{Kind: "undo", Decision: did}); err == nil || !strings.Contains(err.Error(), "already been delivered") {
		t.Fatalf("undo after delivery: %v", err)
	}
}

// A push machine collects its deliveries when it syncs: one still held isn't
// handed out until a sync after the hold.
func TestPushMachineGetsAHeldAnswerAfterTheHold(t *testing.T) {
	e := newEnv(t, `[[machine]]
name = "laptop"
mode = "push"
`)
	e.s.hold = 300 * time.Millisecond
	e.s.st.PatchSession("s1", store.SessionPatch{Machine: store.Str("laptop"), HasPlugin: store.Bool(true), State: store.Str("idle")})
	e.s.st.UpsertItem(store.Item{ID: "dk-eeeeeeee", Kind: "ask", Title: "q", Machine: "laptop", SessionID: "s1"})
	if err := e.s.Decide("dk-eeeeeeee", Action{Kind: "answer", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	sync := func() SyncReply {
		t.Helper()
		reply, err := e.s.Receive("laptop", strings.NewReader(`{"_cursor":"seg-2026110900.jsonl:1","_more":false}`+"\n"), nil, Hashes{Rules: "-", Context: "-"})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if r := sync(); len(r.Deliveries) != 0 {
		t.Fatalf("handed out during the hold: %+v", r.Deliveries)
	}
	time.Sleep(e.s.hold + 50*time.Millisecond)
	if r := sync(); len(r.Deliveries) != 1 || !strings.Contains(r.Deliveries[0].Text, "yes") {
		t.Fatalf("after the hold: %+v", r.Deliveries)
	}
}

// Only a decision Undo can take back is held: a note, or a lead's wake-up,
// goes out at once.
func TestOnlyDecisionsAreHeld(t *testing.T) {
	e := newEnv(t, "")
	e.s.hold = time.Hour
	id, _ := fileTask(t, e)
	it, _, _ := e.s.st.GetItem(id)
	if err := e.s.queue(it, "a note", "note"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.add(store.Delivery{ID: "01LEADWAKE", Machine: it.Machine, SessionID: it.SessionID, Nonce: "n1", Text: "[docket #n1] wake up"}); err != nil {
		t.Fatal(err)
	}
	due, _ := e.s.st.DueDeliveries()
	if len(due) != 2 {
		t.Fatalf("due = %+v", due)
	}
	for _, d := range due {
		if d.HoldUntil != 0 {
			t.Fatalf("held: %+v", d)
		}
	}
}

// The server holds a decision longer than the web UI offers to undo it, so
// an Undo the toast shows always arrives in time; and a running server holds.
func TestUndoHoldOutlastsTheToast(t *testing.T) {
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const answerUndoMs = (\d+);`).FindSubmatch(js)
	if m == nil {
		t.Fatal("app.js has no answerUndoMs")
	}
	ms, _ := strconv.Atoi(string(m[1]))
	if toastFor := time.Duration(ms) * time.Millisecond; ms < 3000 || undoHold-toastFor < 500*time.Millisecond {
		t.Fatalf("toast offers Undo for %v, server holds %v: leave at least half a second for the click to arrive", toastFor, undoHold)
	}
	cfg := newEnv(t, "").s.cfg
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.hold != undoHold {
		t.Fatalf("Open's hold = %v, want %v", s.hold, undoHold)
	}
}
