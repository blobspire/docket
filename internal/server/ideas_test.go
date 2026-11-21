package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/sessions"
	"github.com/blobspire/docket/internal/store"
)

// post sends a same-origin fetch to the web handler.
func post(e *env, path, ctype string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("X-Requested-With", "fetch")
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, req)
	return rec
}

func form(e *env, path string, kv ...string) *httptest.ResponseRecorder {
	v := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		v = append(v, kv[i]+"="+strings.ReplaceAll(kv[i+1], " ", "+"))
	}
	return post(e, path, "application/x-www-form-urlencoded", strings.NewReader(strings.Join(v, "&")))
}

func ideasEnv(t *testing.T, extra string) *env {
	return newEnv(t, `person = "Cole"
[transcribe]
url = "http://127.0.0.1:1/inference"
[[machine]]
name = "box"
mode = "local"
`+extra)
}

func TestDictationChunksBecomeOneWAV(t *testing.T) {
	e := ideasEnv(t, "")
	rec := form(e, "/dictation", "id", "dict-abcdefgh", "source", "voice", "auto_send", "1", "project", "docket")
	if rec.Code != 200 {
		t.Fatalf("new: %d %s", rec.Code, rec.Body)
	}
	// The same id again is the same dictation (a retry), not an error.
	if rec := form(e, "/dictation", "id", "dict-abcdefgh", "source", "voice"); rec.Code != 200 {
		t.Fatalf("retry of new: %d", rec.Code)
	}
	chunk := func(seq string, n int) *httptest.ResponseRecorder {
		return post(e, "/dictation/dict-abcdefgh/audio?seq="+seq, "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte{1, 0}, n/2)))
	}
	if r := chunk("0", 3200); r.Code != 200 {
		t.Fatalf("chunk 0: %d %s", r.Code, r.Body)
	}
	if r := chunk("0", 3200); r.Code != 200 || !strings.Contains(r.Body.String(), `"next":1`) {
		t.Fatalf("a repeated chunk is acknowledged and ignored: %d %s", r.Code, r.Body)
	}
	if r := chunk("2", 3200); r.Code != 409 || !strings.Contains(r.Body.String(), `"next":1`) {
		t.Fatalf("a gap is refused with the number expected: %d %s", r.Code, r.Body)
	}
	if r := post(e, "/dictation/dict-abcdefgh/audio?seq=1", "", strings.NewReader("abc")); r.Code != 400 {
		t.Fatalf("odd byte count: %d", r.Code)
	}
	if r := chunk("1", 1600); r.Code != 200 {
		t.Fatalf("chunk 1: %d", r.Code)
	}
	if r := post(e, "/dictation/dict-abcdefgh/audio?seq=2", "", bytes.NewReader(make([]byte, maxChunk+2))); r.Code != 413 {
		t.Fatalf("an oversized chunk: %d", r.Code)
	}
	cross := httptest.NewRequest("POST", "/dictation/dict-abcdefgh/audio?seq=2", strings.NewReader("ab"))
	cross.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, cross)
	if rec.Code != 403 {
		t.Fatalf("a cross-site upload must be refused: %d", rec.Code)
	}
	if r := form(e, "/dictation/dict-abcdefgh/finish", "auto_send", "1", "text", "typed first"); r.Code != 200 || !strings.Contains(r.Body.String(), "transcribing") {
		t.Fatalf("finish: %d %s", r.Code, r.Body)
	}
	d, _, _ := e.s.st.GetDictation("dict-abcdefgh")
	if d.Status != store.DictTranscribing || d.AudioMS != 150 || d.Text != "typed first" || d.ProjectHint != "docket" || !d.AutoSend {
		t.Fatalf("dictation = %+v", d)
	}
	wav, err := os.ReadFile(e.s.blobPath(d.AudioSHA))
	if err != nil {
		t.Fatal(err)
	}
	if len(wav) != 44+4800 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || binary.LittleEndian.Uint32(wav[40:]) != 4800 ||
		binary.LittleEndian.Uint32(wav[24:]) != 16000 || binary.LittleEndian.Uint32(wav[4:]) != 36+4800 {
		t.Fatalf("bad WAV: %d bytes, header %x", len(wav), wav[:44])
	}
	if parts, _ := filepath.Glob(filepath.Join(e.s.cfg.DataDir, "dictations", "*.pcm")); len(parts) != 0 {
		t.Fatalf("chunks left behind: %v", parts)
	}
	// Finishing again is harmless.
	if r := form(e, "/dictation/dict-abcdefgh/finish"); r.Code != 200 {
		t.Fatalf("second finish: %d", r.Code)
	}
}

