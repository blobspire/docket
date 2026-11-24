package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/agentctx"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// fakeNtfy counts pushes.
func fakeNtfy(t *testing.T) (*httptest.Server, func() int) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func pushEnv(t *testing.T, ntfy string) *env {
	return newEnv(t, `person = "Cole"
public_url = "https://docket.example"
[ntfy]
server = "`+ntfy+`"
topic = "t"
[[machine]]
name = "box"
mode = "local"
`)
}

var recSeq int

// send ingests one record from machine "box" and processes it.
func (e *env) send(t *testing.T, r record.Record, o ingest.Options) {
	t.Helper()
	recSeq++
	r.V, r.ID, r.Machine = 1, fmt.Sprintf("01REVIEW%018d", recSeq), "box"
	r.TS = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(r)
	if _, err := e.s.st.IngestBatch("box", []store.RawEvent{{ID: r.ID, TS: r.TS, Event: r.Event, SessionID: r.SessionID, Body: b}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.ProcessWith(e.s.st, 50, o); err != nil {
		t.Fatal(err)
	}
}

// file sends one agent filing through ingest, as the CLI would.
func (e *env) file(t *testing.T, sid string, it record.Item) {
	t.Helper()
	e.send(t, record.Record{Event: "cli.file", SessionID: sid, Item: &it}, ingest.Options{})
}

func active(t *testing.T, st *store.Store) []store.Item {
	t.Helper()
	items, err := st.Items("open", "snoozed", "answered")
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestNoiseSuppressesEveryRefileForAWeek(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "lead", record.Item{ID: "dk-hb000001", Kind: "fyi", Title: "Heartbeat ok", Key: "hb", Project: "cwant"})
	if err := e.s.Decide("dk-hb000001", Action{Kind: "noise", Text: "heartbeats don't need me"}); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 4; i++ {
		e.file(t, "lead", record.Item{ID: "dk-hb00000" + string(rune('0'+i)), Kind: "fyi", Title: "Heartbeat ok", Key: "hb", Project: "cwant"})
		if a := active(t, e.s.st); len(a) != 0 {
			t.Fatalf("re-file %d of a noised key reached the queue: %+v", i, a)
		}
	}
	// A plain dismissal is not "not worth my time": the next filing shows.
	e.file(t, "lead", record.Item{ID: "dk-ci000001", Kind: "issue", Title: "CI red", Key: "ci", Project: "cwant"})
	e.s.Decide("dk-ci000001", Action{Kind: "dismiss"})
	e.file(t, "lead", record.Item{ID: "dk-ci000002", Kind: "issue", Title: "CI red again", Key: "ci", Project: "cwant"})
	if a := active(t, e.s.st); len(a) != 1 {
		t.Fatalf("a plain dismiss suppressed the next filing: %+v", a)
	}
	// A week-old mark no longer suppresses.
	e.s.st.DB.Exec(`UPDATE decisions SET ts=ts-8*86400 WHERE action='noise'`)
	e.file(t, "lead", record.Item{ID: "dk-hb000009", Kind: "fyi", Title: "Heartbeat ok", Key: "hb", Project: "cwant"})
	if a := active(t, e.s.st); len(a) != 2 {
		t.Fatalf("an 8-day-old mark still suppresses: %+v", a)
	}
}

func TestRefileFromAnotherSessionGetsTheAnswer(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("monday", store.SessionPatch{Machine: store.Str("box"), State: store.Str("ended"), HasPlugin: store.Bool(true)})
	st.PatchSession("tuesday", store.SessionPatch{Machine: store.Str("box"), State: store.Str("idle"), HasPlugin: store.Bool(true)})
	e.file(t, "monday", record.Item{ID: "dk-mon00001", Kind: "ask", Title: "Raise the CI limit?", Key: "ci-limit", Project: "windward"})
	e.file(t, "tuesday", record.Item{ID: "dk-tue00001", Kind: "ask", Title: "Raise the CI limit? (still red)", Key: "ci-limit", Project: "windward"})
	if err := e.s.Decide("dk-mon00001", Action{Kind: "approve"}); err != nil {
		t.Fatal(err)
	}
	ds, _ := st.ItemDeliveries("dk-mon00001")
	if len(ds) != 1 || ds[0].SessionID != "tuesday" || !strings.Contains(ds[0].Text, "dk-tue00001") {
		t.Fatalf("the answer must reach the waiting session, naming the id it was told: %+v", ds)
	}
}

func TestEscalationReopensAndPushes(t *testing.T) {
	ntfy, pushes := fakeNtfy(t)
	e := pushEnv(t, ntfy.URL)
	st := e.s.st
	// Snoozed, then raised to now: back in the queue, and it pushes.
	e.file(t, "s1", record.Item{ID: "dk-escsno1", Kind: "ask", Title: "Launch before Friday?", Key: "launch", Project: "windward"})
	st.Snooze("dk-escsno1", time.Now().Add(24*time.Hour).Unix())
	e.file(t, "s1", record.Item{ID: "dk-escsno2", Kind: "ask", Title: "Launch before Friday?", Key: "launch", Project: "windward", Urgency: "now"})
	if it, _, _ := st.GetItem("dk-escsno1"); it.Status != "open" || it.Urgency != "now" || it.UrgentSince == 0 {
		t.Fatalf("snoozed item after escalation: %+v", it)
	}
	// Answered, then raised to now: its answer is on the way; it stays
	// answered (the receipt makes it delivered) and never pushes.
	e.file(t, "s1", record.Item{ID: "dk-escans1", Kind: "ask", Title: "Which room?", Key: "room", Project: "windward"})
	st.SetAnswer("dk-escans1", "room 2", "answer")
	e.file(t, "s1", record.Item{ID: "dk-escans2", Kind: "ask", Title: "Which room?", Key: "room", Project: "windward", Urgency: "now"})
	if it, _, _ := st.GetItem("dk-escans1"); it.Status != "answered" {
		t.Fatalf("answered item after escalation: %s", it.Status)
	}
	st.DB.Exec(`UPDATE items SET urgent_since=urgent_since-120 WHERE urgent_since>0`)
	e.s.pushes(e.ctx)
	if pushes() != 1 {
		t.Fatalf("pushes = %d, want 1 (the reopened snoozed item only)", pushes())
	}
	// Re-filing it at the same urgency later doesn't re-arm the push.
	e.file(t, "s1", record.Item{ID: "dk-escsno3", Kind: "ask", Title: "Launch before Friday?", Key: "launch", Project: "windward", Urgency: "now"})
	st.DB.Exec(`UPDATE items SET urgent_since=urgent_since-120 WHERE urgent_since>0`)
	e.s.pushes(e.ctx)
	if pushes() != 1 {
		t.Fatalf("a same-urgency re-file re-armed the push: %d", pushes())
	}
}

func TestFolderDefaultFollowsTheMajority(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	root := "/w/docket"
	e.file(t, "a", record.Item{ID: "dk-f0000001", Kind: "ask", Title: "errand", Project: "cwant", Explicit: true, Root: root})
	for i := 0; i < 3; i++ {
		e.file(t, "a", record.Item{ID: "dk-f000001" + string(rune('0'+i)), Kind: "ask", Title: "docket work", Project: "docket", Explicit: true, Root: root})
	}
	// A project the CLI only filled in from the folder or lead never votes.
	for i := 0; i < 5; i++ {
		e.file(t, "a", record.Item{ID: "dk-f000002" + string(rune('0'+i)), Kind: "ask", Title: "lead default", Project: "cwant", Root: root})
	}
	dirs, _ := st.ProjectDirs("box")
	if len(dirs) != 1 || dirs[0].Project != "docket" {
		t.Fatalf("dirs = %+v", dirs)
	}
	// One more errand doesn't take the folder from the established project.
	e.file(t, "a", record.Item{ID: "dk-f0000031", Kind: "ask", Title: "another errand", Project: "cwant", Explicit: true, Root: root})
	if dirs, _ := st.ProjectDirs("box"); dirs[0].Project != "docket" {
		t.Fatalf("one errand re-homed the folder: %+v", dirs)
	}
}

func TestLoginFailureIsHeldWhenDismissed(t *testing.T) {
	ntfy, pushes := fakeNtfy(t)
	e := pushEnv(t, ntfy.URL)
	st := e.s.st
	fail := func() {
		b, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "StopFailure", "error": "authentication_failed"})
		e.send(t, record.Record{Event: "StopFailure", SessionID: "s1", Payload: b}, ingest.Options{SystemProject: "admin"})
	}
	fail()
	items := active(t, st)
	if len(items) != 1 || items[0].Project != "admin" {
		t.Fatalf("items = %+v", items)
	}
	st.DB.Exec(`UPDATE items SET urgent_since=urgent_since-120`)
	e.s.pushes(e.ctx)
	e.s.Decide(items[0].ID, Action{Kind: "dismiss"})
	for i := 0; i < 3; i++ {
		fail()
		st.DB.Exec(`UPDATE items SET urgent_since=urgent_since-120`)
		e.s.pushes(e.ctx)
	}
	if pushes() != 1 {
		t.Fatalf("a dismissed login failure pushed again: %d pushes", pushes())
	}
	if a := active(t, st); len(a) != 1 || a[0].Status != "snoozed" {
		t.Fatalf("items = %+v", a)
	}
}

