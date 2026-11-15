// Package ingest turns raw spool records into sessions and queue items.
//
// Agents decide what reaches the human: nothing here guesses from chat text.
// Rules are deterministic and no model reads anything.
//   - cli.file / cli.add / cli.done: items an agent filed, added to or resolved
//   - StopFailure / auth notifications: Claude login or billing failures, which
//     an agent can't file itself because it can't run (one item per machine)
//   - Notification / Stop / UserPromptSubmit / SessionEnd: session state (the
//     server raises a lead stuck on a dialog; see the server's health checks)
//   - docket.receipt: an answer reached its session
package ingest

import (
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
	SrcFailure      = "stop_failure"
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

// Options carry what ingest needs to know from the server's config.
type Options struct {
	// LeadProject maps a lead name to its project ("" if unknown).
	LeadProject func(lead string) string
	// SystemProject is where docket's own health items go (logins, machines).
	SystemProject string
}

func (o Options) system() string {
	if o.SystemProject != "" {
		return o.SystemProject
	}
	return "system"
}

// Process applies every unprocessed event, oldest first. It returns how many
// events it handled.
func Process(s *store.Store, batch int) (int, error) { return ProcessWith(s, batch, Options{}) }

// ProcessWith is Process with server configuration.
func ProcessWith(s *store.Store, batch int, o Options) (int, error) {
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
			if err := apply(s, e, o); err != nil {
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

func apply(s *store.Store, e store.RawEvent, o Options) error {
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
		return fileItem(s, r, base, o)

	case "cli.add":
		var a struct {
			ItemID   string            `json:"item_id"`
			Text     string            `json:"text"`
			Evidence []record.Evidence `json:"evidence"`
		}
		_ = json.Unmarshal(r.Payload, &a)
		it, ok, _ := s.ResolveItem(a.ItemID)
		if !ok {
			return nil
		}
		if a.Text != "" {
			if err := s.AppendToItem(it.ID, clip(a.Text, 20000)); err != nil {
				return err
			}
		}
		for _, ev := range a.Evidence {
			s.AddEvidence(store.Evidence{SHA256: ev.SHA256, ItemID: it.ID, Name: ev.Name, Mime: ev.Mime, Size: ev.Size})
		}
		return nil

	case "cli.done":
		d := r.Done
		if d == nil {
			return nil
		}
		var it store.Item
		var ok bool
		switch {
		case d.ItemID != "":
			it, ok, _ = s.ResolveItem(d.ItemID)
		case d.Key != "":
			k := keyNorm(d.Key)
			if p := store.Slug(d.Project); p != "" {
				it, ok = s.ActiveByKey("key:" + p + ":" + k)
			}
			if !ok {
				it, ok = s.MovedByKey(k, store.Slug(d.Project), sid, r.Machine)
			}
			// The project resolved now may differ from the one at filing time
			// (a folder default vs. an explicit --project then); only an
			// explicitly named project rules out looking elsewhere.
			if !ok && !d.Explicit {
				it, ok = s.ActiveByAgentKey(k, sid, r.Machine)
			}
		}
		// Only an item that still needs someone: an agent's "done" must never
		// overwrite the human's dismissal or "not worth my time".
		if !ok || !active(it.Status) {
			return nil
		}
		return s.SetStatus(it.ID, "done", firstNonEmpty(d.Note, "resolved by the agent"), "")

	case "SessionStart":
		return s.PatchSession(sid, store.SessionPatch{State: store.Str("idle"), StartedAt: store.Int(at), EndedAt: store.Int(0)})

	case "UserPromptSubmit":
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("busy"), WaitingFor: store.Str(""), WaitingSince: store.Int(0)}); err != nil {
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
		return notification(s, sid, p, base, at, o)

	case "cli.project":
		var pr struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(r.Payload, &pr)
		_ = s.EnsureProject(store.Slug(pr.Name), clip(oneLine(pr.Description, 200), 200))
		return nil

	case "StopFailure":
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("waiting"), WaitingFor: store.Str("error: " + p.Error)}); err != nil {
			return err
		}
		// Only a failure no agent can recover from is the human's business:
		// a usage limit or a flaky API call clears on its own.
		if !criticalErrors[p.Error] {
			return nil
		}
		it := base
		it.ID = ulid.ItemID()
		it.Kind = "health"
		it.Source = SrcFailure
		it.Blocking = true
		it.Urgency = "now"
		it.Project = o.system()
		it.SessionID = "" // one item per machine, not per session
		it.Title = failureTitle(p.Error) + " on " + base.Machine
		it.Body = strings.TrimSpace(firstNonEmpty(p.ErrorDetails, p.LastMessage))
		it.DedupeKey = "stopfail:" + base.Machine + ":" + p.Error
		return upsertHealth(s, it)

	case "Stop":
		patch := store.SessionPatch{State: store.Str("idle"), WaitingFor: store.Str(""), WaitingSince: store.Int(0)}
		if p.LastMessage != "" {
			patch.LastMessage = store.Str(clip(p.LastMessage, 4000))
		}
		if err := s.PatchSession(sid, patch); err != nil {
			return err
		}
		if err := confirmReceipts(s, p.LastMessage); err != nil {
			return err
		}
		// A turn that finished means any earlier failure (an expired login, a
		// billing hold) has cleared on this machine.
		if _, err := s.ResolveBySession(sid, "the session continued", SrcNotification, SrcFailure); err != nil {
			return err
		}
		if r.Machine != "" {
			if _, err := s.ResolveKeyPrefix("stopfail:"+r.Machine+":", "a turn on "+r.Machine+" finished normally"); err != nil {
				return err
			}
			if _, err := s.ResolveKeyPrefix("auth:"+r.Machine+":", "a turn on "+r.Machine+" finished normally"); err != nil {
				return err
			}
		}
		return nil

	case "SessionEnd":
		if err := s.PatchSession(sid, store.SessionPatch{State: store.Str("ended"), EndedAt: store.Int(at)}); err != nil {
			return err
		}
		_, err := s.ResolveBySession(sid, "the session ended", SrcNotification)
		return err

	case "docket.nudge", "backfill.candidate", "PermissionDenied":
		return nil // older versions guessed asks from chat text; ignored now

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

	}
	return nil
}

