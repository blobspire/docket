package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/cli"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
)

type env struct {
	t   *testing.T
	s   *Server
	ctx context.Context
	m   Machine
}

func newEnv(t *testing.T, cfgText string) *env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DOCKET_HOME", filepath.Join(home, "agent"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude")) // no CLAUDE.md block, empty registry
	path := filepath.Join(home, "docket.toml")
	if cfgText == "" {
		cfgText = `person = "Cole"
[[machine]]
name = "box"
mode = "local"
`
	}
	os.WriteFile(path, []byte("data_dir = \""+filepath.Join(home, "data")+"\"\n"+cfgText), 0o600)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m, _ := cfg.Machine("box")
	return &env{t: t, s: s, ctx: context.Background(), m: m}
}

func getenv(kv map[string]string) func(string) string {
	return func(k string) string {
		if v, ok := kv[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
}

// cycle pulls the local spool and applies everything, like the loops do.
func (e *env) cycle() {
	e.t.Helper()
	if err := e.s.pull(e.ctx, e.m); err != nil {
		e.t.Fatal(err)
	}
	if _, err := ingest.Process(e.s.st, 100); err != nil {
		e.t.Fatal(err)
	}
	e.s.deliverAll(e.ctx)
}

func TestAskAnswerDeliverReceipt(t *testing.T) {
	e := newEnv(t, "")
	agent := getenv(map[string]string{"DOCKET_SESSION": "sess-1", "DOCKET_LEAD": "site"})
	hook.Run("SessionStart", strings.NewReader(`{"session_id":"sess-1","cwd":"/w/site","source":"startup"}`), io.Discard, io.Discard, agent)
	var out bytes.Buffer
	if err := cli.File("ask", []string{"Ship the redesign today?", "--options", "ship|wait", "--blocking"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	hook.Run("Stop", strings.NewReader(`{"session_id":"sess-1","last_assistant_message":"Filed the question."}`), io.Discard, io.Discard, agent)
	e.cycle()

	now, _, _, _ := e.s.Queue()
	if len(now) != 1 || now[0].Title != "Ship the redesign today?" || now[0].Session.State != "idle" {
		t.Fatalf("queue = %+v", now)
	}
	id := now[0].ID
	if err := e.s.Decide(id, Action{Kind: "choose", Option: 0, Text: "nice work"}); err != nil {
		t.Fatal(err)
	}
	e.s.deliverAll(e.ctx) // local machine: answer lands in the mailbox

	// The agent's next tool call picks the answer up.
	var hookOut bytes.Buffer
	hook.Run("PostToolUse", strings.NewReader(`{"session_id":"sess-1"}`), &hookOut, io.Discard, agent)
	if !strings.Contains(hookOut.String(), "Cole's answer to") || !strings.Contains(hookOut.String(), "Chose: ship. nice work") {
		t.Fatalf("delivered %q", hookOut.String())
	}
	e.cycle() // the receipt comes back
	it, _, _ := e.s.st.GetItem(id)
	if it.Status != "delivered" {
		t.Fatalf("status = %s", it.Status)
	}
	ds, _ := e.s.st.ItemDeliveries(id)
	if len(ds) != 1 || ds[0].Status != "confirmed" {
		t.Fatalf("deliveries = %+v", ds)
	}
}

func TestPhysicalItemsCannotBeApproved(t *testing.T) {
	e := newEnv(t, `[[machine]]
name = "box"
mode = "local"
physical_keywords = ["robot", "arm"]
`)
	e.s.st.UpsertItem(store.Item{ID: "dk-aaaaaaaa", Kind: "approval", Title: "Move the robot arm to home", Machine: "box", SessionID: "s"})
	if err := e.s.Decide("dk-aaaaaaaa", Action{Kind: "approve"}); err == nil {
		t.Fatal("approved a physical action remotely")
	}
	e.s.st.UpsertItem(store.Item{ID: "dk-ffffffff", Kind: "approval", Title: "Silence the disk alarm and fix the harmless warning", Machine: "box", SessionID: "s"})
	if err := e.s.Decide("dk-ffffffff", Action{Kind: "approve"}); err != nil {
		t.Fatalf("'alarm'/'harmless' were mistaken for 'arm': %v", err)
	}
	e.s.st.UpsertItem(store.Item{ID: "dk-bbbbbbbb", Kind: "ask", Title: "Skip the 34 cells?", Machine: "box", SessionID: "s", Options: []string{"skip", "keep"}})
	if err := e.s.Decide("dk-bbbbbbbb", Action{Kind: "choose", Option: 0}); err != nil {
		t.Fatalf("an ordinary decision on the same machine was refused: %v", err)
	}
	ds, _ := e.s.st.ItemDeliveries("dk-bbbbbbbb")
	if len(ds) != 1 || !strings.Contains(ds[0].Text, "does not authorize any physical or robot motion") {
		t.Fatalf("delivery = %+v", ds)
	}
}

func TestHumanTaskDoneTellsTheAgent(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-cccccccc", Kind: "human_task", Title: "Insert the SSD", Machine: "box", SessionID: "s"})
	e.s.Decide("dk-cccccccc", Action{Kind: "done"})
	ds, _ := e.s.st.ItemDeliveries("dk-cccccccc")
	if len(ds) != 1 || !strings.Contains(ds[0].Text, "marked this done") {
		t.Fatalf("deliveries = %+v", ds)
	}
}

func TestPushRules(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer ntfy.Close()
	e := newEnv(t, `public_url = "https://docket.example"
[ntfy]
server = "`+ntfy.URL+`"
topic = "t"
quiet_start = "00:00"
quiet_end = "00:00"
[[machine]]
name = "box"
mode = "local"
`)
	old := time.Now().Add(-10 * time.Minute).Unix()
	st := e.s.st
	st.PatchSession("idle", store.SessionPatch{State: store.Str("idle")})
	st.PatchSession("busy", store.SessionPatch{State: store.Str("busy")})
	st.UpsertItem(store.Item{ID: "dk-00000001", Kind: "approval", Title: "rm -rf build", SessionID: "idle", CreatedAt: old})
	st.UpsertItem(store.Item{ID: "dk-00000002", Kind: "approval", Title: "still working", SessionID: "busy", CreatedAt: old})
	st.UpsertItem(store.Item{ID: "dk-00000003", Kind: "review", Title: "look at the hero", SessionID: "idle", CreatedAt: old})
	st.UpsertItem(store.Item{ID: "dk-00000004", Kind: "approval", Title: "too fresh", SessionID: "idle"})
	e.s.pushes(e.ctx)
	mu.Lock()
	if len(got) != 1 || got[0]["title"] != "An agent needs your approval" || got[0]["click"] != "https://docket.example/item/dk-00000001" {
		t.Fatalf("pushes = %+v", got)
	}
	mu.Unlock()
	e.s.pushes(e.ctx) // never twice for the same item
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("pushed again: %+v", got)
	}
}

func TestWebQueueAndCrossSiteGuard(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-dddddddd", Kind: "issue", Title: "Disk is 95% full", Machine: "box"})
	h := e.s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Disk is 95% full") {
		t.Fatalf("GET / = %d\n%s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/item/dk-dddddddd", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Disk is 95% full") {
		t.Fatalf("GET /item = %d", rec.Code)
	}
	for _, p := range []string{"/health", "/done", "/static/app.js", "/api/version", "/search?q=disk", "/evidence", "/checklist"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
	}

	form := url.Values{"action": {"done"}}.Encode()
	req := httptest.NewRequest("POST", "/item/dk-dddddddd/act", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST allowed: %d", rec.Code)
	}
	req = httptest.NewRequest("POST", "/item/dk-dddddddd/act", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("same-site POST = %d %s", rec.Code, rec.Body.String())
	}
	it, _, _ := e.s.st.GetItem("dk-dddddddd")
	if it.Status != "done" {
		t.Fatalf("status = %s", it.Status)
	}
}

func TestLoginAllowlist(t *testing.T) {
	e := newEnv(t, `allow_logins = ["me@example.com"]
[[machine]]
name = "box"
mode = "local"
`)
	h := e.s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no login header: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Tailscale-User-Login", "me@example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("allowed login: %d", rec.Code)
	}
}

func TestReceiveFromPushMachine(t *testing.T) {
	e := newEnv(t, `[[machine]]
name = "laptop"
mode = "push"
`)
	e.s.st.PatchSession("s1", store.SessionPatch{Machine: store.Str("laptop"), HasPlugin: store.Bool(true), State: store.Str("idle")})
	e.s.st.UpsertItem(store.Item{ID: "dk-eeeeeeee", Kind: "ask", Title: "q", Machine: "laptop", SessionID: "s1"})
	e.s.Decide("dk-eeeeeeee", Action{Kind: "answer", Text: "yes"})
	export := `{"id":"01AAA","v":1,"event":"Stop","session_id":"s1","machine":"laptop","payload":{"session_id":"s1"}}` + "\n" + `{"_cursor":"seg-2026110900.jsonl:120","_more":false}` + "\n"
	reply, err := e.s.Receive("laptop", strings.NewReader(export), []byte(`[{"session_id":"s1","status":"idle","pid":1}]`))
	if err != nil {
		t.Fatal(err)
	}
	if reply.Cursor != "seg-2026110900.jsonl:120" || len(reply.Deliveries) != 1 || !strings.Contains(reply.Deliveries[0].Text, "yes") {
		t.Fatalf("reply = %+v", reply)
	}
	reply, _ = e.s.Receive("laptop", strings.NewReader(`{"_cursor":"seg-2026110900.jsonl:120","_more":false}`+"\n"), nil)
	if len(reply.Deliveries) != 0 {
		t.Fatal("handed out the same delivery twice")
	}
	if _, err := e.s.Receive("nope", strings.NewReader(""), nil); err == nil {
		t.Fatal("accepted an unconfigured machine")
	}
}