func TestStuckLeadClosesWhenItsSessionEnds(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[lead]]
name = "orch"
machine = "box"
dir = "/tmp/orch-test"
`)
	st := e.s.st
	st.PatchSession("L1", store.SessionPatch{Machine: store.Str("box"), Lead: store.Str("orch"), State: store.Str("waiting"), WaitingFor: store.Str("permission"), WaitingSince: store.Int(time.Now().Add(-time.Hour).Unix()), HasPlugin: store.Bool(true)})
	e.s.watchHealth(e.ctx)
	a := active(t, st)
	if len(a) != 1 {
		t.Fatalf("no stuck item: %+v", a)
	}
	// Done on it never sends anything to (and so never resumes) the session.
	e.s.Decide(a[0].ID, Action{Kind: "done"})
	if ds, _ := st.ItemDeliveries(a[0].ID); len(ds) != 0 {
		t.Fatalf("done on a watchdog item queued a delivery: %+v", ds)
	}
	e.s.watchHealth(e.ctx)
	st.PatchSession("L1", store.SessionPatch{State: store.Str("ended")})
	e.s.watchHealth(e.ctx)
	if a := active(t, st); len(a) != 0 {
		t.Fatalf("still open after the session ended: %+v", a)
	}
}

func TestIdleLeadIsNotStuck(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[lead]]
name = "orch"
machine = "box"
dir = "/tmp/orch-test"
`)
	b, _ := json.Marshal(map[string]any{"session_id": "L1", "hook_event_name": "Notification", "notification_type": "idle_prompt"})
	e.send(t, record.Record{Event: "Notification", SessionID: "L1", Lead: "orch", Payload: b}, ingest.Options{})
	e.s.st.DB.Exec(`UPDATE sessions SET waiting_since=? WHERE id='L1' AND waiting_since>0`, time.Now().Add(-time.Hour).Unix())
	e.s.watchHealth(e.ctx)
	if a := active(t, e.s.st); len(a) != 0 {
		t.Fatalf("an idle lead was reported stuck: %+v", a)
	}
}