func TestTypedIdeaSkipsTranscription(t *testing.T) {
	e := newEnv(t, "") // no [transcribe]: typing still works, recording doesn't
	if r := form(e, "/dictation", "source", "voice"); r.Code != 409 {
		t.Fatalf("recording without speech-to-text: %d", r.Code)
	}
	if r := form(e, "/dictation", "text", ""); r.Code != 400 {
		t.Fatalf("empty: %d", r.Code)
	}
	r := form(e, "/dictation", "id", "dict-typed123", "text", "add a dark mode toggle")
	if r.Code != 200 {
		t.Fatalf("typed: %d %s", r.Code, r.Body)
	}
	d, _, _ := e.s.st.GetDictation("dict-typed123")
	if d.Status != store.DictSent || d.Source != "text" {
		t.Fatalf("dictation = %+v", d)
	}
}

func TestTranscriptionUsesTheGlossary(t *testing.T) {
	var mu sync.Mutex
	var prompt, lang string
	fail := true
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		mu.Lock()
		defer mu.Unlock()
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "prompt":
				prompt = string(b)
			case "language":
				lang = string(b)
			}
		}
		if fail {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"model not loaded"}`))
			return
		}
		w.Write([]byte(`{"text":" [BLANK_AUDIO] Add a  kill switch\n to cwant. (music)"}`))
	}))
	defer whisper.Close()
	e := newEnv(t, `person = "Cole"
[transcribe]
url = "`+whisper.URL+`"
vocabulary = ["DAgger", "FR5"]
said_as = { cwant = "quant" }
[[machine]]
name = "box"
mode = "local"
`)
	e.s.st.EnsureProject("cwant", "trading")
	e.s.st.EnsureProject("vlm-teacher", "robots")
	form(e, "/dictation", "id", "dict-voice001", "source", "voice")
	post(e, "/dictation/dict-voice001/audio?seq=0", "", bytes.NewReader(make([]byte, 6400)))
	form(e, "/dictation/dict-voice001/finish", "auto_send", "1")

	for i := 0; i < transcribeAttempts; i++ {
		e.s.transcribeTick(e.ctx)
	}
	d, _, _ := e.s.st.GetDictation("dict-voice001")
	if d.Status != store.DictFailed || !strings.Contains(d.Error, "model not loaded") || d.AudioSHA == "" {
		t.Fatalf("after %d failures: %+v", transcribeAttempts, d)
	}
	if _, err := os.Stat(e.s.blobPath(d.AudioSHA)); err != nil {
		t.Fatal("a failed transcription must keep the audio")
	}
	mu.Lock()
	if !strings.Contains(prompt, "cwant (pronounced quant)") || strings.Count(prompt, "cwant") != 1 || !strings.Contains(prompt, "vlm-teacher") || !strings.Contains(prompt, "DAgger") || !strings.Contains(prompt, "Cole") || lang != "en" {
		t.Fatalf("glossary = %q, language %q", prompt, lang)
	}
	fail = false
	mu.Unlock()
	if r := form(e, "/dictation/dict-voice001/do/retry"); r.Code != 200 {
		t.Fatalf("retry: %d %s", r.Code, r.Body)
	}
	e.s.transcribeTick(e.ctx)
	d, _, _ = e.s.st.GetDictation("dict-voice001")
	if d.Status != store.DictSent || d.Text != "Add a kill switch to cwant." || d.Error != "" {
		t.Fatalf("transcribed: %+v", d)
	}
}

// fakeClaude answers the router with a fixed set of ideas.
func fakeClaude(t *testing.T, answer string) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	os.WriteFile(filepath.Join(dir, "answer.json"), []byte(answer), 0o600)
	os.WriteFile(filepath.Join(dir, "prompt.txt"), nil, 0o600)
	os.WriteFile(fake, []byte(`#!/bin/sh
