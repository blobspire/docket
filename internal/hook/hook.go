// Package hook handles Claude Code hook events on the agent's machine.
//
// Every event is recorded in the local spool. A few events also do work:
//   - SessionStart: tells the session its id via CLAUDE_ENV_FILE
//   - SessionStart, UserPromptSubmit, PostToolUse, Stop: hand over any answer
//     waiting in the mailbox for this session
//   - Stop: the self-check, which nudges an agent (at most once per turn) that
//     asked for something in chat without filing it
//   - Wait (a background Stop hook with asyncRewake): sleeps until an answer
//     arrives for this idle session, then wakes it by exiting 2
//
// Hooks must never break a session, so everything is best effort.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"crypto/sha256"
	"encoding/hex"

	"github.com/blobspire/docket/internal/claudemd"
	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/selfcheck"
	"github.com/blobspire/docket/internal/spool"
	"github.com/blobspire/docket/internal/turnstate"
)

const maxInput = 8 << 20

// How long a waiter stays armed. The plugin's hook timeout must be longer.
const defaultWaitMax = 11*time.Hour + 50*time.Minute

type input struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	AgentID        string `json:"agent_id"`
	StopHookActive bool   `json:"stop_hook_active"`
	LastMessage    string `json:"last_assistant_message"`
}

var sessionIDOK = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Run handles one hook event and returns the process exit code. Only the
// Wait event ever returns non-zero (2, to wake the session).
func Run(event string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) (int, error) {
	if getenv("DOCKET_INTERNAL") == "1" {
		return 0, nil
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxInput))
	if err != nil {
		return 0, err
	}
	var in input
	_ = json.Unmarshal(raw, &in)
	if event == "" {
		event = in.HookEventName
	}
	if event == "" {
		event = "unknown"
	}
	box := mailbox.Box{Dir: config.MailboxDir()}
	sp := spool.Spool{Dir: config.SpoolDir()}

	switch event {
	case "Wait":
		return wait(in, box, sp, stderr, getenv), nil
	case "PostToolUse":
		// Hot path: runs after every tool call, so record nothing unless an
		// answer is waiting. Never deliver into a subagent's context.
		if in.AgentID == "" && box.Pending(in.SessionID) {
			deliver(in.SessionID, box, sp, stdout, getenv, "PostToolUse", "post_tool")
		}
		return 0, nil
	}

	r := record.New(event, getenv)
	r.SessionID = in.SessionID
	r.Cwd = in.Cwd
	r.TranscriptPath = in.TranscriptPath
	if len(raw) > 0 {
		r.Payload = record.CapPayload(raw)
	}
	if err := sp.Append(r); err != nil {
		return 0, err
	}

	switch event {
	case "SessionStart":
		bindSession(in.SessionID, getenv)
		deliverPlain(in.SessionID, box, sp, stdout, getenv, "session_start")
	case "UserPromptSubmit":
		deliverPlain(in.SessionID, box, sp, stdout, getenv, "prompt")
	case "Stop":
		if in.AgentID != "" {
			return 0, nil
		}
		if box.Pending(in.SessionID) {
			deliver(in.SessionID, box, sp, stdout, getenv, "Stop", "stop")
			return 0, nil
		}
		selfCheck(in, sp, stdout, getenv)
	}
	return 0, nil
}

// deliver hands waiting answers to the session as hook additionalContext.
func deliver(sid string, box mailbox.Box, sp spool.Spool, stdout io.Writer, getenv func(string) string, hookEvent, via string) {
	ds, err := box.Claim(sid)
	if err != nil || len(ds) == 0 {
		return
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     hookEvent,
		"additionalContext": mailbox.Text(ds),
	}})
	fmt.Fprintln(stdout, string(out))
	receipts(ds, sp, getenv, via)
}

// deliverPlain hands answers over as plain stdout, which SessionStart and
// UserPromptSubmit add to the session's context.
func deliverPlain(sid string, box mailbox.Box, sp spool.Spool, stdout io.Writer, getenv func(string) string, via string) {
	ds, err := box.Claim(sid)
	if err != nil || len(ds) == 0 {
		return
	}
	fmt.Fprintln(stdout, mailbox.Text(ds))
	receipts(ds, sp, getenv, via)
}

