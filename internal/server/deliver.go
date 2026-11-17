package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// How long a sent delivery may go unconfirmed before it is resent or retried
// another way.
const unconfirmedAfter = 3 * time.Minute

// Action is a decision the human made on an item in the queue.
type Action struct {
	Kind    string // approve | decline | choose | answer | done | dismiss | noise | snooze | stash | unstash | reopen | reclassify | undo
	Text    string // optional comment / free-form answer / the human's "not worth it" note
	Option  int    // for choose: index into item.Options
	Until   time.Time
	Project string // for reclassify
	Urgency string // for reclassify
}

// Decide applies the human's action to an item and, when the agent should
// hear about it, queues a delivery to the session that filed it. docket only
// relays decisions the human actually made here; it never decides itself.
func (s *Server) Decide(id string, a Action) error {
	it, ok, err := s.st.GetItem(id)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no such item")
	}
	defer s.st.Bump()
	// A closed item can only be reopened, undone, or kept on (or taken off)
	// the Stash; acting on it again would quietly reopen it, and moving it
	// would put a move between the close and the Reopen that undoes it.
	// One whose answer reached its agent can still take a follow-up, or be
	// closed.
	switch it.Status {
	case "done", "dismissed", "expired":
		if a.Kind == "reopen" {
			return s.reopen(it)
		}
		if a.Kind != "undo" && a.Kind != "stash" && a.Kind != "unstash" {
			return errors.New("this item is already closed; reopen it to act on it again")
		}
	case "delivered":
		switch a.Kind {
		case "undo", "reclassify", "answer", "done", "dismiss", "stash", "unstash", "reopen":
		default:
			return errors.New("your answer has reached the agent; send a follow-up, or mark it done")
		}
	case "answered":
		// Its answer is still on the way: a second one (a double press, a
		// second tab) would reach the agent twice. Nor can it be snoozed: it
		// would wake as open, asking for an answer it already has.
		switch a.Kind {
		case "approve", "decline", "choose", "answer":
			return errors.New("you've already answered this; the answer is on its way to the agent")
		case "snooze":
			return errors.New("its answer is on its way to the agent; dismiss it to take it off the list")
		}
	}
	switch a.Kind {
	case "done", "dismiss":
		// A watchdog condition that still holds would re-open the moment it's
		// dismissed: hide it for a day instead. It closes on its own when the
		// condition clears.
		// The same goes for a machine whose login is still failing.
		if a.Kind == "dismiss" && (it.Source == ingest.SrcWatchdog || it.Source == ingest.SrcFailure) && it.Kind != "rule" {
			return s.st.Snooze(id, time.Now().Add(24*time.Hour).Unix())
		}
		status := map[string]string{"done": "done", "dismiss": "dismissed"}[a.Kind]
		if err := s.st.SetStatus(id, status, a.Text, a.Kind); err != nil {
			return err
		}
		if a.Kind == "done" {
			return s.tellDone(it, a.Text)
		}
		return nil
	case "stash":
		if it.Kind == "rule" {
			return errors.New("approve or reject a rule; it can't be stashed")
		}
		closed, err := s.st.Stash(id)
		if err != nil || !closed {
			return err
		}
		return s.tellDone(it, "")
	case "unstash":
		return s.st.Unstash(id, true)
	case "noise":
		return s.noise(it, strings.TrimSpace(a.Text))
	case "snooze":
		return s.st.Snooze(id, a.Until.Unix())
	case "reopen":
		return s.reopen(it)
	case "reclassify":
		p, u := store.Slug(a.Project), a.Urgency
		if _, ok := urgencyRank[u]; u != "" && !ok {
			return errors.New("urgency must be now, soon or normal")
		}
		if a.Project != "" && p == "" {
			return fmt.Errorf("%q isn't a usable project name: use letters, digits and dashes", a.Project)
		}
		if p == projectName(it) {
			p = "" // where it already is: nothing to move
		}
		if p == store.Unsorted {
			return errors.New(`"unsorted" is where items without a project land; pick a project`)
		}
		if p != "" {
			s.st.EnsureProject(p, "")
		}
		return s.st.Reclassify(id, p, u)
	case "undo":
		sentNoise, sentDone := false, false
		if ds, _ := s.st.ItemDeliveries(id); len(ds) > 0 {
			for _, d := range ds {
				sentNoise = sentNoise || (d.LiveOnly && d.Status != "pending" && strings.Contains(d.Text, "not worth their time"))
				sentDone = sentDone || (d.LiveOnly && d.Status != "pending" && strings.Contains(d.Text, "marked your docket item"))
			}
		}
		action, err := s.st.Undo(id)
		if err != nil {
			return err
		}
		// A rule's approval (or rejection) wrote its row in the rules table;
		// undoing it makes it a proposal again, so agents stop getting it.
		if it.Kind == "rule" && (action == "approve" || action == "decline") { // an edited rule is logged as approve
			if err := s.st.DeleteRule(id); err != nil {
				return err
			}
			s.rulesChanged()
		}
		if action == "noise" {
			s.rulesChanged()
			if sentNoise {
				return s.notifyQuietly(it, fmt.Sprintf("%s took back the \"not worth my time\" on %q: it's open again.", s.cfg.Person, it.Title))
			}
		}
		if (action == "done" || action == "stash") && sentDone {
			if now, _, _ := s.st.GetItem(id); now.Status == "open" {
				return s.notifyQuietly(it, fmt.Sprintf("%s reopened %q: it isn't done after all, and it's open again.", s.cfg.Person, it.Title))
			}
		}
		return nil
	}

	if it.Kind == "rule" {
		return s.decideRule(it, a)
	}

	// Answers that go back to the agent.
	if it.SessionID == "" {
		return errors.New("this item has no session to answer")
	}
	var answer string
	switch a.Kind {
	case "approve":
		answer = "Approved." + comment(a.Text)
	case "decline":
		answer = "Declined: don't do this." + comment(a.Text)
	case "choose":
		if a.Option < 0 || a.Option >= len(it.Options) {
			return errors.New("no such option")
		}
		answer = "Chose: " + it.Options[a.Option] + "." + comment(a.Text)
	case "answer":
		if strings.TrimSpace(a.Text) == "" {
			return errors.New("the answer is empty")
		}
		answer = strings.TrimSpace(a.Text)
	default:
		return fmt.Errorf("unknown action %q", a.Kind)
	}
	if err := s.st.SetAnswer(id, answer, a.Kind); err != nil {
		return err
	}
	return s.queue(it, fmt.Sprintf("%s's answer to %q (from the docket queue): %s", s.cfg.Person, it.Title, answer))
}