for a in "$@"; do last="$a"; done
printf '%s' "$last" > "`+filepath.Join(dir, "prompt.txt")+`"
cat "`+filepath.Join(dir, "answer.json")+`"
`), 0o755)
	return fake
}

func TestRouterSplitsAndChecksIdeas(t *testing.T) {
	answer := `{"is_error":false,"total_cost_usd":0.01,"structured_output":{"ideas":[
	 {"project":"docket","title":"Swipe to stash","body":"Swipe left to stash.","route":"save","reason":"later"},
	 {"project":"nope","title":"Better charts","body":"Charts.","route":"research","reason":"unknown project"},
	 {"project":"docket","title":"Tell the old session","body":"Fix the typo.","route":"send","session_id":"gone-session","reason":"x"},
	 {"project":"docket","title":"Tell the live session","body":"Use the new endpoint.","route":"send","session_id":"live-session-1","reason":"x"},
	 {"project":"garden","new_project_description":"The garden robot","title":"Water the tomatoes","body":"Daily.","route":"build","reason":"new"}
	]}}`
	fake := fakeClaude(t, answer)
	e := newEnv(t, `person = "Cole"
[chief]
claude = "`+fake+`"
[[machine]]
name = "box"
mode = "local"
`)
	e.s.st.EnsureProject("docket", "the dashboard")
	e.s.st.PatchSession("live-session-1", store.SessionPatch{Machine: store.Str("box"), State: store.Str("busy"), HasPlugin: store.Bool(true), LastEventAt: store.Int(time.Now().Unix()), Cwd: store.Str("/x")})
	e.s.startFn = func(ctx context.Context, m Machine, prompt string, args []string) error { return nil }
	form(e, "/dictation", "id", "dict-route001", "text", "lots of ideas", "project", "docket")
	e.s.routeDictations(e.ctx)

	d, _, _ := e.s.st.GetDictation("dict-route001")
	if d.Status != store.DictRouted {
		t.Fatalf("dictation = %+v", d)
	}
	ideas, _ := e.s.st.IdeasOf(d.ID)
	if len(ideas) != 5 {
		t.Fatalf("%d ideas", len(ideas))
	}
	by := map[string]store.Idea{}
	for _, i := range ideas {
		by[i.Title] = i
	}
	if i := by["Swipe to stash"]; i.Status != store.IdeaSaved || i.Project != "docket" {
		t.Fatalf("save: %+v", i)
	}
	if i := by["Better charts"]; i.Project != "docket" || i.Status != store.IdeaQueued {
		t.Fatalf("an unknown project falls back to the hint: %+v", i)
	}
	if i := by["Tell the old session"]; i.Route != "research" || i.Status != store.IdeaQueued {
		t.Fatalf("a send to a session that isn't running is researched instead: %+v", i)
	}
	if i := by["Tell the live session"]; i.Status != store.IdeaSent || i.SessionID != "live-session-1" {
		t.Fatalf("send: %+v", i)
	}
	ds, _ := e.s.st.Deliveries("pending")
	if len(ds) != 1 || ds[0].SessionID != "live-session-1" || !strings.Contains(ds[0].Text, "Use the new endpoint.") || !strings.HasPrefix(ds[0].Text, "[docket #") {
		t.Fatalf("deliveries = %+v", ds)
	}
	if i := by["Water the tomatoes"]; i.Project != "garden" || i.Route != "build" || !e.s.projectExists("garden") {
		t.Fatalf("new project: %+v", i)
	}
	prompt, _ := os.ReadFile(filepath.Join(filepath.Dir(fake), "prompt.txt"))
	if !strings.Contains(string(prompt), "lots of ideas") || !strings.Contains(string(prompt), "live-session-1") || !strings.Contains(string(prompt), `"docket" project`) {
		t.Fatalf("router prompt lacks the dictation, the sessions or the hint:\n%s", prompt)
	}
}

func TestRouterFailureKeepsTheDictation(t *testing.T) {
	fake := fakeClaude(t, `{"is_error":true,"result":"overloaded"}`)
	e := newEnv(t, `person = "Cole"