func fileItem(s *store.Store, r record.Record, base store.Item, o Options) error {
	if r.Item == nil || r.Item.ID == "" || strings.TrimSpace(r.Item.Title) == "" {
		return nil
	}
	in := r.Item
	it := base
	it.ID = in.ID
	it.Kind = in.Kind
	it.Source = SrcCLI
	it.Title = clip(oneLine(in.Title, 500), 500)
	it.Body = clip(in.Body, 20000)
	it.Options = in.Options
	it.Recommend = in.Recommend
	it.Blocking = in.Blocking
	it.Blocks = in.Blocks
	it.Physical = in.Physical
	it.Due = in.Due
	it.Component = clip(strings.TrimSpace(in.Component), 60)
	it.Place = store.Slug(in.Place)
	it.ExpiresAt = in.Expires
	switch in.Urgency {
	case "now", "soon", "normal":
		it.Urgency = in.Urgency
	}
	explicit := store.Slug(in.Project)
	it.Project = explicit
	if it.Project == "" {
		it.Project = projectFor(s, o, r.Lead, base.Machine, firstNonEmpty(in.Root, base.Cwd))
	}
	if in.Key != "" {
		k := keyNorm(in.Key)
		it.DedupeKey = "key:" + it.Project + ":" + k
		if hit, ok := s.ActiveByKey(it.DedupeKey); ok {
			// An older docket scoped keys by folder: when that scope happens to
			// be this project's name, the old item is this one; give it the project.
			if hit.Project == "" || hit.Project == store.Unsorted {
				_ = s.ClaimUnsorted(hit.ID, it.Project)
			}
		} else {
			if moved, ok := s.MovedByKey(k, it.Project, it.SessionID, base.Machine); ok {
				// The human moved this item to another project: still the same one.
				it.DedupeKey = moved.DedupeKey
			} else {
				// An item filed with this key before keys were scoped by project
				// (an older docket, or while it had no project) becomes this one.
				s.AdoptLegacyKey(k, base.Machine, it.SessionID, firstNonEmpty(in.Root, base.Cwd), it.Project, it.DedupeKey)
			}
		}
		// The human said items like this aren't worth their time: a re-file
		// within the week is kept for the record but stays out of the queue.
		if when, ok := s.RecentlyNoised(it.DedupeKey, time.Now().Add(-7*24*time.Hour).Unix()); ok {
			// Kept for the record, outside the key (so it can't merge into, or
			// reopen, anything).
			it.Status = "dismissed"
			it.Resolution = "suppressed: --key " + in.Key + " was marked not worth the time on " + time.Unix(when, 0).Format("2006-01-02")
			it.DedupeKey = ""
		}
	}
	id, _, err := s.UpsertItem(it)
	if err != nil {
		return err
	}
	// Bookkeeping is best effort: a failure here must never stop ingest.
	if id != in.ID {
		_ = s.AddAlias(in.ID, id, base.Machine, it.SessionID) // the agent was told in.ID
	}
	if explicit != "" {
		_ = s.EnsureProject(explicit, clip(oneLine(in.NewProject, 200), 200))
		// A project the agent named votes for the folder's default; one the
		// CLI filled in from that default confirms it.
		if in.Root != "" {
			_ = s.LearnDir(base.Machine, in.Root, explicit, in.Explicit)
		}
	}
	for _, ev := range in.Evidence {
		if err := s.AddEvidence(store.Evidence{SHA256: ev.SHA256, ItemID: id, Name: ev.Name, Mime: ev.Mime, Size: ev.Size}); err != nil {
			return err
		}
	}
	return nil
}