func comment(t string) string {
	if t = strings.TrimSpace(t); t != "" {
		return " " + t
	}
	return ""
}

// reopen puts an item back on the docket: off the Stash, and open again if
// it was closed. A close the human made here is undone, with everything it
// set off (a queued "marked done", a noise mark and its note, an expiry, a
// rule's approval); one an agent or docket made is reopened.
func (s *Server) reopen(it store.Item) error {
	stashed, undid := it.StashedAt > 0, false
	// Peel the Stash off the top of the log, one undo at a time, so each
	// restores what it changed: a Remove, a stash that only kept the item, or
	// the stash that closed it (which reopens it, taking back its note).
	for i := 0; i < 50; i++ {
		top, prev := s.st.TopDecision(it.ID)
		if top != "stash" && top != "unstash" {
			break
		}
		if err := s.Decide(it.ID, Action{Kind: "undo"}); err != nil {
			return err
		}
		undid = true
		if top == "stash" && (prev == "open" || prev == "snoozed" || prev == "maybe") {
			break // that stash was the close
		}
	}
	if err := s.st.DropStash(it.ID); err != nil { // anything an older docket left behind
		return err
	}
	var err error
	if it, _, err = s.st.GetItem(it.ID); err != nil {
		return err
	}
	if !closedStatus(it.Status) {
		if stashed || undid {
			return s.st.Touch(it.ID) // back under "Waiting on agents" for a day
		}
		return errors.New("this item is still open")
	}
	switch d := s.st.LastDecision(it.ID); {
	case d == "done", d == "dismiss", d == "noise", d == "expire",
		it.Kind == "rule" && (d == "approve" || d == "decline"):
		return s.Decide(it.ID, Action{Kind: "undo"})
	}
	return s.st.Reopen(it.ID)
}

