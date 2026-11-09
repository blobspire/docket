// Package hook handles Claude Code hook events: it records each one in the
// local spool, quickly and without ever failing the session.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/spool"
)

// maxInput bounds how much hook JSON we read from stdin.
const maxInput = 8 << 20

// Common fields Claude Code sends to every hook.
type input struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
}

var sessionIDOK = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Run records one hook event. event is the name the plugin passed; the
// payload's own hook_event_name wins when present. Anything it writes to
// stdout is read by Claude Code, so it writes nothing unless it means to.
func Run(event string, stdin io.Reader, stdout io.Writer, getenv func(string) string) error {
	if getenv("DOCKET_INTERNAL") == "1" {
		return nil // a session docket itself started; don't record it
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxInput))
	if err != nil {
		return err
	}
	var in input
	_ = json.Unmarshal(raw, &in) // best effort; the raw payload is kept regardless
	if in.HookEventName != "" {
		event = in.HookEventName
	}
	if event == "" {
		event = "unknown"
	}

	r := record.New(event, getenv)
	r.SessionID = in.SessionID
	r.Cwd = in.Cwd
	r.TranscriptPath = in.TranscriptPath
	if len(raw) > 0 {
		r.Payload = record.CapPayload(raw)
	}

	if err := (spool.Spool{Dir: config.SpoolDir()}).Append(r); err != nil {
		return err
	}

	if event == "SessionStart" {
		bindSession(in.SessionID, getenv)
	}
	return nil
}

// bindSession exports DOCKET_SESSION into the session's environment through
// CLAUDE_ENV_FILE, so `docket ask` run by the agent knows which session filed it.
func bindSession(sessionID string, getenv func(string) string) {
	envFile := getenv("CLAUDE_ENV_FILE")
	if envFile == "" || !sessionIDOK.MatchString(sessionID) {
		return
	}
	f, err := os.OpenFile(envFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "export DOCKET_SESSION=%s\n", sessionID)
}
