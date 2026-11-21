package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// Review round 1: each test pins one root cause the reviewers found.

func TestLateAudioIsRefusedNotDropped(t *testing.T) {
	e := ideasEnv(t, "")
	form(e, "/dictation", "id", "dict-late0001", "source", "voice")
	post(e, "/dictation/dict-late0001/audio?seq=0", "", bytes.NewReader(make([]byte, 3200)))
	form(e, "/dictation/dict-late0001/finish") // finished (by the server's stale check, another tab…)
	if r := post(e, "/dictation/dict-late0001/audio?seq=0", "", bytes.NewReader(make([]byte, 3200))); r.Code != 200 {
		t.Fatalf("a chunk it already has is still a harmless retry: %d", r.Code)
	}
	if r := post(e, "/dictation/dict-late0001/audio?seq=1", "", bytes.NewReader(make([]byte, 3200))); r.Code != http.StatusGone {
		t.Fatalf("new audio for a finished recording must be refused (410) so the page keeps it: %d %s", r.Code, r.Body)
	}
	if r := form(e, "/dictation/dict-late0001/finish", "text", "words it never got"); r.Code != http.StatusGone {
		t.Fatalf("typed words a finished recording lacks must be refused: %d", r.Code)
	}
	if r := form(e, "/dictation/dict-late0001/finish"); r.Code != 200 {
		t.Fatalf("finishing again with nothing new is harmless: %d", r.Code)
	}
}

func TestChunkRacingFinishIsNeverLost(t *testing.T) {
	e := ideasEnv(t, "")
	for trial := 0; trial < 20; trial++ {
		id := "dict-race" + string(rune('a'+trial)) + "aaa"
		form(e, "/dictation", "id", id, "source", "voice")
		post(e, "/dictation/"+id+"/audio?seq=0", "", bytes.NewReader(make([]byte, 6400)))
		var wg sync.WaitGroup
		var code int
		wg.Add(2)
		go func() {
			defer wg.Done()
			code = post(e, "/dictation/"+id+"/audio?seq=1", "", bytes.NewReader(make([]byte, 6400))).Code
		}()
		go func() { defer wg.Done(); form(e, "/dictation/"+id+"/finish") }()
		wg.Wait()
		d, _, _ := e.s.st.GetDictation(id)
		wav, _ := os.ReadFile(e.s.blobPath(d.AudioSHA))
		want := 44 + 6400
		if code == 200 {
			want += 6400 // acknowledged: it must be in the recording
		}
		if len(wav) != want {
			t.Fatalf("trial %d: chunk answered %d but the WAV has %d bytes (want %d)", trial, code, len(wav), want)
		}
	}
	if parts, _ := filepath.Glob(filepath.Join(e.s.cfg.DataDir, "dictations", "*.pcm")); len(parts) != 0 {
		t.Fatalf("part files left behind: %v", parts)
	}
}

func TestWAVHeaderIsExactly16kMono16bit(t *testing.T) {
	h := wavHeader(1000)
	want := []byte("RIFF")
	want = binary.LittleEndian.AppendUint32(want, 1036)
	want = append(want, "WAVEfmt "...)
	want = binary.LittleEndian.AppendUint32(want, 16)
	want = binary.LittleEndian.AppendUint16(want, 1)     // PCM
	want = binary.LittleEndian.AppendUint16(want, 1)     // mono
	want = binary.LittleEndian.AppendUint32(want, 16000) // rate
	want = binary.LittleEndian.AppendUint32(want, 32000) // bytes per second
	want = binary.LittleEndian.AppendUint16(want, 2)     // block align
	want = binary.LittleEndian.AppendUint16(want, 16)    // bits
	want = append(want, "data"...)
	want = binary.LittleEndian.AppendUint32(want, 1000)
	if !bytes.Equal(h, want) {
		t.Fatalf("header\n got %x\nwant %x", h, want)
	}
}