// projectFor picks a project for an item that didn't name one (older
// clients, the shim's fallback records): the lead's project, then the
// deepest mapped folder, then a project named like the folder.
func projectFor(s *store.Store, o Options, lead, machine, dir string) string {
	if lead != "" && o.LeadProject != nil {
		if p := o.LeadProject(lead); p != "" {
			return p
		}
	}
	if dirs, err := s.ProjectDirs(machine); err == nil {
		if p := store.ProjectForDir(dirs, dir); p != "" {
			return p
		}
	}
	if b := store.Slug(filepath.Base(dir)); b != "" {
		if _, ok := s.GetProject(b); ok {
			return b
		}
	}
	return store.Unsorted
}

// keyNorm is the stored form of an agent's --key: at most 200 bytes, cut on
// a character boundary, the same on every path that looks a key up.
func keyNorm(k string) string {
	if len(k) <= 200 {
		return k
	}
	cut := 200
	for cut > 0 && (k[cut]&0xC0) == 0x80 {
		cut--
	}
	return k[:cut]
}

func active(status string) bool {
	return status == "maybe" || status == "open" || status == "snoozed" || status == "answered"
}

func notification(s *store.Store, sid string, p payload, base store.Item, at int64, o Options) error {
	switch p.NotificationType {
	case "idle_prompt":
		// Finished and waiting for the next prompt: idle, not stuck. But a
		// dialog that's still open (a permission or a question) stays the wait.
		if sess, _, _ := s.GetSession(sid); sess.State == "waiting" && sess.WaitingFor != "" {
			return nil
		}
		return s.PatchSession(sid, store.SessionPatch{State: store.Str("idle"), WaitingFor: store.Str(""), WaitingSince: store.Int(0)})
	case "permission_prompt", "agent_needs_input", "elicitation_dialog", "elicitation_url_dialog":
		what := "your input"
		if p.NotificationType == "permission_prompt" {
			what = "permission"
			if sess, ok, _ := s.GetSession(sid); ok && sess.PendingPermission != "" {
				what = "permission: " + oneLine(sess.PendingPermission, 160)
			}
		}
		patch := store.SessionPatch{State: store.Str("waiting"), WaitingFor: store.Str(what)}
		if sess, _, _ := s.GetSession(sid); sess.WaitingSince == 0 {
			patch.WaitingSince = store.Int(at)
		}
		return s.PatchSession(sid, patch)
	}
	// auth_storage_failure means the sign-in worked but couldn't be saved,
	// not that the machine is signed out.
	if strings.HasPrefix(p.NotificationType, "auth_") && strings.Contains(p.NotificationType, "fail") && p.NotificationType != "auth_storage_failure" {
		it := base
		it.ID = ulid.ItemID()
		it.Kind = "health"
		it.Source = SrcFailure
		it.Blocking = true
		it.Urgency = "now"
		it.Project = o.system()
		it.SessionID = "" // one item per machine, not per session
		it.Title = "Claude Code is signed out on " + base.Machine
		it.Body = p.Message
		// The same sign-in problem as a StopFailure authentication_failed:
		// one item for both.
		it.DedupeKey = "stopfail:" + base.Machine + ":authentication_failed"
		return upsertHealth(s, it)
	}
	return nil
}

// upsertHealth files a machine-level failure unless the human said such
// alerts aren't worth their time this week.
func upsertHealth(s *store.Store, it store.Item) error {
	if _, noised := s.RecentlyNoised(it.DedupeKey, time.Now().Add(-7*24*time.Hour).Unix()); noised {
		return nil
	}
	_, _, err := s.UpsertItem(it)
	return err
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

func parseTS(ts string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.Unix()
	}
	return time.Now().Unix()
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