func TestWatchdogRecheckChangesNothing(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.SetQuota(store.Quota{Machine: "box", Account: "Personal", Window: "seven_day", UsedPct: 91, CapturedAt: time.Now().Unix()})
	e.s.watchHealth(e.ctx)
	d1 := st.QueueDigest()
	c1, _ := e.s.ContextFor("box")
	time.Sleep(1100 * time.Millisecond)
	st.SetQuota(store.Quota{Machine: "box", Account: "Personal", Window: "seven_day", UsedPct: 92, CapturedAt: time.Now().Unix()})
	e.s.watchHealth(e.ctx)
	if d2 := st.QueueDigest(); d2 != d1 {
		t.Fatal("a re-check of the same condition changed the queue digest (the page would reload)")
	}
	if c2, _ := e.s.ContextFor("box"); string(c2) != string(c1) {
		t.Fatal("a re-check of the same condition changed context.json (it would be resent)")
	}
	// A real change in the same second still changes it.
	st.UpsertItem(store.Item{ID: "dk-new00001", Kind: "ask", Title: "new"})
	if st.QueueDigest() == d1 {
		t.Fatal("the digest missed a new item")
	}
}

func TestNoiseFeedbackNeverWakesTheSession(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("live", store.SessionPatch{Machine: store.Str("box"), State: store.Str("idle"), HasPlugin: store.Bool(true)})
	e.file(t, "live", record.Item{ID: "dk-q0000001", Kind: "review", Title: "PR merged", Project: "x"})
	if err := e.s.Decide("dk-q0000001", Action{Kind: "noise", Text: "no merged-PR reports"}); err != nil {
		t.Fatal(err)
	}
	e.s.deliverAll(e.ctx)
	getenv := func(k string) string {
		return map[string]string{"DOCKET_WAIT_POLL": "20ms", "DOCKET_WAIT_MAX": "400ms"}[k]
	}
	if code, _ := hook.Run("Wait", strings.NewReader(`{"session_id":"live"}`), &bytes.Buffer{}, &bytes.Buffer{}, getenv); code != 0 {
		t.Fatalf("the noise feedback woke the idle session (code %d)", code)
	}
	var out bytes.Buffer
	hook.Run("Stop", strings.NewReader(`{"session_id":"live","last_assistant_message":"ok"}`), &out, &bytes.Buffer{}, getenv)
	if !strings.Contains(out.String(), "no merged-PR reports") {
		t.Fatalf("the next turn didn't get the feedback: %q", out.String())
	}
	// A second mark on the closed item is refused, so nothing is sent twice.
	if err := e.s.Decide("dk-q0000001", Action{Kind: "noise"}); err == nil {
		t.Fatal("noise on a closed item was accepted")
	}
}

func TestContextTellsAMachineWhatWasProcessed(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s1", record.Item{ID: "dk-s0000001", Kind: "ask", Title: "a", Key: "k", Project: "p"})
	e.file(t, "s1", record.Item{ID: "dk-s0000002", Kind: "ask", Title: "a again", Key: "k", Project: "p"})
	e.s.Decide("dk-s0000001", Action{Kind: "dismiss"})
	data, _ := e.s.ContextFor("box")
	var c agentctx.Context
	json.Unmarshal(data, &c)
	seen := strings.Join(c.Seen, ",")
	if !strings.Contains(seen, "dk-s0000001") || !strings.Contains(seen, "dk-s0000002") || len(c.Open) != 0 {
		t.Fatalf("seen=%q open=%+v", seen, c.Open)
	}
	// The machine's own record of those filings now defers to the server.
	m := c.Merged(agentctx.Overlay{Filed: []agentctx.Item{{ID: "dk-s0000001", Title: "a"}, {ID: "dk-s0000002", Title: "a again"}, {ID: "dk-unseen01", Title: "not yet processed", CreatedAt: time.Now().Unix()}}})
	if len(m.Open) != 1 || m.Open[0].ID != "dk-unseen01" {
		t.Fatalf("merged open = %+v", m.Open)
	}
}