func TestRecordingEdgeCases(t *testing.T) {
	e := ideasEnv(t, "")
	st := e.s.st
	// A live recording is left alone; only a silent one is finished.
	form(e, "/dictation", "id", "dict-fresh001", "source", "voice")
	post(e, "/dictation/dict-fresh001/audio?seq=0", "", bytes.NewReader(make([]byte, 3200)))
	e.s.finishStale()
	if d, _, _ := st.GetDictation("dict-fresh001"); d.Status != store.DictRecording {
		t.Fatalf("a recording still arriving was finished: %+v", d)
	}
	// Typed words and no audio: sent as text, not dropped.
	form(e, "/dictation", "id", "dict-typed002", "source", "voice")
	form(e, "/dictation/dict-typed002/finish", "text", "only typed")
	if d, _, _ := st.GetDictation("dict-typed002"); d.Status != store.DictSent || d.Text != "only typed" {
		t.Fatalf("typed-only = %+v", d)
	}
}

func TestReviewBeforeSendAndEmptyTranscripts(t *testing.T) {
	text := `{"text":"Fix the login page."}`
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(text)) }))
	defer whisper.Close()
	e := newEnv(t, "person = \"Cole\"\n[transcribe]\nurl = \""+whisper.URL+"\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n")
	form(e, "/dictation", "id", "dict-review01", "source", "voice", "auto_send", "0")
	post(e, "/dictation/dict-review01/audio?seq=0", "", bytes.NewReader(make([]byte, 3200)))
	form(e, "/dictation/dict-review01/finish", "auto_send", "0")
	e.s.transcribeTick(e.ctx)
	if d, _, _ := e.s.st.GetDictation("dict-review01"); d.Status != store.DictDraft || d.Text != "Fix the login page." {
		t.Fatalf("auto_send off must leave a draft to review: %+v", d)
	}
	text = `{"text":" [BLANK_AUDIO] "}`
	form(e, "/dictation", "id", "dict-silent01", "source", "voice")
	post(e, "/dictation/dict-silent01/audio?seq=0", "", bytes.NewReader(make([]byte, 3200)))
	form(e, "/dictation/dict-silent01/finish")
	e.s.transcribeTick(e.ctx) // one try is enough: retrying won't find speech
	if d, _, _ := e.s.st.GetDictation("dict-silent01"); d.Status != store.DictFailed || !strings.Contains(d.Error, "no speech") {
		t.Fatalf("an empty transcript must fail as no speech, not reach the router: %+v", d)
	}
}

func TestRouterEdgeCases(t *testing.T) {
	fake := fakeClaude(t, `{"is_error":false,"structured_output":{"ideas":[]}}`)
	e := newEnv(t, "person = \"Cole\"\n[chief]\nclaude = \""+fake+"\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n")
	form(e, "/dictation", "id", "dict-none0001", "text", "hmm")
	e.s.routeDictations(e.ctx)
	if d, _, _ := e.s.st.GetDictation("dict-none0001"); d.Status != store.DictSent || d.Attempts != 1 {
		t.Fatalf("no ideas back: one failed try, kept for another: %+v", d)
	}
	e.s.routeDictations(e.ctx)
	if d, _, _ := e.s.st.GetDictation("dict-none0001"); d.Status != store.DictFailed {
		t.Fatalf("after 2 tries it fails visibly: %+v", d)
	}
	// A send to an ended session, or one without the plugin, is researched instead.
	e.s.st.PatchSession("ended-1", store.SessionPatch{State: store.Str("ended"), HasPlugin: store.Bool(true)})
	e.s.st.PatchSession("noplug-1", store.SessionPatch{State: store.Str("idle"), HasPlugin: store.Bool(false)})
	d := store.Dictation{ID: "dict-x"}
	e.s.addRoutedIdea(d, "", "", "To ended", "b", "send", "ended-1", "r")
	e.s.addRoutedIdea(d, "", "", "To plugless", "b", "send", "noplug-1", "r")
	ideas, _ := e.s.st.IdeasOf("dict-x")
	for _, i := range ideas {
		if i.Route != "research" || i.Status != store.IdeaQueued {
			t.Fatalf("%q: %+v", i.Title, i)
		}
	}
}

