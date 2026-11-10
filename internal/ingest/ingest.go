// Package ingest turns raw spool records into sessions and queue items.
//
// Rules are deterministic: no model reads anything here.
//   - cli.file / cli.done: items an agent filed or resolved itself
//   - PermissionDenied: auto mode refused an action; the agent may need the human
//   - Notification permission_prompt / agent_needs_input: a session is waiting on the human
//   - StopFailure: login, billing or API errors (health)
//   - Stop / UserPromptSubmit / SessionEnd: session state, and closing the
//     notification-based items once the session has moved on
//   - docket.receipt: an answer reached its session
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// Sources an item can come from. Notification-based items close on their own
// when the session continues; filed items stay until someone resolves them.
const (
	SrcCLI          = "cli"
	SrcNotification = "notification"
	SrcDenied       = "permission_denied"
	SrcFailure      = "stop_failure"
	SrcBackfill     = "backfill"
	SrcWatchdog     = "watchdog"
)

// Critical health errors: the session can't continue until the human acts.
var criticalErrors = map[string]bool{
	"authentication_failed": true, "oauth_org_not_allowed": true, "account_on_hold": true,
	"billing_error": true, "cloud_credential_error": true,
}

// correctionRe spots prompts where the human corrects or redirects an agent:
// raw material for the preference rules the brain proposes.
var correctionRe = regexp.MustCompile(`(?i)^\s*(no[,.! ]|nope|don'?t|do not|stop|actually|instead|wrong|that'?s not|not what i|why did you|why are you|please don'?t|never |always |i said|i told you|we (already|don'?t)|you should(n'?t)?|remember (to|that))`)

var nonceRe = regexp.MustCompile(`\[docket (dk-[0-9a-z]{8} )?#([0-9a-z]{6,16})\]`)

// Process applies every unprocessed event, oldest first. It returns how many
// events it handled.
func Process(s *store.Store, batch int) (int, error) {
	total := 0
	for {
		evs, err := s.Unprocessed(batch)
		if err != nil {
			return total, err
		}
		if len(evs) == 0 {
			return total, nil
		}
		for _, e := range evs {
			if err := apply(s, e); err != nil {
				return total, fmt.Errorf("ingest %s: %w", e.ID, err)
			}
			if err := s.MarkProcessed(e.ID); err != nil {
				return total, err
			}
			total++
		}
		s.Bump()
	}
}

// legacy is a v0 record written by the plugin shim when the binary was missing.
type legacy struct {
	V     int             `json:"v"`
	TS    string          `json:"ts"`
	Event string          `json:"event"`
	Raw   json.RawMessage `json:"raw"`
}