[chief]
claude = "`+fake+`"
[[machine]]
name = "box"
mode = "local"
`)
	form(e, "/dictation", "id", "dict-fail0001", "text", "an idea")
	for i := 0; i < routeAttempts; i++ {
		e.s.routeDictations(e.ctx)
	}
	d, _, _ := e.s.st.GetDictation("dict-fail0001")
	if d.Status != store.DictFailed || !strings.Contains(d.Error, "overloaded") || d.Text != "an idea" {
		t.Fatalf("dictation = %+v", d)
	}
	if r := form(e, "/dictation/dict-fail0001/do/retry"); r.Code != 200 {
		t.Fatal(r.Body)
	}
	if d, _, _ := e.s.st.GetDictation("dict-fail0001"); d.Status != store.DictSent {
		t.Fatalf("retry: %+v", d)
	}
}

type started struct {
	machine, prompt string
	args            []string
}

func (e *env) captureStarts() *[]started {
	var got []started
	e.s.startFn = func(ctx context.Context, m Machine, prompt string, args []string) error {
		got = append(got, started{m.Name, prompt, args})
		return nil
	}
	return &got
}

func addIdea(t *testing.T, e *env, id, project, route string) {
	t.Helper()
	if err := e.s.st.AddIdea(store.Idea{ID: id, Project: project, Title: "An idea for " + project, Body: "Do the thing.", Route: route, Status: store.IdeaQueued}); err != nil {
		t.Fatal(err)
	}
}

func TestPlacementPrefersTheLeadThenAnAwakeLaptopThenAClone(t *testing.T) {
	cloneDir := t.TempDir()
	e := newEnv(t, `person = "Cole"
[ideas]
clone_dir = "`+cloneDir+`"
[[machine]]
name = "box"
mode = "local"
[[machine]]
name = "mac"
mode = "push"
[[lead]]
name = "site"
machine = "box"
dir = "/srv/site"
`)
	st := e.s.st
	got := e.captureStarts()
	// 1. A project with a lead on a machine docket reaches: there.
	st.EnsureProject("site", "")
	addIdea(t, e, "idea-aaaaaaa1", "site", "research")
	e.s.placeIdeas(e.ctx)
	if len(*got) != 1 || (*got)[0].machine != "box" || !strings.Contains(strings.Join((*got)[0].args, " "), "--cwd /srv/site --idea idea-aaaaaaa1 -- --permission-mode auto --model claude-opus-5-5") {
		t.Fatalf("starts = %+v", *got)
	}
	if !strings.Contains((*got)[0].prompt, "docket propose") || !strings.Contains((*got)[0].prompt, "--idea idea-aaaaaaa1") {
		t.Fatalf("research prompt: %s", (*got)[0].prompt)
	}
	if i, _, _ := st.GetIdea("idea-aaaaaaa1"); i.Status != store.IdeaStarting || i.Machine != "box" || i.SessionName != "idea-aaaaaaa1" {
		t.Fatalf("idea = %+v", i)
	}

	// 2. Only on the laptop, which is awake: a start waits for its sync.
	st.EnsureProject("docket", "")
	st.LearnDir("mac", "/Users/c/code/docket", "docket", true)
	st.SetDirRemote("mac", "/Users/c/code/docket", "https://github.com/acme/docket.git")
	st.MachineSeen("mac")
	addIdea(t, e, "idea-aaaaaaa2", "docket", "research")
	e.s.placeIdeas(e.ctx)
	reply, err := e.s.Receive("mac", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: "-", Context: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Starts) != 1 || reply.Starts[0].Cwd != "/Users/c/code/docket" || reply.Starts[0].Idea != "idea-aaaaaaa2" || reply.Starts[0].Prompt == "" {
		t.Fatalf("starts handed out = %+v", reply.Starts)
	}
	again, _ := e.s.Receive("mac", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: "-", Context: "-"})
	if len(again.Starts) != 0 {
		t.Fatal("a start is handed out once")
	}
	// The laptop couldn't start it: back to the queue, and that machine is skipped.
	e.sendFrom(t, "mac", record.Record{Event: "session.started", Payload: json.RawMessage(`{"start_id":"` + reply.Starts[0].ID + `","ok":false,"error":"tmux: no server"}`)})
	if i, _, _ := st.GetIdea("idea-aaaaaaa2"); i.Status != store.IdeaQueued || !strings.Contains(i.Error, "tmux: no server") {
		t.Fatalf("after a failed start: %+v", i)
	}

	// 3. The laptop is asleep (or failed): clone the repository on the server.
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Skipf("git: %v %s", err, out)
	}
	exec.Command("git", "-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init").Run()
	st.DB.Exec(`UPDATE project_dirs SET remote=? WHERE machine='mac'`, repo)
	*got = nil
	e.s.placeIdeas(e.ctx)
	if len(*got) != 1 {
		t.Fatalf("starts = %+v", *got)
	}
	want := filepath.Join(cloneDir, filepath.Base(repo))
	if !strings.Contains(strings.Join((*got)[0].args, " "), "--cwd "+want) || !strings.Contains((*got)[0].prompt, "docket's clone") {
		t.Fatalf("clone start = %+v", (*got)[0])
	}
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Fatal("not cloned")
	}
	homes, _ := st.ProjectHomes("docket")
	if len(homes) != 2 || homes[0].Clone || !homes[1].Clone || homes[1].Machine != "box" {
		t.Fatalf("homes = %+v", homes)
	}

	// 4. No remote and the laptop asleep: wait for it.
	st.EnsureProject("windward", "")
	st.LearnDir("mac", "/Users/c/code/windward", "windward", true)
	st.DB.Exec(`UPDATE machines SET last_seen_at=? WHERE name='mac'`, time.Now().Add(-time.Hour).Unix())
	addIdea(t, e, "idea-aaaaaaa3", "windward", "research")
	e.s.placeIdeas(e.ctx)
	if i, _, _ := st.GetIdea("idea-aaaaaaa3"); i.Status != store.IdeaWaiting {
		t.Fatalf("windward = %+v", i)
	}

	// 5. No folder anywhere: a scratch folder on the server.
	st.EnsureProject("garden", "")
	addIdea(t, e, "idea-aaaaaaa4", "garden", "build")
	*got = nil
	e.s.placeIdeas(e.ctx)
	if len(*got) != 1 || !strings.Contains(strings.Join((*got)[0].args, " "), filepath.Join("workspaces", "garden")) || !strings.Contains((*got)[0].prompt, "build it") {
		t.Fatalf("workspace start = %+v", *got)
	}
}

func TestMaxRunningQueuesTheRest(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[ideas]
max_running = 1
[[machine]]
name = "box"
mode = "local"
`)
	got := e.captureStarts()
	e.s.st.EnsureProject("a", "")
	addIdea(t, e, "idea-bbbbbbb1", "a", "research")
	addIdea(t, e, "idea-bbbbbbb2", "a", "research")
	e.s.placeIdeas(e.ctx)
	if len(*got) != 1 {
		t.Fatalf("started %d with max_running = 1", len(*got))
	}
	if i, _, _ := e.s.st.GetIdea("idea-bbbbbbb2"); i.Status != store.IdeaQueued {
		t.Fatalf("second = %+v", i)
	}
}

