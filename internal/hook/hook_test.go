package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/spool"
)

func setup(t *testing.T) func(string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DOCKET_HOME", home)
	env := map[string]string{"USER": "tester", "TMUX_PANE": "%3", "DOCKET_HOME": home}
	return func(k string) string { return env[k] }
}

func records(t *testing.T) []record.Record {
	t.Helper()
	lines, err := spool.Spool{Dir: config.SpoolDir()}.Tail(100)
	if err != nil {
		t.Fatal(err)
	}
	var out []record.Record
	for _, l := range lines {
		var r record.Record
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestRecordsStopWithMessage(t *testing.T) {
	getenv := setup(t)
	payload := `{"session_id":"abc-123","transcript_path":"/t.jsonl","cwd":"/w","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"I need your approval to push."}`
	var out bytes.Buffer
	if err := Run("Stop", strings.NewReader(payload), &out, getenv); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("hook wrote to stdout (Claude Code would read it): %q", out.String())
	}
	rs := records(t)
	if len(rs) != 1 {
		t.Fatalf("got %d records", len(rs))
	}
	r := rs[0]
	if r.Event != "Stop" || r.SessionID != "abc-123" || r.Cwd != "/w" || r.TranscriptPath != "/t.jsonl" || r.Env["TMUX_PANE"] != "%3" || len(r.ID) != 26 {
		t.Fatalf("record = %+v", r)
	}
	if !strings.Contains(string(r.Payload), "need your approval") {
		t.Fatalf("payload lost the message: %s", r.Payload)
	}
}

func TestGarbageInputStillRecorded(t *testing.T) {
	getenv := setup(t)
	if err := Run("Notification", strings.NewReader("not json at all"), &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	rs := records(t)
	if len(rs) != 1 || rs[0].Event != "Notification" || !strings.Contains(string(rs[0].Payload), "unparsable") {
		t.Fatalf("got %+v", rs)
	}
}

func TestHugeStringsAreCapped(t *testing.T) {
	getenv := setup(t)
	huge := strings.Repeat("é", record.MaxString) // multi-byte, twice the cap in bytes
	payload, _ := json.Marshal(map[string]string{"session_id": "s", "last_assistant_message": huge})
	if err := Run("Stop", bytes.NewReader(payload), &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	r := records(t)[0]
	var p map[string]string
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["last_assistant_message"]
	if len(msg) > record.MaxString+64 || !strings.Contains(msg, "truncated") {
		t.Fatalf("not capped: %d bytes", len(msg))
	}
}

func TestInternalSessionsAreIgnored(t *testing.T) {
	getenv := setup(t)
	internal := func(k string) string {
		if k == "DOCKET_INTERNAL" {
			return "1"
		}
		return getenv(k)
	}
	Run("Stop", strings.NewReader(`{"session_id":"x"}`), &bytes.Buffer{}, internal)
	if rs := records(t); len(rs) != 0 {
		t.Fatalf("recorded %d internal events", len(rs))
	}
}

func TestSessionStartBindsSessionID(t *testing.T) {
	getenv := setup(t)
	envFile := filepath.Join(t.TempDir(), "env.sh")
	withEnvFile := func(k string) string {
		if k == "CLAUDE_ENV_FILE" {
			return envFile
		}
		return getenv(k)
	}
	Run("SessionStart", strings.NewReader(`{"session_id":"0b1c-22","hook_event_name":"SessionStart","source":"startup"}`), &bytes.Buffer{}, withEnvFile)
	b, _ := os.ReadFile(envFile)
	if string(b) != "export DOCKET_SESSION=0b1c-22\n" {
		t.Fatalf("env file = %q", b)
	}
	// A hostile session id must never reach the shell.
	os.Remove(envFile)
	Run("SessionStart", strings.NewReader(`{"session_id":"x; rm -rf ~","hook_event_name":"SessionStart"}`), &bytes.Buffer{}, withEnvFile)
	if _, err := os.Stat(envFile); err == nil {
		t.Fatal("wrote an env file for an unsafe session id")
	}
}

func TestSecretsNeverCaptured(t *testing.T) {
	getenv := setup(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-secret-value")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	Run("Stop", strings.NewReader(`{}`), &bytes.Buffer{}, func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return os.Getenv(k)
	})
	line, _ := spool.Spool{Dir: config.SpoolDir()}.Tail(1)
	if strings.Contains(string(line[0]), "sk-secret-value") {
		t.Fatal("captured a token value")
	}
	r := records(t)[0]
	if r.Env["CLAUDE_CODE_ENTRYPOINT"] != "cli" {
		t.Fatalf("expected harmless CLAUDE_* value kept, env=%v", r.Env)
	}
}
