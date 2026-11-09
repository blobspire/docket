package ingest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

type harness struct {
	t *testing.T
	s *store.Store
	n int
}

func newHarness(t *testing.T) *harness {
	s, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &harness{t: t, s: s}
}

// send ingests one record and processes it.
func (h *harness) send(r record.Record) {
	h.t.Helper()
	h.n++
	if r.ID == "" {
		r.ID = fmt.Sprintf("01TEST%020d", h.n)
	}
	if r.V == 0 {
		r.V = 1
	}
	if r.Machine == "" {
		r.Machine = "lab"
	}
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(r)
	if _, err := h.s.IngestBatch(r.Machine, []store.RawEvent{{ID: r.ID, Event: r.Event, SessionID: r.SessionID, Body: b}}, ""); err != nil {
		h.t.Fatal(err)
	}
	if _, err := Process(h.s, 50); err != nil {
		h.t.Fatal(err)
	}
}

func hookRec(event, sid string, payload map[string]any) record.Record {
	payload["session_id"] = sid
	payload["hook_event_name"] = event
	b, _ := json.Marshal(payload)
	return record.Record{Event: event, SessionID: sid, Cwd: "/w/site", Payload: b}
}

func (h *harness) active() []store.Item {
	items, err := h.s.Items(store.Active...)
	if err != nil {
		h.t.Fatal(err)
	}
	return items
}

func TestFiledAskBecomesItemWithEvidence(t *testing.T) {
	h := newHarness(t)
	h.send(record.Record{Event: "cli.file", SessionID: "s1", Lead: "site", Cwd: "/w/site", Item: &record.Item{
		ID: "dk-aaaabbbb", Kind: "review", Title: "Review the new hero", Evidence: []record.Evidence{{SHA256: "abc", Name: "hero.png", Mime: "image/png", Size: 10}},
	}})
	items := h.active()
	if len(items) != 1 || items[0].ID != "dk-aaaabbbb" || items[0].Kind != "review" || items[0].Lead != "site" {
		t.Fatalf("items = %+v", items)
	}
	ev, _ := h.s.Evidence("dk-aaaabbbb")
	if len(ev) != 1 || ev[0].Name != "hero.png" {
		t.Fatalf("evidence = %+v", ev)
	}
	h.send(record.Record{Event: "cli.done", SessionID: "s1", Done: &record.Done{ItemID: "dk-aaaabbbb", Note: "merged"}})
	if n := len(h.active()); n != 0 {
		t.Fatalf("%d active after done", n)
	}
}

func TestFilingWithKeyUpdatesOneItem(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		h.send(record.Record{Event: "cli.file", Lead: "trading", Item: &record.Item{ID: fmt.Sprintf("dk-0000000%d", i), Kind: "human_task", Title: "Install the SSD", Key: "ssd"}})
	}
	items := h.active()
	if len(items) != 1 || items[0].MentionCount != 3 {
		t.Fatalf("items = %+v", items)
	}
}

func TestPermissionPromptLifecycle(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("SessionStart", "s1", map[string]any{"source": "startup"}))
	h.send(hookRec("PermissionRequest", "s1", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "git push origin main"}}))
	h.send(hookRec("Notification", "s1", map[string]any{"notification_type": "permission_prompt", "message": "Claude needs your permission"}))
	items := h.active()
	if len(items) != 1 || items[0].Kind != "blocker" || items[0].Title != "Waiting for your permission: git push origin main" {
		t.Fatalf("items = %+v", items)
	}
	sess, _, _ := h.s.GetSession("s1")
	if sess.State != "waiting" || sess.WaitingFor != "permission" {
		t.Fatalf("session = %+v", sess)
	}
	// The human answered at the terminal; the turn finished.
	h.send(hookRec("Stop", "s1", map[string]any{"last_assistant_message": "Pushed."}))
	if n := len(h.active()); n != 0 {
		t.Fatalf("permission item still open after the session continued")
	}
	sess, _, _ = h.s.GetSession("s1")
	if sess.State != "idle" || sess.LastMessage != "Pushed." {
		t.Fatalf("session = %+v", sess)
	}
}

func TestPermissionDeniedIsAnApprovalThatStaysOpen(t *testing.T) {
	h := newHarness(t)
	in := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}, "reason": "[Irreversible Local Destruction]"}
	h.send(hookRec("PermissionDenied", "s1", in))
	h.send(hookRec("PermissionDenied", "s1", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}, "reason": "[Irreversible Local Destruction]"}))
	h.send(hookRec("Stop", "s1", map[string]any{"last_assistant_message": "I couldn't clean the build directory."}))
	items := h.active()
	if len(items) != 1 || items[0].Kind != "approval" || items[0].MentionCount != 2 {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Title != "Auto mode refused: rm -rf build" {
		t.Fatalf("title = %q", items[0].Title)
	}
}

func TestLoginFailureIsCriticalHealth(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("StopFailure", "s1", map[string]any{"error": "authentication_failed", "last_assistant_message": "API Error: 401"}))
	items := h.active()
	if len(items) != 1 || items[0].Kind != "health" || !items[0].Blocking {
		t.Fatalf("items = %+v", items)
	}
}

func TestReceiptConfirmsDelivery(t *testing.T) {
	h := newHarness(t)
	h.s.UpsertItem(store.Item{ID: "dk-12345678", Kind: "ask", Title: "q", SessionID: "s1"})
	h.s.SetAnswer("dk-12345678", "yes", "answer")
	h.s.AddDelivery(store.Delivery{ID: "d1", ItemID: "dk-12345678", Machine: "lab", SessionID: "s1", Text: "yes", Nonce: "abc123"})
	h.s.DeliverySent("d1", "mailbox")
	h.send(hookRec("UserPromptSubmit", "s1", map[string]any{"prompt": "[docket dk-12345678 #abc123] Cole answered: yes"}))
	it, _, _ := h.s.GetItem("dk-12345678")
	if it.Status != "delivered" {
		t.Fatalf("status = %s", it.Status)
	}
}

func TestLegacyRawRecordsAreUnderstood(t *testing.T) {
	h := newHarness(t)
	raw := `{"id":"raw-1","v":0,"ts":"2026-11-08T00:00:00Z","event":"Notification","raw":{"session_id":"s9","notification_type":"agent_needs_input","message":"needs input"}}`
	h.s.IngestBatch("lab", []store.RawEvent{{ID: "raw-1", Body: []byte(raw)}}, "")
	if _, err := Process(h.s, 10); err != nil {
		t.Fatal(err)
	}
	items := h.active()
	if len(items) != 1 || items[0].SessionID != "s9" || items[0].Kind != "blocker" {
		t.Fatalf("items = %+v", items)
	}
}

func TestNudgeMissBecomesMaybe(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(map[string]string{"sentence": "You'll need to log in to the vendor portal.", "outcome": "miss"})
	h.send(record.Record{Event: "docket.nudge", SessionID: "s1", Payload: b})
	items := h.active()
	if len(items) != 1 || items[0].Status != "maybe" {
		t.Fatalf("items = %+v", items)
	}
}