func TestIdeaFromStartToShipped(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[lead]]
name = "docket"
machine = "box"
dir = "/srv/docket"
match_dir = true
`)
	st := e.s.st
	e.captureStarts()
	st.EnsureProject("docket", "")
	addIdea(t, e, "idea-ccccccc1", "docket", "research")
	e.s.placeIdeas(e.ctx)

	// The session reports in: the idea is being researched, and the lead
	// working in the same folder doesn't claim it.
	sid := "idea-session-1"
	e.send(t, record.Record{Event: "SessionStart", SessionID: sid, Cwd: "/srv/docket", Env: map[string]string{"DOCKET_IDEA": "idea-ccccccc1"}}, ingest.Options{})
	i, _, _ := st.GetIdea("idea-ccccccc1")
	if i.Status != store.IdeaResearching || i.SessionID != sid {
		t.Fatalf("after SessionStart: %+v", i)
	}
	sess, _, _ := st.GetSession(sid)
	if l, _ := e.s.cfg.Lead("docket"); sess.Idea != "idea-ccccccc1" || l.owns(sess) {
		t.Fatalf("session = %+v (a lead must not own an idea's session)", sess)
	}

	// Its proposal: an ask with the two options, linked to the idea.
	e.file(t, sid, record.Item{ID: "dk-prop0001", Kind: "ask", Title: "Add swipe to stash", Options: []string{ProposeBuild, ProposeSave}, Recommend: ProposeBuild, Key: "idea-ccccccc1", Project: "docket", Idea: "idea-ccccccc1"})
	i, _, _ = st.GetIdea("idea-ccccccc1")
	if i.Status != store.IdeaProposed || i.ItemID != "dk-prop0001" {
		t.Fatalf("after propose: %+v", i)
	}
	page := get(e, "/")
	if !strings.Contains(page, "k-idea") {
		t.Fatal("the proposal's row should be marked as an idea")
	}

	// Build it: the answer goes to the session, and the idea is being built.
	if err := e.s.Decide("dk-prop0001", Action{Kind: "choose", Option: 0}); err != nil {
		t.Fatal(err)
	}
	if i, _, _ = st.GetIdea("idea-ccccccc1"); i.Status != store.IdeaBuilding {
		t.Fatalf("after Build it: %+v", i)
	}
	ds, _ := st.ItemDeliveries("dk-prop0001")
	if len(ds) == 0 || !strings.Contains(ds[0].Text, "Chose: Build it") {
		t.Fatalf("deliveries = %+v", ds)
	}

	// Shipped: the idea is built and the proposal closes.
	e.send(t, record.Record{Event: "cli.idea", SessionID: sid, Payload: json.RawMessage(`{"op":"done","id":"idea-ccccccc1","why":"merged #12"}`)}, ingest.Options{})
	i, _, _ = st.GetIdea("idea-ccccccc1")
	if i.Status != store.IdeaBuilt || i.Note != "merged #12" || i.EndedAt == 0 {
		t.Fatalf("after done: %+v", i)
	}
	if it, _, _ := st.GetItem("dk-prop0001"); !closedStatus(it.Status) {
		t.Fatalf("proposal still %s", it.Status)
	}
	if body := get(e, "/ideas"); !strings.Contains(body, "Shipped") || !strings.Contains(body, "merged #12") {
		t.Fatal("the Ideas page should show it shipped")
	}
}

func TestProposalDecisionsMoveTheIdea(t *testing.T) {
	for _, c := range []struct {
		act  Action
		want string
	}{
		{Action{Kind: "choose", Option: 1}, store.IdeaSaved},
		{Action{Kind: "dismiss"}, store.IdeaDismissed},
		{Action{Kind: "noise", Text: "meh"}, store.IdeaDismissed},
		{Action{Kind: "answer", Text: "what about tablets?"}, store.IdeaProposed},
	} {
		e := newEnv(t, "")
		st := e.s.st
		st.EnsureProject("docket", "")
		st.AddIdea(store.Idea{ID: "idea-ddddddd1", Project: "docket", Title: "x", Route: "research", Status: store.IdeaResearching, SessionID: "s1"})
		e.file(t, "s1", record.Item{ID: "dk-prop0002", Kind: "ask", Title: "Plan", Options: []string{ProposeBuild, ProposeSave}, Project: "docket", Idea: "idea-ddddddd1", Key: "idea-ddddddd1"})
		if err := e.s.Decide("dk-prop0002", c.act); err != nil {
			t.Fatal(err)
		}
		if i, _, _ := st.GetIdea("idea-ddddddd1"); i.Status != c.want {
			t.Fatalf("%s: idea %s, want %s", c.act.Kind, i.Status, c.want)
		}
	}
}

func TestStuckStartsAreNoticed(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[machine]]
name = "mac"
mode = "push"
`)
	st := e.s.st
	old := time.Now().Add(-time.Hour).Unix()
	st.AddIdea(store.Idea{ID: "idea-eeeeeee1", Project: "p", Title: "local", Status: store.IdeaStarting, Machine: "box", SessionName: "idea-eeeeeee1", StartedAt: old})
	st.AddIdea(store.Idea{ID: "idea-eeeeeee2", Project: "p", Title: "laptop", Status: store.IdeaStarting, Machine: "mac", SessionName: "idea-eeeeeee2", StartedAt: old})
	st.AddStart(store.Start{ID: "start-1", Machine: "mac", Name: "idea-eeeeeee2", IdeaID: "idea-eeeeeee2"})
	e.s.watchIdeas(e.ctx)
	if i, _, _ := st.GetIdea("idea-eeeeeee1"); i.Status != store.IdeaFailed || !strings.Contains(i.Error, "didn't report in") {
		t.Fatalf("local = %+v", i)
	}
	if i, _, _ := st.GetIdea("idea-eeeeeee2"); i.Status != store.IdeaQueued {
		t.Fatalf("laptop = %+v", i)
	}
	if ss, _ := st.StartsForIdea("idea-eeeeeee2"); len(ss) != 1 || ss[0].Status != "failed" {
		t.Fatalf("start = %+v", ss)
	}
}