func TestClosedItemsCantBeActedOnAgain(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-c1000001", Kind: "ask", Title: "q", SessionID: "s", Source: "cli", Machine: "box"})
	e.s.Decide("dk-c1000001", Action{Kind: "done", Text: "settled in chat"})
	for _, k := range []string{"snooze", "answer", "approve", "dismiss", "noise"} {
		if err := e.s.Decide("dk-c1000001", Action{Kind: k, Text: "x", Until: time.Now().Add(time.Hour)}); err == nil {
			t.Errorf("%s on a done item was accepted", k)
		}
	}
	if it, _, _ := st.GetItem("dk-c1000001"); it.Status != "done" {
		t.Fatalf("status = %s", it.Status)
	}
	// Its page offers no actions and no answer, only the way back and a
	// question for its agent (Ask the agent, which leaves it closed).
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/item/dk-c1000001", nil))
	body := rec.Body.String()
	if strings.Contains(body, `data-act="snooze"`) || strings.Contains(body, `name="action" value="answer"`) || !strings.Contains(body, `data-act="reopen"`) || !strings.Contains(body, `name="action" value="ask"`) {
		t.Fatal("a closed item's page still offers actions (or no way to reopen it, or no Ask)")
	}
	// Undo reopens it without the stale closing note.
	e.s.Decide("dk-c1000001", Action{Kind: "undo"})
	if it, _, _ := st.GetItem("dk-c1000001"); it.Status != "open" || it.Resolution != "" {
		t.Fatalf("after undo: %s %q", it.Status, it.Resolution)
	}
	// Dismissed and expired are just as closed; delivered takes a follow-up
	// answer, or a close (it's waiting on its agent), but no second answer.
	for _, status := range []string{"dismissed", "expired", "delivered"} {
		id := "dk-cs" + status[:3] + "01"
		st.UpsertItem(store.Item{ID: id, Kind: "approval", Title: "q", SessionID: "s", Source: "cli", Machine: "box"})
		st.DB.Exec(`UPDATE items SET status=? WHERE id=?`, status, id)
		refused := []string{"approve", "decline", "snooze", "done", "dismiss", "noise"}
		if status == "delivered" {
			refused = []string{"approve", "decline", "snooze", "noise"}
		}
		for _, k := range refused {
			if err := e.s.Decide(id, Action{Kind: k, Until: time.Now().Add(time.Hour)}); err == nil {
				t.Errorf("%s on a %s item was accepted", k, status)
			}
		}
		if err := e.s.Decide(id, Action{Kind: "answer", Text: "x"}); (err == nil) != (status == "delivered") {
			t.Errorf("answer on a %s item: err=%v", status, err)
		}
		if ds, _ := st.ItemDeliveries(id); (len(ds) > 0) != (status == "delivered") {
			t.Errorf("deliveries for a %s item: %+v", status, ds)
		}
	}
	// Reopen brings a closed item back.
	st.DB.Exec(`UPDATE items SET status='dismissed' WHERE id='dk-csdis01'`)
	if err := e.s.Decide("dk-csdis01", Action{Kind: "reopen"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-csdis01"); it.Status != "open" {
		t.Fatalf("after reopen: %s", it.Status)
	}
}

func TestMarkdownImagesBecomeLinks(t *testing.T) {
	out := string(renderMarkdown("see ![chart](https://tracker.example/p.png) and https://example.com/x"))
	if strings.Contains(out, "<img") || !strings.Contains(out, `href="https://tracker.example/p.png"`) {
		t.Fatalf("an image would load from the agent's host: %s", out)
	}
	if strings.Count(out, `target="_blank"`) != 2 || strings.Count(out, `rel="noopener noreferrer"`) != 2 {
		t.Fatalf("links (including bare URLs) must open in a new tab without the opener: %s", out)
	}
	long := strings.Repeat("word ", 3000)
	if short := string(renderMarkdownShort(long, "dk-x")); len(short) > shortBody+500 || !strings.Contains(short, "/item/dk-x") {
		t.Fatalf("a long body isn't cut in rows (%d bytes)", len(short))
	}
}

func TestRedirectsStayOnSite(t *testing.T) {
	for _, p := range []string{"/", "/?p=admin&sort=newest", "/item/dk-1"} {
		if !localPath(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range []string{"", "//evil.example", `/\evil.example/x`, "/\t/evil.example", "https://evil.example", "/\x00x"} {
		if localPath(p) {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestUpgradeClosesGuessedItemsAndAdoptsOldKeys(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-u0000001", Kind: "ask", Title: "Want me to run it?", Status: "maybe", Source: "backfill"})
	st.UpsertItem(store.Item{ID: "dk-u0000002", Kind: "approval", Title: "Auto mode refused: rm -rf build", Source: "permission_denied"})
	st.UpsertItem(store.Item{ID: "dk-u0000003", Kind: "ask", Title: "Real ask", Source: "cli", Machine: "box", Cwd: "/w/mac-repo/sub", DedupeKey: "key:mac-repo:arm-plug", CreatedAt: time.Now().Add(-time.Hour).Unix()})
	// Decoys: the same key in a real project, from another repo, and from another machine.
	st.UpsertItem(store.Item{ID: "dk-u0000005", Kind: "ask", Title: "Windward's", Source: "cli", Machine: "box", Project: "windward", DedupeKey: "key:windward:arm-plug"})
	st.UpsertItem(store.Item{ID: "dk-u0000006", Kind: "ask", Title: "Another repo's", Source: "cli", Machine: "box", Cwd: "/w/other", DedupeKey: "key:other:arm-plug", CreatedAt: time.Now().Add(-time.Minute).Unix()})
	st.UpsertItem(store.Item{ID: "dk-u0000007", Kind: "ask", Title: "Another machine's", Source: "cli", Machine: "lap", Cwd: "/w/mac-repo", DedupeKey: "key:mac-repo2:arm-plug"})
	// A live sign-out alert from the current version must survive any reopen.
	st.UpsertItem(store.Item{ID: "dk-u0000008", Kind: "health", Title: "Claude Code is signed out on box", Source: "stop_failure", Machine: "box", DedupeKey: "auth:box:auth_failed"})
	st.DB.Exec(`DELETE FROM kv WHERE k='upgrade:agents-file'`)
	upgraded, err := store.Open(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	upgraded.Close()
	a := active(t, st)
	if len(a) != 5 {
		t.Fatalf("after the upgrade: %+v", a)
	}
	// It runs once: a later open (every laptop sync opens the store) leaves
	// the live alert alone even if it looked like an old per-session one.
	st.DB.Exec(`UPDATE items SET session_id='s-now' WHERE id='dk-u0000008'`)
	st.UpsertItem(store.Item{ID: "dk-u0000009", Kind: "ask", Title: "filed after the upgrade", Status: "maybe", Source: "backfill"})
	again, _ := store.Open(st.Path())
	again.Close()
	if it, _, _ := st.GetItem("dk-u0000008"); it.Status != "open" {
		t.Fatalf("a reopen closed a live alert: %s", it.Status)
	}
	if it, _, _ := st.GetItem("dk-u0000009"); it.Status != "maybe" {
		t.Fatalf("the one-time upgrade ran again: %s", it.Status)
	}
	// The first post-upgrade re-file of the old key from that repo updates
	// it, not a duplicate, and touches none of the decoys.
	e.file(t, "s", record.Item{ID: "dk-u0000004", Kind: "ask", Title: "Real ask, still", Key: "arm-plug", Project: "docket", Root: "/w/mac-repo"})
	it, _, _ := st.GetItem("dk-u0000003")
	if it.Project != "docket" || it.Title != "Real ask, still" {
		t.Fatalf("not adopted: %+v", it)
	}
	if _, ok, _ := st.GetItem("dk-u0000004"); ok {
		t.Fatal("a duplicate was created")
	}
	for id, title := range map[string]string{"dk-u0000005": "Windward's", "dk-u0000006": "Another repo's", "dk-u0000007": "Another machine's"} {
		if d, _, _ := st.GetItem(id); d.Title != title || d.Status != "open" {
			t.Errorf("decoy %s changed: %+v", id, d)
		}
	}
}

func TestProjectEditRenamesWithDescription(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.EnsureProject("ladr", "LADR website")
	form := strings.NewReader("rename=lab-site&description=The+lab+website")
	req := httptest.NewRequest("POST", "/project/ladr", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	if p, ok := e.s.st.GetProject("lab-site"); !ok || p.Description != "The lab website" || rec.Header().Get("Location") != "/?p=lab-site" {
		t.Fatalf("project = %+v %v, location %q", p, ok, rec.Header().Get("Location"))
	}
	req = httptest.NewRequest("POST", "/project/lab-site", strings.NewReader("rename=!!!"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("an unusable name was accepted silently: %q", rec.Header().Get("Location"))
	}
}

func TestSeenIncludesAppliedDoneAndProjectRecords(t *testing.T) {
	e := newEnv(t, "")
	b, _ := json.Marshal(map[string]string{"name": "robots", "description": "Robot arm work"})
	e.send(t, record.Record{Event: "cli.project", SessionID: "s", Payload: b}, ingest.Options{})
	projRec := fmt.Sprintf("01REVIEW%018d", recSeq)
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{Key: "nothing"}}, ingest.Options{})
	doneRec := fmt.Sprintf("01REVIEW%018d", recSeq)
	data, _ := e.s.ContextFor("box")
	var c agentctx.Context
	json.Unmarshal(data, &c)
	seen := strings.Join(c.Seen, ",")
	if !strings.Contains(seen, projRec) || !strings.Contains(seen, doneRec) {
		t.Fatalf("seen = %q, want %s and %s", seen, projRec, doneRec)
	}
	if p, ok := e.s.st.GetProject("robots"); !ok || p.Description != "Robot arm work" {
		t.Fatalf("projects add didn't create it: %+v", p)
	}
}

// sendFrom is send from another machine.
func (e *env) sendFrom(t *testing.T, machine string, r record.Record) {
	t.Helper()
	recSeq++
	r.V, r.ID, r.Machine = 1, fmt.Sprintf("01REVIEW%018d", recSeq), machine
	r.TS = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(r)
	if _, err := e.s.st.IngestBatch(machine, []store.RawEvent{{ID: r.ID, TS: r.TS, Event: r.Event, SessionID: r.SessionID, Body: b}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Process(e.s.st, 50); err != nil {
		t.Fatal(err)
	}
}

func TestRefileFollowsTheFilerAcrossMachines(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "mon", record.Item{ID: "dk-x0000001", Kind: "ask", Title: "Raise the limit?", Key: "lim", Project: "w"})
	e.sendFrom(t, "lap", record.Record{Event: "cli.file", SessionID: "tue", Item: &record.Item{ID: "dk-x0000002", Kind: "ask", Title: "Raise the limit?", Key: "lim", Project: "w"}})
	e.s.Decide("dk-x0000001", Action{Kind: "approve"})
	ds, _ := st.ItemDeliveries("dk-x0000001")
	if len(ds) != 1 || ds[0].Machine != "lap" || ds[0].SessionID != "tue" {
		t.Fatalf("the answer must go to the re-filer's machine and session: %+v", ds)
	}
	// A filing with no session (cron, CI) doesn't move it off its session's machine.
	e.sendFrom(t, "ci", record.Record{Event: "cli.file", Item: &record.Item{ID: "dk-x0000003", Kind: "ask", Title: "Raise the limit?", Key: "lim", Project: "w"}})
	if it, _, _ := st.GetItem("dk-x0000001"); it.Machine != "lap" || it.SessionID != "tue" {
		t.Fatalf("a session-less re-file moved it: machine=%s session=%s", it.Machine, it.SessionID)
	}
}

func TestDismissedLoginFailureStaysHeldADay(t *testing.T) {
	ntfy, pushes := fakeNtfy(t)
	e := pushEnv(t, ntfy.URL)
	st := e.s.st
	b, _ := json.Marshal(map[string]any{"session_id": "s1", "error": "authentication_failed"})
	e.send(t, record.Record{Event: "StopFailure", SessionID: "s1", Payload: b}, ingest.Options{})
	it := active(t, st)[0]
	st.MarkPushed([]string{it.ID})
	e.s.Decide(it.ID, Action{Kind: "dismiss"})
	st.Wake()
	st.DB.Exec(`UPDATE items SET urgent_since=urgent_since-120`)
	e.s.pushes(e.ctx)
	if got, _, _ := st.GetItem(it.ID); got.Status != "snoozed" || pushes() != 0 {
		t.Fatalf("status=%s pushes=%d", got.Status, pushes())
	}
	// "Not worth my time" on it keeps the next failure out entirely.
	st.DB.Exec(`UPDATE items SET status='open' WHERE id=?`, it.ID)
	e.s.Decide(it.ID, Action{Kind: "noise"})
	e.send(t, record.Record{Event: "StopFailure", SessionID: "s1", Payload: b}, ingest.Options{})
	if a := active(t, st); len(a) != 0 {
		t.Fatalf("a noised login alert came back: %+v", a)
	}
}

func TestFolderMajorityIsExact(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	root := "/w/repo"
	n := 0
	file := func(project string, explicit bool) {
		n++
		e.file(t, "a", record.Item{ID: fmt.Sprintf("dk-mj%06d", n), Kind: "ask", Title: "x", Project: project, Explicit: explicit, Root: root})
	}
	dir := func() string { d, _ := st.ProjectDirs("box"); return d[0].Project }
	for i := 0; i < 3; i++ {
		file("docket", true)
	}
	file("cwant", true)
	file("cwant", true)
	if dir() != "docket" {
		t.Fatalf("two errands against three choices moved it: %s", dir())
	}
	file("cwant", true)
	if dir() != "cwant" {
		t.Fatalf("the third contrary choice should win it: %s", dir())
	}
	// Filings that used the default confirm it.
	for i := 0; i < 5; i++ {
		file("cwant", false)
	}
	file("docket", true)
	file("docket", true)
	if dir() != "cwant" {
		t.Fatalf("defaulted filings didn't count: %s", dir())
	}
	// A default that came from elsewhere (a lead, a parent folder) for a
	// different project never votes against the folder's own.
	for i := 0; i < 8; i++ {
		file("ops", false)
	}
	if dir() != "cwant" {
		t.Fatalf("defaulted filings for another project moved it: %s", dir())
	}
	// Nor does one teach a folder that has no mapping yet.
	e.file(t, "a", record.Item{ID: "dk-mjfresh1", Kind: "ask", Title: "x", Project: "ops", Root: "/w/fresh"})
	if d, _ := st.ProjectDirs("box"); len(d) != 1 {
		t.Fatalf("a defaulted filing created a mapping: %+v", d)
	}
}

func TestDigestSeesWhatThePageShows(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("s", store.SessionPatch{State: store.Str("busy")})
	st.UpsertItem(store.Item{ID: "dk-dg000001", Kind: "ask", Title: "q", SessionID: "s", Blocking: true})
	d0 := st.QueueDigest()
	st.PatchSession("s", store.SessionPatch{State: store.Str("waiting")})
	d1 := st.QueueDigest()
	if d1 == d0 {
		t.Fatal("the agent pausing on an item didn't change the digest")
	}
	for _, q := range []string{`title='q2'`, `component='c'`, `place='lab'`, `due='2030-01-01'`, `urgency='soon'`, `project='p'`} {
		st.DB.Exec(`UPDATE items SET ` + q + ` WHERE id='dk-dg000001'`)
		if d := st.QueueDigest(); d == d1 {
			t.Errorf("changing %s (same second) didn't change the digest", q)
		} else {
			d1 = d
		}
	}
}

func TestSeenIsThisMachinesRecentWork(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-sn000001", Kind: "ask", Title: "mine", Machine: "box"})
	st.UpsertItem(store.Item{ID: "dk-sn000002", Kind: "ask", Title: "other machine", Machine: "lap"})
	st.UpsertItem(store.Item{ID: "dk-sn000003", Kind: "ask", Title: "old", Machine: "box", CreatedAt: time.Now().Add(-30 * 24 * time.Hour).Unix()})
	data, _ := e.s.ContextFor("box")
	var c agentctx.Context
	json.Unmarshal(data, &c)
	seen := strings.Join(c.Seen, ",")
	if !strings.Contains(seen, "dk-sn000001") || strings.Contains(seen, "dk-sn000002") || strings.Contains(seen, "dk-sn000003") {
		t.Fatalf("seen = %q", seen)
	}
}

func TestNoiseLastsTheWholeWeek(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "l", record.Item{ID: "dk-nw000001", Kind: "fyi", Title: "hb", Key: "hb", Project: "c"})
	e.s.Decide("dk-nw000001", Action{Kind: "noise"})
	e.s.st.DB.Exec(`UPDATE decisions SET ts=ts-6*86400 WHERE action='noise'`)
	e.file(t, "l", record.Item{ID: "dk-nw000002", Kind: "fyi", Title: "hb", Key: "hb", Project: "c"})
	if a := active(t, e.s.st); len(a) != 0 {
		t.Fatalf("a 6-day-old mark stopped suppressing: %+v", a)
	}
}

func TestPushTimeoutCountsOnlyWhenSent(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 15 s push timeout")
	}
	// A listener that accepts and never answers: the message was written.
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	defer silent.Close()
	e := pushEnv(t, silent.URL)
	e.s.st.UpsertItem(store.Item{ID: "dk-pt000001", Kind: "issue", Title: "down", Urgency: "now", UrgentSince: time.Now().Add(-time.Hour).Unix()})
	e.s.pushes(e.ctx)
	if it, _, _ := e.s.st.GetItem("dk-pt000001"); it.PushedAt == 0 {
		t.Fatal("a push ntfy received but answered late was sent again")
	}
}

func TestQuotaJudgesTheNewestSnapshot(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	now := time.Now().Unix()
	st.SetQuota(store.Quota{Machine: "box", Account: "Personal", Window: "seven_day", UsedPct: 95, CapturedAt: now - 3600})
	st.SetQuota(store.Quota{Machine: "lap", Account: "Personal", Window: "seven_day", UsedPct: 20, CapturedAt: now})
	e.s.watchHealth(e.ctx)
	if a := active(t, st); len(a) != 0 {
		t.Fatalf("a stale high reading opened an item over a fresh low one: %+v", a)
	}
}

func TestActRedirectStaysOnSite(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-rd000001", Kind: "issue", Title: "x"})
	req := httptest.NewRequest("POST", "/item/dk-rd000001/act", strings.NewReader("action=done&back=//evil.example/x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("redirected to %q", loc)
	}
}

func TestExplicitProjectDoneStaysInIt(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s", record.Item{ID: "dk-ex000001", Kind: "issue", Title: "ci", Key: "ci", Project: "cwant"})
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{Key: "ci", Project: "windward", Explicit: true}}, ingest.Options{})
	if it, _, _ := e.s.st.GetItem("dk-ex000001"); it.Status != "open" {
		t.Fatal("done --key ci --project windward closed cwant's item")
	}
}

func TestMovedItemStillTakesItsKey(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s", record.Item{ID: "dk-mv000001", Kind: "issue", Title: "cert expiring", Key: "cert", Project: "site"})
	e.s.Decide("dk-mv000001", Action{Kind: "reclassify", Project: "ops"})
	e.file(t, "s", record.Item{ID: "dk-mv000002", Kind: "issue", Title: "cert expiring tomorrow", Key: "cert", Project: "site"})
	a := active(t, st)
	if len(a) != 1 || a[0].Project != "ops" || a[0].Title != "cert expiring tomorrow" {
		t.Fatalf("a re-file after the human moved it made a duplicate: %+v", a)
	}
	// The agent sees it under ops now (context.json) and files there.
	e.file(t, "s", record.Item{ID: "dk-mv000003", Kind: "issue", Title: "cert expiring today", Key: "cert", Project: "ops"})
	if a := active(t, st); len(a) != 1 || a[0].Title != "cert expiring today" {
		t.Fatalf("a re-file under the new project made a duplicate: %+v", a)
	}
	e.send(t, record.Record{Event: "cli.done", SessionID: "s", Done: &record.Done{Key: "cert", Project: "ops", Explicit: true}}, ingest.Options{})
	if a := active(t, st); len(a) != 0 {
		t.Fatalf("done --key didn't reach the moved item: %+v", a)
	}
}

func TestMergedItemRedirectsItsAgent(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("ladr", "")
	st.EnsureProject("site", "")
	e.file(t, "a", record.Item{ID: "dk-mg000001", Kind: "ask", Title: "from ladr", Key: "dns", Project: "ladr"})
	e.file(t, "b", record.Item{ID: "dk-mg000002", Kind: "ask", Title: "from site", Key: "dns", Project: "site"})
	if err := st.RenameProject("ladr", "site"); err != nil {
		t.Fatal(err)
	}
	// The merged-away id now means the survivor, for done and add.
	if it, ok, _ := st.ResolveItem("dk-mg000001"); !ok || it.ID != "dk-mg000002" {
		t.Fatalf("resolve = %+v %v", it, ok)
	}
	// The survivor's own agent isn't told a stranger's id.
	e.s.Decide("dk-mg000002", Action{Kind: "approve"})
	ds, _ := st.ItemDeliveries("dk-mg000002")
	if len(ds) != 1 || strings.Contains(ds[0].Text, "dk-mg000001") {
		t.Fatalf("deliveries = %+v", ds)
	}
}

func TestUndoingAnExpiryKeepsTheAnswerGoing(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-ua000001", Kind: "ask", Title: "room?", SessionID: "s", Machine: "box", Source: "cli", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	e.s.Decide("dk-ua000001", Action{Kind: "answer", Text: "room 2"})
	st.DB.Exec(`UPDATE items SET expires_at=? WHERE id='dk-ua000001'`, time.Now().Add(-time.Minute).Unix())
	st.Expire(time.Now().Unix())
	e.s.Decide("dk-ua000001", Action{Kind: "undo"})
	if ds, _ := st.ItemDeliveries("dk-ua000001"); len(ds) != 1 {
		t.Fatalf("undoing the expiry dropped the answer's delivery: %+v", ds)
	}
}

func TestImageInsideALinkKeepsTheLink(t *testing.T) {
	out := string(renderMarkdown("[![logo](http://img.example/l.png)](https://link.example/)"))
	if !strings.Contains(out, `href="https://link.example/"`) || strings.Contains(out, "img.example") || strings.Count(out, "<a ") != 1 {
		t.Fatalf("rendered %s", out)
	}
}

func TestAMovedFilingLeavesTheOriginalMachinesList(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "mac1", record.Item{ID: "dk-mf000001", Kind: "ask", Title: "db?", Key: "db", Project: "w"})
	e.sendFrom(t, "lab", record.Record{Event: "cli.file", SessionID: "tl1", Item: &record.Item{ID: "dk-mf000002", Kind: "ask", Title: "db?", Key: "db", Project: "w"}})
	data, _ := e.s.ContextFor("box")
	var c agentctx.Context
	json.Unmarshal(data, &c)
	if !strings.Contains(strings.Join(c.Seen, ","), "dk-mf000001") || len(c.Open) != 0 {
		t.Fatalf("box still owns a filing another machine took over: seen=%v open=%+v", c.Seen, c.Open)
	}
}

func TestOneSignInItemWhicheverWayItsReported(t *testing.T) {
	e := newEnv(t, "")
	b1, _ := json.Marshal(map[string]any{"session_id": "s1", "error": "authentication_failed"})
	e.send(t, record.Record{Event: "StopFailure", SessionID: "s1", Payload: b1}, ingest.Options{})
	b2, _ := json.Marshal(map[string]any{"session_id": "s2", "notification_type": "auth_failed", "message": "signed out"})
	e.send(t, record.Record{Event: "Notification", SessionID: "s2", Payload: b2}, ingest.Options{})
	if a := active(t, e.s.st); len(a) != 1 {
		t.Fatalf("one sign-in problem made %d items", len(a))
	}
}

func TestLegacyKeyAdoptedBeforeTheMachineHasAContext(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-lk000001", Kind: "ask", Title: "Postgres or SQLite?", Source: "cli", Machine: "box", Cwd: "/w/api", DedupeKey: "key:api:db-choice"})
	// No context yet: the CLI sends no project, so the server files it unsorted.
	e.file(t, "s", record.Item{ID: "dk-lk000002", Kind: "ask", Title: "Postgres or SQLite?", Key: "db-choice", Root: "/w/api"})
	if a := active(t, st); len(a) != 1 || a[0].ID != "dk-lk000001" {
		t.Fatalf("a re-file before the first sync duplicated the old item: %+v", a)
	}
}

func TestReopenUndoesWhatTheCloseSetOff(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("live", store.SessionPatch{Machine: store.Str("box"), State: store.Str("idle"), HasPlugin: store.Bool(true)})
	// Expired: reopening keeps it open (the expiry is gone).
	st.UpsertItem(store.Item{ID: "dk-ro000001", Kind: "ask", Title: "launch?", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	st.Expire(time.Now().Unix())
	e.s.Decide("dk-ro000001", Action{Kind: "reopen"})
	e.s.chiefTick(e.ctx)
	if it, _, _ := st.GetItem("dk-ro000001"); it.Status != "open" || it.ExpiresAt != 0 {
		t.Fatalf("reopened expired item: %s expires=%d", it.Status, it.ExpiresAt)
	}
	// Not worth my time: the mark, its rules.md line and the suppression go too.
	e.file(t, "live", record.Item{ID: "dk-ro000002", Kind: "issue", Title: "Raise the spending limit?", Key: "lim", Project: "w"})
	e.s.Decide("dk-ro000002", Action{Kind: "noise", Text: "spending is mine"})
	e.s.Decide("dk-ro000002", Action{Kind: "reopen"})
	if strings.Contains(e.s.RulesMarkdown(), "spending is mine") {
		t.Fatal("rules.md still carries a mark that was taken back")
	}
	e.s.Decide("dk-ro000002", Action{Kind: "dismiss"})
	e.file(t, "live", record.Item{ID: "dk-ro000003", Kind: "issue", Title: "Raise the spending limit?", Key: "lim", Project: "w"})
	if a := active(t, st); len(a) != 2 {
		t.Fatalf("the taken-back mark still suppresses re-files: %+v", a)
	}
	// Done: the queued "marked done" note is withdrawn.
	st.UpsertItem(store.Item{ID: "dk-ro000004", Kind: "human_task", Title: "paste the signature", SessionID: "live", Machine: "box", Source: "cli"})
	e.s.Decide("dk-ro000004", Action{Kind: "done"})
	e.s.Decide("dk-ro000004", Action{Kind: "reopen"})
	if ds, _ := st.ItemDeliveries("dk-ro000004"); len(ds) != 0 {
		t.Fatalf("a reopened task still tells the agent it's done: %+v", ds)
	}
	// Closed by an agent (no decision): it simply comes back, without the note.
	st.UpsertItem(store.Item{ID: "dk-ro000005", Kind: "ask", Title: "x", Source: "cli"})
	st.SetStatus("dk-ro000005", "done", "resolved by the agent", "")
	e.s.Decide("dk-ro000005", Action{Kind: "reopen"})
	if it, _, _ := st.GetItem("dk-ro000005"); it.Status != "open" || it.Resolution != "" {
		t.Fatalf("agent-closed item after reopen: %s %q", it.Status, it.Resolution)
	}
}

func TestMovingToWhereItIsIsANoOp(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-nm000001", Kind: "ask", Title: "x"})
	if err := e.s.Decide("dk-nm000001", Action{Kind: "reclassify", Project: "unsorted"}); err != nil {
		t.Fatalf("moving an unsorted item to unsorted: %v", err)
	}
	e.s.st.UpsertItem(store.Item{ID: "dk-nm000002", Kind: "ask", Title: "y", Project: "w"})
	if err := e.s.Decide("dk-nm000002", Action{Kind: "reclassify", Project: "unsorted"}); err == nil {
		t.Fatal("moved a project's item to unsorted")
	}
	if strings.Contains(string(renderMarkdown("[![](https://ci.example/b.svg)](https://ci.example/run)")), "></a>") {
		t.Fatal("a linked image without alt text renders an empty link")
	}
}