func TestAFailedLaptopStartFailsVisiblyAndRetryGoesBack(t *testing.T) {
	e := newEnv(t, "person = \"Cole\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n[[machine]]\nname = \"mac\"\nmode = \"push\"\n")
	st := e.s.st
	st.EnsureProject("windward", "")
	st.LearnDir("mac", "/Users/c/windward", "windward", true) // no remote
	st.MachineSeen("mac")
	addIdea(t, e, "idea-push0001", "windward", "research")
	e.s.placeIdeas(e.ctx)
	starts, _ := st.Starts("mac", "pending")
	st.StartResult(starts[0].ID, false, "tmux: not found")
	st.EditIdea("idea-push0001", func(x *store.Idea) error { x.Status = store.IdeaQueued; return nil })
	st.MachineSeen("mac")
	e.s.placeIdeas(e.ctx)
	i, _, _ := st.GetIdea("idea-push0001")
	if i.Status != store.IdeaFailed || !strings.Contains(i.Error, "tmux: not found") {
		t.Fatalf("its only home failed and there's nothing to clone: it must fail visibly, not wait forever: %+v", i)
	}
	if err := e.s.doIdeaAct(e.ctx, i, "retry"); err != nil {
		t.Fatal(err)
	}
	e.s.placeIdeas(e.ctx)
	var starts2, stops []string
	pend, _ := st.Starts("mac", "pending")
	for _, p := range pend {
		if p.Op == "stop" {
			stops = append(stops, p.Name)
		} else {
			starts2 = append(starts2, p.Name)
		}
	}
	if len(starts2) != 1 || starts2[0] != "idea-push0001-2" || len(stops) != 1 || stops[0] != "idea-push0001-1" {
		t.Fatalf("Try again must go back to the laptop with a fresh session name (and stop the old one): starts %v stops %v", starts2, stops)
	}
}

func TestDismissWinsOverPlacement(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("a", "")
	addIdea(t, e, "idea-dism0001", "a", "research")
	var dismissErr error
	e.s.startFn = func(ctx context.Context, m Machine, prompt string, args []string) error {
		i, _, _ := st.GetIdea("idea-dism0001")
		dismissErr = e.s.doIdeaAct(ctx, i, "dismiss") // the human clicks while it starts
		return nil
	}
	e.s.placeIdeas(e.ctx)
	if dismissErr == nil {
		t.Fatal("a starting idea can't be dismissed from the page (its proposal can)")
	}
	// A dismiss that lands before placement claims it wins: nothing starts.
	addIdea(t, e, "idea-dism0002", "a", "research")
	i, _, _ := st.GetIdea("idea-dism0002")
	e.s.doIdeaAct(e.ctx, i, "dismiss")
	started := false
	e.s.startFn = func(context.Context, Machine, string, []string) error { started = true; return nil }
	e.s.placeOne(e.ctx, i) // a stale snapshot still says queued
	if started {
		t.Fatal("a dismissed idea was started")
	}
	if i, _, _ := st.GetIdea("idea-dism0002"); i.Status != store.IdeaDismissed {
		t.Fatalf("dismiss overwritten: %+v", i)
	}
}

func TestAHandedOutStartIsNeverDoubled(t *testing.T) {
	e := newEnv(t, "person = \"Cole\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n[[machine]]\nname = \"mac\"\nmode = \"push\"\n")
	st := e.s.st
	old := time.Now().Add(-time.Hour).Unix()
	st.AddIdea(store.Idea{ID: "idea-sent0001", Project: "p", Title: "x", Status: store.IdeaStarting, Machine: "mac", SessionName: "idea-sent0001-1", StartedAt: old})
	st.AddStart(store.Start{ID: "st-sent", Machine: "mac", Name: "idea-sent0001-1", IdeaID: "idea-sent0001"})
	st.StartSent("st-sent")
	e.s.watchIdeas(e.ctx)
	if i, _, _ := st.GetIdea("idea-sent0001"); i.Status != store.IdeaStarting {
		t.Fatalf("a start the laptop already has must not be placed again: %+v", i)
	}
	// A session from an earlier run reporting in doesn't take the idea over.
	if changed, _ := st.IdeaSessionSeen("idea-sent0001", "old-run-session", "box", "idea-sent0001-0"); changed {
		t.Fatal("an earlier run's session took over the idea")
	}
	if changed, _ := st.IdeaSessionSeen("idea-sent0001", "this-run", "mac", "idea-sent0001-1"); !changed {
		t.Fatal("the current run's session must link")
	}
}

