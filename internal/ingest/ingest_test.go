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

func TestPermissionPromptIsSessionStateOnly(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("SessionStart", "s1", map[string]any{"source": "startup"}))
	h.send(hookRec("PermissionRequest", "s1", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "git push origin main"}}))
	h.send(hookRec("Notification", "s1", map[string]any{"notification_type": "permission_prompt", "message": "Claude needs your permission"}))
	if items := h.active(); len(items) != 0 {
		t.Fatalf("a permission prompt became an item: %+v", items)
	}
	sess, _, _ := h.s.GetSession("s1")
	if sess.State != "waiting" || sess.WaitingFor != "permission: git push origin main" || sess.WaitingSince == 0 {
		t.Fatalf("session = %+v", sess)
	}
	h.send(hookRec("Stop", "s1", map[string]any{"last_assistant_message": "Pushed."}))
	sess, _, _ = h.s.GetSession("s1")
	if sess.State != "idle" || sess.LastMessage != "Pushed." || sess.WaitingSince != 0 {
		t.Fatalf("session = %+v", sess)
	}
}

func TestPermissionDeniedFilesNothing(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("PermissionDenied", "s1", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "rm -rf build"}, "reason": "[Irreversible Local Destruction]"}))
	if items := h.active(); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
}

func TestLoginFailureIsOneUrgentItemPerMachine(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("StopFailure", "s1", map[string]any{"error": "authentication_failed", "last_assistant_message": "API Error: 401"}))
	h.send(hookRec("StopFailure", "s2", map[string]any{"error": "authentication_failed", "last_assistant_message": "API Error: 401"}))
	items := h.active()
	if len(items) != 1 || items[0].Kind != "health" || !items[0].Blocking || items[0].Urgency != "now" || items[0].UrgentSince == 0 {
		t.Fatalf("items = %+v", items)
	}
	// Any session on that machine finishing a turn means the login works again.
	h.send(hookRec("Stop", "s3", map[string]any{"last_assistant_message": "Done."}))
	if n := len(h.active()); n != 0 {
		t.Fatalf("login failure still open after a turn finished on the machine")
	}
}

func TestPassingFailuresFileNothing(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("StopFailure", "s1", map[string]any{"error": "rate_limit", "last_assistant_message": "You've hit your 5-hour limit"}))
	if items := h.active(); len(items) != 0 {
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
	if sess, _, _ := h.s.GetSession("s9"); sess.State != "waiting" {
		t.Fatalf("session = %+v", sess)
	}
}

func TestOldScrapedRecordsAreIgnored(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(map[string]string{"sentence": "You'll need to log in to the vendor portal.", "outcome": "miss"})
	h.send(record.Record{Event: "docket.nudge", SessionID: "s1", Payload: b})
	h.send(record.Record{Event: "backfill.candidate", SessionID: "s1", Payload: b})
	if items := h.active(); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
}

func TestProjectsKeysAndAliases(t *testing.T) {
	h := newHarness(t)
	file := func(id, title string, it record.Item) {
		it.ID, it.Kind, it.Title = id, "ask", title
		h.send(record.Record{Event: "cli.file", SessionID: "s1", Cwd: "/w/robot/sub", Item: &it})
	}
	file("dk-00000001", "Which arm first?", record.Item{Project: "VLM Teacher", NewProject: "Robot learning", Root: "/w/robot", Key: "arm", Urgency: "now", Component: "FR5"})
	items := h.active()
	if len(items) != 1 || items[0].Project != "vlm-teacher" || items[0].Urgency != "now" || items[0].DedupeKey != "key:vlm-teacher:arm" {
		t.Fatalf("items = %+v", items)
	}
	if p, ok := h.s.GetProject("vlm-teacher"); !ok || p.Description != "Robot learning" {
		t.Fatalf("project = %+v %v", p, ok)
	}
	// A re-file without --urgency or --project keeps both, and its new id is an alias.
	file("dk-00000002", "Which arm should be measured first?", record.Item{Key: "arm", Root: "/w/robot"})
	it, _, _ := h.s.GetItem("dk-00000001")
	if it.Urgency != "now" || it.Project != "vlm-teacher" || it.Title != "Which arm should be measured first?" || it.Component != "FR5" {
		t.Fatalf("re-file changed what it shouldn't: %+v", it)
	}
	// The learned folder now places project-less filings from below it.
	h.send(record.Record{Event: "cli.file", SessionID: "s2", Cwd: "/w/robot/worktrees/x", Item: &record.Item{ID: "dk-00000003", Kind: "issue", Title: "Guard bug"}})
	if it, _, _ := h.s.GetItem("dk-00000003"); it.Project != "vlm-teacher" {
		t.Fatalf("project = %q", it.Project)
	}
	// done with the id the agent was told after the re-file works.
	h.send(record.Record{Event: "cli.done", SessionID: "s1", Done: &record.Done{ItemID: "dk-00000002"}})
	if it, _, _ := h.s.GetItem("dk-00000001"); it.Status != "done" {
		t.Fatalf("status = %s", it.Status)
	}
}