func closedStatus(st string) bool { return st == "done" || st == "dismissed" || st == "expired" }

// tellDone lets the agent that filed an item know the human marked it done,
// so it doesn't keep waiting on it or file it again. An agent paused on a
// task only the human can do is told straight away (resuming it if need be);
// any other is told quietly, at its next turn, if its session is still
// running. One that already has the human's answer needs nothing more, and
// docket's own items have no agent to tell.
func (s *Server) tellDone(it store.Item, note string) error {
	if it.SessionID == "" || it.Source != ingest.SrcCLI || (it.Status != "open" && it.Status != "snoozed") {
		return nil
	}
	if it.Kind == "human_task" || it.Kind == "blocker" {
		return s.queue(it, fmt.Sprintf("%s marked this done: %q.%s", s.cfg.Person, it.Title, comment(note)))
	}
	var msg string
	switch it.Kind {
	case "review", "fyi":
		msg = fmt.Sprintf("%s marked your docket item %q done: they've looked at it, and nothing more is needed on it.", s.cfg.Person, it.Title)
	case "ask", "approval":
		msg = fmt.Sprintf("%s marked your docket item %q done without choosing an answer. They may have handled it themselves; don't take it as a yes.", s.cfg.Person, it.Title)
	default:
		msg = fmt.Sprintf("%s marked your docket item %q done.", s.cfg.Person, it.Title)
	}
	if n := strings.TrimSpace(note); n != "" {
		msg += " Their note: " + n
	}
	return s.notifyQuietly(it, msg)
}

// noise is the human saying an item wasn't worth their time. It leaves the
// queue, the note goes to the agent that filed it (only if that session is
// still running, and without waking it), and every agent sees it in the
// feedback section of rules.md from then on.
func (s *Server) noise(it store.Item, note string) error {
	if it.Status != "open" && it.Status != "snoozed" && it.Status != "answered" && it.Status != "maybe" {
		return errors.New("this item is already closed")
	}
	res := "Not worth " + s.cfg.Person + "'s time"
	if note != "" {
		res += ": " + note
	}
	if err := s.st.SetStatusWith(it.ID, "dismissed", res, "noise", note); err != nil {
		return err
	}
	s.rulesChanged()
	if it.SessionID == "" || it.Source != ingest.SrcCLI {
		return nil
	}
	msg := fmt.Sprintf("%s marked your docket item %q as not worth their time", s.cfg.Person, it.Title)
	if note != "" {
		msg += ". Their note: " + note
	}
	msg += ". It's closed; don't re-file it. File fewer items like this: keep docket for what really needs " + s.cfg.Person + ", and mention smaller things in chat instead."
	return s.notifyQuietly(it, msg)
}

// notifyQuietly tells the session that filed an item something it should
// know at its next turn, without waking it, and only if it's still running.
func (s *Server) notifyQuietly(it store.Item, msg string) error {
	nonce := randomNonce()
	// A re-filed item keeps its first id; say which id this agent was told.
	if a := s.st.LatestAlias(it.ID, it.SessionID); a != "" {
		msg = "(this is the item you filed as " + a + ") " + msg
	}
	if err := s.st.AddDelivery(store.Delivery{ID: ulid.New(time.Now()), ItemID: it.ID, Machine: it.Machine, SessionID: it.SessionID, Nonce: nonce,
		Text: fmt.Sprintf("[docket %s #%s] %s", it.ID, nonce, msg), LiveOnly: true, Quiet: true}); err != nil {
		return err
	}
	s.kickDeliver()
	return nil
}

// queue creates a delivery of text to the item's session and kicks the deliverer.
func (s *Server) queue(it store.Item, text string) error {
	nonce := randomNonce()
	// A re-filed item keeps its first id; say which id this agent was told.
	if a := s.st.LatestAlias(it.ID, it.SessionID); a != "" {
		text = "(this is the item you filed as " + a + ") " + text
	}
	d := store.Delivery{ID: ulid.New(time.Now()), ItemID: it.ID, Machine: it.Machine, SessionID: it.SessionID, Nonce: nonce,
		Text: fmt.Sprintf("[docket %s #%s] %s", it.ID, nonce, text)}
	if err := s.st.AddDelivery(d); err != nil {
		return err
	}
	s.kickDeliver()
	return nil
}

