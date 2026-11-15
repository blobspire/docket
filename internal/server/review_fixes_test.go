package server

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	if _, err := e.s.st.IngestBatch("box", []store.RawEvent{{ID: r.ID, Event: r.Event, SessionID: r.SessionID, Body: b}}, ""); err != nil {
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
	for _, status := range []string{"snoozed", "answered"} {
		id := "dk-esc" + status[:3] + "1"
		e.file(t, "s1", record.Item{ID: id, Kind: "ask", Title: "Launch before Friday? " + status, Key: "launch-" + status, Project: "windward"})
		if status == "snoozed" {
			st.Snooze(id, time.Now().Add(24*time.Hour).Unix())
		} else {
			st.SetAnswer(id, "later", "answer")
		}
		e.file(t, "s1", record.Item{ID: id + "x", Kind: "ask", Title: "Launch before Friday? " + status, Key: "launch-" + status, Project: "windward", Urgency: "now"})
		it, _, _ := st.GetItem(id)
		if it.Status != "open" || it.Urgency != "now" {
			t.Fatalf("%s item after escalation: status=%s urgency=%s", status, it.Status, it.Urgency)
		}
		st.DB.Exec(`UPDATE items SET urgent_since=urgent_since-120 WHERE id=?`, id)
	}
	e.s.pushes(e.ctx)
	if pushes() != 1 {
		t.Fatalf("escalated items should push once (batched): %d pushes", pushes())
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
	// Its page offers no actions, only the way back.
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/item/dk-c1000001", nil))
	body := rec.Body.String()
	if strings.Contains(body, `data-act="snooze"`) || strings.Contains(body, `class="answer-form"`) || !strings.Contains(body, "Undo it from the Done page") {
		t.Fatal("a closed item's page still offers actions")
	}
	// Undo reopens it without the stale closing note.
	e.s.Decide("dk-c1000001", Action{Kind: "undo"})
	if it, _, _ := st.GetItem("dk-c1000001"); it.Status != "open" || it.Resolution != "" {
		t.Fatalf("after undo: %s %q", it.Status, it.Resolution)
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
	st.UpsertItem(store.Item{ID: "dk-u0000003", Kind: "ask", Title: "Real ask", Source: "cli", Machine: "box", DedupeKey: "key:mac-repo:arm-plug"})
	st.DB.Exec(`DELETE FROM kv WHERE k='upgrade:agents-file'`)
	upgraded, err := store.Open(st.Path())
	if err != nil {
		t.Fatal(err)
	}
	upgraded.Close()
	a := active(t, st)
	if len(a) != 1 || a[0].ID != "dk-u0000003" {
		t.Fatalf("after the upgrade: %+v", a)
	}
	// The first post-upgrade re-file of the old key updates it, not a duplicate.
	e.file(t, "s", record.Item{ID: "dk-u0000004", Kind: "ask", Title: "Real ask, still", Key: "arm-plug", Project: "docket"})
	if a := active(t, st); len(a) != 1 || a[0].ID != "dk-u0000003" || a[0].Project != "docket" || a[0].Title != "Real ask, still" {
		t.Fatalf("after the re-file: %+v", a)
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