func TestDoneByKeyAndNeverOverDismissal(t *testing.T) {
	h := newHarness(t)
	h.send(record.Record{Event: "cli.file", SessionID: "w1", Item: &record.Item{ID: "dk-0000000a", Kind: "issue", Title: "cw-live killed", Key: "cw-live", Project: "cwant", Urgency: "now"}})
	h.send(record.Record{Event: "cli.done", SessionID: "w1", Done: &record.Done{Key: "cw-live", Project: "cwant", Note: "recovered"}})
	if it, _, _ := h.s.GetItem("dk-0000000a"); it.Status != "done" {
		t.Fatalf("done --key: status = %s", it.Status)
	}
	// Without a project, the same session's key still resolves.
	h.send(record.Record{Event: "cli.file", SessionID: "w1", Item: &record.Item{ID: "dk-0000000b", Kind: "issue", Title: "cw-live killed", Key: "cw-live", Project: "cwant"}})
	h.send(record.Record{Event: "cli.done", SessionID: "w1", Done: &record.Done{Key: "cw-live"}})
	if it, _, _ := h.s.GetItem("dk-0000000b"); it.Status != "done" {
		t.Fatalf("session fallback: status = %s", it.Status)
	}
	// An agent's done never overwrites the human's dismissal.
	h.s.UpsertItem(store.Item{ID: "dk-0000000c", Kind: "ask", Title: "q", SessionID: "w1"})
	h.s.SetStatusWith("dk-0000000c", "dismissed", "Not worth Cole's time", "noise", "too small")
	h.send(record.Record{Event: "cli.done", SessionID: "w1", Done: &record.Done{ItemID: "dk-0000000c"}})
	if it, _, _ := h.s.GetItem("dk-0000000c"); it.Status != "dismissed" {
		t.Fatalf("status = %s", it.Status)
	}
}

func TestNoisedKeyIsSuppressedForAWeek(t *testing.T) {
	h := newHarness(t)
	h.send(record.Record{Event: "cli.file", SessionID: "l1", Item: &record.Item{ID: "dk-0000000d", Kind: "fyi", Title: "Heartbeat ok", Key: "hb", Project: "cwant"}})
	h.s.SetStatusWith("dk-0000000d", "dismissed", "Not worth Cole's time", "noise", "")
	h.send(record.Record{Event: "cli.file", SessionID: "l1", Item: &record.Item{ID: "dk-0000000e", Kind: "fyi", Title: "Heartbeat ok", Key: "hb", Project: "cwant"}})
	if items := h.active(); len(items) != 0 {
		t.Fatalf("a re-file of a noised key reached the queue: %+v", items)
	}
}

func TestCorrectionsAreLogged(t *testing.T) {
	h := newHarness(t)
	h.send(hookRec("UserPromptSubmit", "s1", map[string]any{"prompt": "no we definitely don't want to include the unfit episodes"}))
	h.send(hookRec("UserPromptSubmit", "s1", map[string]any{"prompt": "great, ship it"}))
	h.send(hookRec("UserPromptSubmit", "s1", map[string]any{"prompt": "[docket dk-12345678 #abc123] Cole answered: no"}))
	ds, err := h.s.DecisionsSince(0, 10)
	if err != nil || len(ds) != 1 || ds[0].Action != "correction" || ds[0].Context != "site" {
		t.Fatalf("decisions = %+v %v", ds, err)
	}
}