func receipts(ds []mailbox.Delivery, sp spool.Spool, getenv func(string) string, via string) {
	for _, d := range ds {
		r := record.New("docket.receipt", getenv)
		r.SessionID = d.SessionID
		r.Payload, _ = json.Marshal(map[string]string{"nonce": d.Nonce, "delivery_id": d.ID, "item_id": d.ItemID, "via": via})
		sp.Append(r)
	}
}

// wait is the background waiter: it keeps an idle session reachable.
func wait(in input, box mailbox.Box, sp spool.Spool, stderr io.Writer, getenv func(string) string) int {
	if in.AgentID != "" {
		return 0
	}
	st, ok := turnstate.For(in.SessionID)
	if !ok {
		return 0
	}
	me := os.Getpid()
	st.SetWaiter(me) // supersedes any older waiter for this session
	parent, _ := strconv.Atoi(getenv("CLAUDE_PID"))
	max := defaultWaitMax
	if d, err := time.ParseDuration(getenv("DOCKET_WAIT_MAX")); err == nil && d > 0 {
		max = d
	}
	poll := time.Second
	if d, err := time.ParseDuration(getenv("DOCKET_WAIT_POLL")); err == nil && d > 0 {
		poll = d
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if st.Waiter() != me {
			return 0 // a newer turn armed its own waiter
		}
		if parent > 0 && !alive(parent) {
			return 0
		}
		if box.Pending(in.SessionID) {
			if ds, _ := box.Claim(in.SessionID); len(ds) > 0 {
				fmt.Fprint(stderr, mailbox.Text(ds))
				receipts(ds, sp, getenv, "rewake")
				return 2
			}
		}
		time.Sleep(poll)
	}
	return 0
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// selfCheck nudges the agent once when its final message asks the human for
// something and nothing was filed this turn. The follow-up turn is scored as
// a hit (it filed) or a miss (it didn't; the server keeps the sentence as a
// low-priority "maybe" so it can't vanish).
func selfCheck(in input, sp spool.Spool, stdout io.Writer, getenv func(string) string) {
	st, ok := turnstate.For(in.SessionID)
	if !ok || getenv("DOCKET_SELFCHECK") == "0" || !claudemd.Installed() {
		return
	}
	state := st.Load()
	now := time.Now().UnixNano()
	defer func() {
		state.LastStop = now
		st.Save(state)
	}()

	if in.StopHookActive {
		if p := state.Pending; p != nil {
			outcome := "miss"
			if st.LastFiled() > p.At {
				outcome = "hit"
			}
			nudgeRecord(in.SessionID, sp, getenv, p.Sentence, p.Pattern, outcome)
			state.Pending = nil
		}
		return
	}
	state.Pending = nil
	if st.LastFiled() > state.LastStop {
		return // it already filed something this turn
	}
	sentence, pattern, found := selfcheck.Match(in.LastMessage)
	if !found {
		return
	}
	h := shortHash(sentence)
	for _, done := range state.Nudged {
		if done == h {
			return
		}
	}
	state.Nudged = append(state.Nudged, h)
	state.Pending = &turnstate.Pending{Sentence: sentence, Pattern: pattern, At: now}
	nudgeRecord(in.SessionID, sp, getenv, sentence, pattern, "")

	person := person()
	msg := fmt.Sprintf("docket self-check: your last message says %q. If that is something only %s can do or decide, file it now with `docket ask`, `docket task` or `docket review` (filing does not block you). If it is not, just finish.", clip(sentence, 300), person)
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "Stop", "additionalContext": msg}})
	fmt.Fprintln(stdout, string(out))
}

func nudgeRecord(sid string, sp spool.Spool, getenv func(string) string, sentence, pattern, outcome string) {
	r := record.New("docket.nudge", getenv)
	r.SessionID = sid
	r.Payload, _ = json.Marshal(map[string]string{"sentence": sentence, "pattern": pattern, "outcome": outcome})
	sp.Append(r)
}

// person is how docket refers to the human (set by `docket init --person`).
func person() string {
	if b, err := os.ReadFile(filepath.Join(config.Home(), "person")); err == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			return p
		}
	}
	return "the user"
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(strings.ToLower(s)))
	return hex.EncodeToString(h[:6])
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