func TestStaleRecordingIsFinished(t *testing.T) {
	e := ideasEnv(t, "")
	form(e, "/dictation", "id", "dict-stale001", "source", "voice", "auto_send", "1")
	post(e, "/dictation/dict-stale001/audio?seq=0", "", bytes.NewReader(make([]byte, 3200)))
	e.s.st.DB.Exec(`UPDATE dictations SET updated_at=? WHERE id='dict-stale001'`, time.Now().Add(-time.Hour).Unix())
	e.s.finishStale()
	d, _, _ := e.s.st.GetDictation("dict-stale001")
	if d.Status != store.DictTranscribing || d.AudioSHA == "" || !strings.Contains(d.Error, "stopped arriving") {
		t.Fatalf("stale = %+v", d)
	}
}

func TestIdeasPageAndActions(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("docket", "")
	st.AddIdea(store.Idea{ID: "idea-fffffff1", Project: "docket", Title: "Saved one", Body: "**bold**", Status: store.IdeaSaved})
	st.AddDictation(store.Dictation{ID: "dict-draft001", Source: "voice", Status: store.DictDraft, Text: "my draft words", Transcript: "my draft words"})
	body := get(e, "/ideas")
	for _, want := range []string{"Saved one", "<strong>bold</strong>", "my draft words", "Needs you"} {
		if !strings.Contains(body, want) {
			t.Fatalf("Ideas page lacks %q", want)
		}
	}
	if !strings.Contains(get(e, "/"), "pill-i") {
		t.Fatal("the nav should count the draft")
	}
	if r := form(e, "/idea/idea-fffffff1/research"); r.Code != 200 {
		t.Fatalf("research: %d %s", r.Code, r.Body)
	}
	if i, _, _ := st.GetIdea("idea-fffffff1"); i.Status != store.IdeaQueued || i.Route != "research" {
		t.Fatalf("idea = %+v", i)
	}
	if r := form(e, "/dictation/dict-draft001/do/send", "text", "my fixed words"); r.Code != 200 {
		t.Fatalf("send: %d %s", r.Code, r.Body)
	}
	if d, _, _ := st.GetDictation("dict-draft001"); d.Status != store.DictSent || d.Text != "my fixed words" {
		t.Fatalf("draft = %+v", d)
	}
	// Saved ideas reach every machine's context, for `docket ideas`.
	st.AddIdea(store.Idea{ID: "idea-fffffff2", Project: "docket", Title: "Another", Status: store.IdeaSaved})
	data, _ := e.s.ContextFor("box")
	if !strings.Contains(string(data), "idea-fffffff2") {
		t.Fatalf("context = %s", data)
	}
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ideas", nil))
	if pp := rec.Header().Get("Permissions-Policy"); !strings.Contains(pp, "microphone=(self)") {
		t.Fatalf("Permissions-Policy = %q", pp)
	}
}