type payload struct {
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	TranscriptPath   string          `json:"transcript_path"`
	Source           string          `json:"source"`
	Reason           string          `json:"reason"`
	Prompt           string          `json:"prompt"`
	NotificationType string          `json:"notification_type"`
	Message          string          `json:"message"`
	Title            string          `json:"title"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	Error            string          `json:"error"`
	ErrorDetails     string          `json:"error_details"`
	LastMessage      string          `json:"last_assistant_message"`
	StopHookActive   bool            `json:"stop_hook_active"`
}

// Nudge and receipt details written by docket's own hooks.
type nudge struct {
	Sentence string `json:"sentence"`
	Pattern  string `json:"pattern"`
	Outcome  string `json:"outcome"` // "" when nudging, "hit" or "miss" when scored
}

type candidate struct {
	Sentence string `json:"sentence"`
	Pattern  string `json:"pattern"`
	At       string `json:"at"`
}

func apply(s *store.Store, e store.RawEvent) error {
	var r record.Record
	if err := json.Unmarshal(e.Body, &r); err != nil {
		return nil // unreadable record: keep it in events, skip it here
	}
	if r.V == 0 {
		var l legacy
		if json.Unmarshal(e.Body, &l) == nil && len(l.Raw) > 0 {
			r.Event, r.TS, r.Payload = l.Event, l.TS, l.Raw
			r.Machine = e.Machine
		}
	}
	// The machine is the one the server collected the record from (its name
	// in the server config), not whatever the record calls itself.
	if e.Machine != "" {
		r.Machine = e.Machine
	}
	var p payload
	if len(r.Payload) > 0 {
		_ = json.Unmarshal(r.Payload, &p)
	}
	sid := firstNonEmpty(r.SessionID, p.SessionID)
	cwd := firstNonEmpty(r.Cwd, p.Cwd)
	at := parseTS(r.TS)

	// Every event refreshes what we know about its session.
	if sid != "" && !strings.HasPrefix(r.Event, "cli.") && !strings.HasPrefix(r.Event, "backfill.") {
		patch := store.SessionPatch{Machine: store.Str(r.Machine), LastEventAt: store.Int(at), HasPlugin: store.Bool(true)}
		if cwd != "" {
			patch.Cwd = store.Str(cwd)
		}
		if r.Lead != "" {
			patch.Lead = store.Str(r.Lead)
		}
		if tp := firstNonEmpty(r.TranscriptPath, p.TranscriptPath); tp != "" {
			patch.TranscriptPath = store.Str(tp)
		}
		if pane := r.Env["TMUX_PANE"]; pane != "" && r.Env["TMUX"] != "" {
			patch.Tmux = store.Str(pane)
		}
		if err := s.PatchSession(sid, patch); err != nil {
			return err
		}
	}

	base := store.Item{Machine: r.Machine, SessionID: sid, Lead: r.Lead, Cwd: cwd, CreatedAt: at}

	switch r.Event {
	case "cli.file":
		return fileItem(s, r, base)

	case "cli.add":
		var a struct {
			ItemID   string            `json:"item_id"`
			Text     string            `json:"text"`
			Evidence []record.Evidence `json:"evidence"`
		}
		_ = json.Unmarshal(r.Payload, &a)
		if _, ok, _ := s.GetItem(a.ItemID); !ok {
			return nil
		}
		if a.Text != "" {
			if err := s.AppendToItem(a.ItemID, clip(a.Text, 20000)); err != nil {
				return err
			}
		}
		for _, ev := range a.Evidence {
			s.AddEvidence(store.Evidence{SHA256: ev.SHA256, ItemID: a.ItemID, Name: ev.Name, Mime: ev.Mime, Size: ev.Size})
		}
		return s.SetChiefNote(a.ItemID, "")

	case "cli.done":
		if r.Done == nil {
			return nil
		}
		if _, ok, _ := s.GetItem(r.Done.ItemID); !ok {
			return nil
		}
		return s.SetStatus(r.Done.ItemID, "done", firstNonEmpty(r.Done.Note, "resolved by the agent"), "")

	case "SessionStart":
		return s.PatchSession(sid, store.SessionPatch{State: store.Str("idle"), StartedAt: store.Int(at), EndedAt: store.Int(0)})

	case "UserPromptSubmit":
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("busy"), WaitingFor: store.Str("")}); err != nil {
			return err
		}
		if err := confirmReceipts(s, p.Prompt); err != nil {
			return err
		}
		if pr := strings.TrimSpace(p.Prompt); pr != "" && !strings.HasPrefix(pr, "[docket") && correctionRe.MatchString(pr) {
			if err := s.AddCorrection(at, clip(pr, 600), filepath.Base(cwd)); err != nil {
				return err
			}
		}
		_, err := s.ResolveBySession(sid, "the session continued", SrcNotification)
		return err

	case "PermissionRequest":
		return s.PatchSession(sid, store.SessionPatch{PendingPermission: store.Str(describeTool(p.ToolName, p.ToolInput))})

	case "Notification":
		return notification(s, sid, p, base)

	case "PermissionDenied":
		it := base
		cmd := describeTool(p.ToolName, p.ToolInput)
		it.ID = ulid.ItemID()
		it.Kind = "approval"
		it.Source = SrcDenied
		it.Title = "Auto mode refused: " + oneLine(cmd, 160)
		it.Body = "Auto mode blocked this action" + reasonSuffix(p.Reason) + ". The agent may be waiting for your decision.\n\n" + cmd
		it.DedupeKey = "perm:" + sid + ":" + hash(p.ToolName+string(p.ToolInput))
		_, _, err := s.UpsertItem(it)
		return err

	case "StopFailure":
		it := base
		it.ID = ulid.ItemID()
		it.Kind = "health"
		it.Source = SrcFailure
		it.Blocking = criticalErrors[p.Error]
		it.Title = failureTitle(p.Error)
		it.Body = strings.TrimSpace(firstNonEmpty(p.ErrorDetails, p.LastMessage))
		it.DedupeKey = "stopfail:" + sid + ":" + p.Error
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("waiting"), WaitingFor: store.Str("error: " + p.Error)}); err != nil {
			return err
		}
		_, _, err := s.UpsertItem(it)
		return err

	case "Stop":
		patch := store.SessionPatch{State: store.Str("idle"), WaitingFor: store.Str("")}
		if p.LastMessage != "" {
			patch.LastMessage = store.Str(clip(p.LastMessage, 4000))
		}
		if err := s.PatchSession(sid, patch); err != nil {
			return err
		}
		if err := confirmReceipts(s, p.LastMessage); err != nil {
			return err
		}
		// A turn that finished means any earlier failure (a usage limit, an
		// expired login) has cleared for this session.
		_, err := s.ResolveBySession(sid, "the session continued", SrcNotification, SrcFailure)
		return err

	case "SessionEnd":
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("ended"), EndedAt: store.Int(at)}); err != nil {
			return err
		}
		_, err := s.ResolveBySession(sid, "the session ended", SrcNotification)
		return err

	case "docket.nudge":
		var n nudge
		_ = json.Unmarshal(r.Payload, &n)
		switch n.Outcome {
		case "":
			return s.PatchSession(sid, store.SessionPatch{AddNudge: true})
		case "hit":
			return s.PatchSession(sid, store.SessionPatch{AddNudgeHit: true})
		case "miss":
			// The agent was nudged and filed nothing: keep the sentence as a
			// low-priority "maybe" so a real ask can't vanish silently.
			it := base
			it.ID = ulid.ItemID()
			it.Kind = "ask"
			it.Status = "maybe"
			it.Source = SrcBackfill
			it.Title = oneLine(n.Sentence, 200)
			it.Quote = n.Sentence
			it.DedupeKey = "nudge:" + sid + ":" + hash(n.Sentence)
			_, _, err := s.UpsertItem(it)
			return err
		}
		return nil

	case "docket.receipt":
		var rc struct {
			Nonce string `json:"nonce"`
		}
		_ = json.Unmarshal(r.Payload, &rc)
		if rc.Nonce != "" {
			_, err := s.ConfirmDelivery(rc.Nonce)
			return err
		}
		return nil

	case "job.register", "job.beat", "job.done":
		var j struct {
			Name   string `json:"name"`
			Every  int64  `json:"every"`
			Note   string `json:"note"`
			Failed bool   `json:"failed"`
		}
		_ = json.Unmarshal(r.Payload, &j)
		if j.Name == "" {
			return nil
		}
		scope := firstNonEmpty(r.Lead, r.Machine)
		switch r.Event {
		case "job.register":
			return s.JobRegister(store.Job{Scope: scope, Name: j.Name, Machine: r.Machine, SessionID: sid, Lead: r.Lead, Every: j.Every, Note: j.Note}, at)
		case "job.beat":
			return s.JobBeat(scope, j.Name, j.Note, at)
		default:
			return s.JobDone(scope, j.Name, j.Note, j.Failed, at)
		}

	case "backfill.candidate":
		var c candidate
		_ = json.Unmarshal(r.Payload, &c)
		if c.Sentence == "" {
			return nil
		}
		it := base
		it.ID = ulid.ItemID()
		it.Kind = "ask"
		it.Status = "maybe"
		it.Source = SrcBackfill
		it.Title = oneLine(c.Sentence, 200)
		it.Quote = c.Sentence
		it.DedupeKey = "backfill:" + sid + ":" + hash(c.Sentence)
		if t := parseTS(c.At); t > 0 {
			it.CreatedAt = t
		}
		_, _, err := s.UpsertItem(it)
		return err
	}
	return nil
}

func fileItem(s *store.Store, r record.Record, base store.Item) error {
	if r.Item == nil || r.Item.ID == "" || strings.TrimSpace(r.Item.Title) == "" {
		return nil
	}
	in := r.Item
	it := base
	it.ID = in.ID
	it.Kind = in.Kind
	it.Source = SrcCLI
	it.Title = clip(in.Title, 500)
	it.Body = clip(in.Body, 20000)
	it.Options = in.Options
	it.Recommend = in.Recommend
	it.Blocking = in.Blocking
	it.Blocks = in.Blocks
	it.Physical = in.Physical
	it.Due = in.Due
	if in.Key != "" {
		scope := firstNonEmpty(r.Lead, filepath.Base(base.Cwd), base.Machine)
		it.DedupeKey = "key:" + scope + ":" + in.Key
	}
	id, _, err := s.UpsertItem(it)
	if err != nil {
		return err
	}
	for _, ev := range in.Evidence {
		if err := s.AddEvidence(store.Evidence{SHA256: ev.SHA256, ItemID: id, Name: ev.Name, Mime: ev.Mime, Size: ev.Size}); err != nil {
			return err
		}
	}
	return nil
}

func notification(s *store.Store, sid string, p payload, base store.Item) error {
	switch p.NotificationType {
	case "idle_prompt":
		return s.PatchSession(sid, store.SessionPatch{State: store.Str("waiting"), WaitingFor: store.Str("your input")})
	case "permission_prompt", "agent_needs_input", "elicitation_dialog", "elicitation_url_dialog":
		what := "your input"
		title := "Waiting for your input"
		if p.NotificationType == "permission_prompt" {
			what = "permission"
			title = "Waiting for your permission"
			if sess, ok, _ := s.GetSession(sid); ok && sess.PendingPermission != "" {
				title += ": " + oneLine(sess.PendingPermission, 160)
			}
		}
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("waiting"), WaitingFor: store.Str(what)}); err != nil {
			return err
		}
		it := base
		it.ID = ulid.ItemID()
		it.Kind = "blocker"
		it.Source = SrcNotification
		it.Blocking = true
		it.Title = title
		it.Body = p.Message
		it.DedupeKey = "notif:" + sid + ":" + p.NotificationType
		_, _, err := s.UpsertItem(it)
		return err
	}
	if strings.HasPrefix(p.NotificationType, "auth_") && strings.Contains(p.NotificationType, "fail") {
		it := base
		it.ID = ulid.ItemID()
		it.Kind = "health"
		it.Source = SrcFailure
		it.Blocking = true
		it.Title = "Claude Code sign-in problem on " + base.Machine
		it.Body = p.Message
		it.DedupeKey = "auth:" + base.Machine + ":" + p.NotificationType
		_, _, err := s.UpsertItem(it)
		return err
	}
	return nil
}

// confirmReceipts marks deliveries confirmed when their nonce shows up in a
// prompt or message the session saw.
func confirmReceipts(s *store.Store, text string) error {
	for _, m := range nonceRe.FindAllStringSubmatch(text, -1) {
		if _, err := s.ConfirmDelivery(m[2]); err != nil {
			return err
		}
	}
	return nil
}

func describeTool(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	if cmd, ok := in["command"].(string); ok && cmd != "" {
		return cmd
	}
	for _, k := range []string{"file_path", "url", "path", "pattern"} {
		if v, ok := in[k].(string); ok && v != "" {
			return name + " " + v
		}
	}
	if len(input) > 0 && string(input) != "null" {
		return name + " " + clip(string(input), 300)
	}
	return firstNonEmpty(name, "a tool call")
}

func failureTitle(code string) string {
	switch code {
	case "authentication_failed", "oauth_org_not_allowed":
		return "Claude Code is signed out or its login expired"
	case "billing_error", "account_on_hold":
		return "Claude account problem: " + strings.ReplaceAll(code, "_", " ")
	case "rate_limit":
		return "Hit a Claude usage limit"
	case "":
		return "A turn failed with an API error"
	}
	return "A turn failed: " + strings.ReplaceAll(code, "_", " ")
}

func reasonSuffix(r string) string {
	if r = strings.TrimSpace(r); r == "" {
		return ""
	}
	return " (" + oneLine(r, 200) + ")"
}

func parseTS(ts string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.Unix()
	}
	return time.Now().Unix()
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, n)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