func randomNonce() string {
	const alphabet = "0123456789abcdefghijkmnpqrstvwxyz"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// deliverAll sends pending deliveries and retries unconfirmed ones.
func (s *Server) deliverAll(ctx context.Context) {
	pending, err := s.st.Deliveries("pending")
	if err != nil {
		s.log.Printf("deliver: %v", err)
		return
	}
	for _, d := range pending {
		s.deliverOne(ctx, d, false)
	}
	sent, _ := s.st.Deliveries("sent")
	for _, d := range sent {
		if time.Since(time.Unix(d.SentAt, 0)) > unconfirmedAfter && d.Attempts < 4 {
			s.deliverOne(ctx, d, true)
		}
	}
}

func (s *Server) deliverOne(ctx context.Context, d store.Delivery, retry bool) {
	m, ok := s.cfg.Machine(d.Machine)
	if !ok {
		if d.LiveOnly {
			s.st.DeliveryDropped(d.ID, "machine "+d.Machine+" isn't in the config")
			return
		}
		s.st.DeliveryFailed(d.ID, "machine "+d.Machine+" isn't in the config")
		return
	}
	sess, _, _ := s.st.GetSession(d.SessionID)
	if m.Mode == "push" {
		return // handed out on the machine's next `docket sync`
	}
	if d.LiveOnly {
		// For a running session's mailbox or nobody: never typed into tmux,
		// never resuming an ended session.
		if sess.State == "ended" || sess.ID == "" || !sess.HasPlugin {
			s.st.DeliveryDropped(d.ID, "the session isn't running")
			return
		}
		s.mailbox(ctx, m, d)
		return
	}
	switch {
	case sess.State == "ended":
		s.resume(ctx, m, sess, d)
	case (!sess.HasPlugin || retry) && sess.Tmux != "":
		// No docket hooks in this session (it started before the plugin was
		// installed), or the hook path didn't confirm: type it into tmux.
		s.paste(ctx, m, sess, d)
	default:
		s.mailbox(ctx, m, d) // idempotent: a resend never delivers twice
	}
}

func (s *Server) mailbox(ctx context.Context, m Machine, d store.Delivery) {
	if m.Mode == "push" {
		return // handed out on the machine's next `docket sync`
	}
	body, _ := json.Marshal(mailbox.Delivery{ID: d.ID, ItemID: d.ItemID, SessionID: d.SessionID, Nonce: d.Nonce, Text: d.Text, Quiet: d.Quiet})
	if _, err := s.remote(ctx, m, strings.NewReader(string(body)), "mailbox", "put"); err != nil {
		s.st.DeliveryFailed(d.ID, err.Error())
		return
	}
	s.st.DeliverySent(d.ID, "mailbox")
}

func (s *Server) paste(ctx context.Context, m Machine, sess store.Session, d store.Delivery) {
	if _, err := s.remote(ctx, m, strings.NewReader(d.Text), "session", "send", "--pane", sess.Tmux); err != nil {
		// Unsafe right now (someone attached, text in the input line, a
		// dialog open): leave it and try again on the next pass.
		s.st.DeliveryFailed(d.ID, err.Error())
		return
	}
	s.st.DeliverySent(d.ID, "tmux")
}

// resume restarts a finished session in tmux with the answer as its prompt.
func (s *Server) resume(ctx context.Context, m Machine, sess store.Session, d store.Delivery) {
	if sess.Kind != "" && sess.Kind != "interactive" && sess.Kind != "bg" {
		s.st.DeliveryFailed(d.ID, "the session has ended and isn't resumable ("+sess.Kind+")")
		return
	}
	if sess.Cwd == "" {
		s.st.DeliveryFailed(d.ID, "the session has ended and its folder is unknown")
		return
	}
	name := sess.Name
	if name == "" {
		name = "docket-" + short(d.SessionID)
	}
	if _, err := s.remote(ctx, m, strings.NewReader(d.Text), "session", "resume", "--id", d.SessionID, "--cwd", sess.Cwd, "--name", name); err != nil {
		s.st.DeliveryFailed(d.ID, err.Error())
		return
	}
	s.st.DeliverySent(d.ID, "resume")
	s.st.PatchSession(d.SessionID, store.SessionPatch{State: store.Str("busy")})
}