func TestSessionScriptExportsTheIdea(t *testing.T) {
	sc := sessions.Script(sessions.StartOpts{Name: "idea-abcdefgh", Cwd: "/srv/x", Idea: "idea-abcdefgh", Args: []string{"--model", "claude-opus-5-5"}}, "/bin/claude", "/run/p", "/run/e")
	if !strings.Contains(sc, "export DOCKET_IDEA='idea-abcdefgh'") || !strings.Contains(sc, "--remote-control 'idea-abcdefgh'") {
		t.Fatalf("script:\n%s", sc)
	}
	if err := sessions.Stop("lead-robotics"); err == nil {
		t.Fatal("docket must only stop sessions it started for ideas")
	}
}

func TestCloneableRemotes(t *testing.T) {
	for r, ok := range map[string]bool{
		"https://github.com/a/b.git": true, "ssh://git@github.com/a/b": true, "git@github.com:a/b.git": true, "/srv/repo": true,
		"ext::sh -c touch% /tmp/x": false, "-uhoh": false, "file:///etc": false, "git@x:a::b": false,
	} {
		if cloneable(r) != ok {
			t.Errorf("cloneable(%q) = %v", r, !ok)
		}
	}
	if !sameRemote("https://github.com/A/b.git", "git@github.com:a/b") {
		t.Error("https and scp forms of one repo should match")
	}
}