func TestAStartedLaptopSessionThatNeverReportsFails(t *testing.T) {
	e := newEnv(t, "person = \"Cole\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n[[machine]]\nname = \"mac\"\nmode = \"push\"\n")
	st := e.s.st
	old := time.Now().Add(-48 * time.Hour).Unix()
	st.AddIdea(store.Idea{ID: "idea-strt0001", Project: "p", Title: "x", Status: store.IdeaStarting, Machine: "mac", SessionName: "idea-strt0001-1", StartedAt: old, Attempts: 1})
	st.AddStart(store.Start{ID: "st-strt", Machine: "mac", Name: "idea-strt0001-1", IdeaID: "idea-strt0001"})
	st.StartSent("st-strt")
	st.StartResult("st-strt", true, "")
	st.AddIdea(store.Idea{ID: "idea-strt0002", Project: "p", Title: "y", Status: store.IdeaStarting, Machine: "mac", SessionName: "idea-strt0002-1", StartedAt: old, Attempts: 1})
	e.s.watchIdeas(e.ctx)
	if i, _, _ := st.GetIdea("idea-strt0001"); i.Status != store.IdeaFailed || !strings.Contains(i.Error, "never reported in") {
		t.Fatalf("a started session that never reports in must fail, not hold a slot forever: %+v", i)
	}
	if i, _, _ := st.GetIdea("idea-strt0002"); i.Status != store.IdeaQueued {
		t.Fatalf("a claim whose start never got queued goes back to the queue: %+v", i)
	}
}

func TestStartsAreHandedOutOnceAcrossProcesses(t *testing.T) {
	e := newEnv(t, "")
	other, err := store.Open(e.s.st.Path()) // `docket server receive` is another process
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	e.s.st.AddStart(store.Start{ID: "st-once", Machine: "mac", Name: "idea-once0001-1", IdeaID: "idea-once0001"})
	if !e.s.st.StartSent("st-once") || other.StartSent("st-once") {
		t.Fatal("a start must be handed out exactly once")
	}
	// Writes from the other process mid-edit don't make edits fail.
	e.s.st.AddIdea(store.Idea{ID: "idea-edit0001", Title: "x", Status: store.IdeaQueued})
	_, err = e.s.st.EditIdea("idea-edit0001", func(x *store.Idea) error {
		other.Set("poke", "1")
		x.Status = store.IdeaSaved
		return nil
	})
	if err != nil {
		t.Fatalf("an edit failed because another process wrote: %v", err)
	}
}

func TestPruneAudioKeepsWhatEvidenceUses(t *testing.T) {
	e := ideasEnv(t, "")
	st := e.s.st
	os.MkdirAll(filepath.Join(e.s.cfg.DataDir, "blobs"), 0o700)
	shared, alone := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, sha := range []string{shared, alone} {
		os.WriteFile(e.s.blobPath(sha), []byte("RIFF"), 0o600)
	}
	old := time.Now().Add(-200 * 24 * time.Hour).Unix()
	st.AddDictation(store.Dictation{ID: "dict-old00001", Status: store.DictRouted, AudioSHA: shared, CreatedAt: old})
	st.AddDictation(store.Dictation{ID: "dict-old00002", Status: store.DictRouted, AudioSHA: alone, CreatedAt: old})
	st.AddEvidence(store.Evidence{SHA256: shared, ItemID: "dk-x", Name: "a.wav"})
	e.s.pruneAudio()
	if _, err := os.Stat(e.s.blobPath(shared)); err != nil {
		t.Fatal("a blob evidence still uses was deleted")
	}
	if _, err := os.Stat(e.s.blobPath(alone)); err == nil {
		t.Fatal("old audio wasn't pruned")
	}
}

func TestSyncConfirmsRemotes(t *testing.T) {
	e := newEnv(t, "person = \"Cole\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n[[machine]]\nname = \"mac\"\nmode = \"push\"\n")
	e.s.st.LearnDir("mac", "/Users/c/app", "app", true)
	raw, _ := json.Marshal(map[string]string{"/Users/c/app": "https://github.com/acme/app.git", "/Users/c/x": "ext::sh -c id"})
	e.s.ApplyRemotes("mac", raw)
	homes, _ := e.s.st.ProjectHomes("app")
	if len(homes) != 1 || homes[0].Remote != "https://github.com/acme/app.git" {
		t.Fatalf("homes = %+v", homes)
	}
	_ = record.Record{}
}
