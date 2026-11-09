package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// How long a sent delivery may go unconfirmed before it is resent or retried
// another way.
const unconfirmedAfter = 3 * time.Minute

// Action is a decision the human made on an item in the queue.
type Action struct {
	Kind   string // approve | decline | choose | answer | done | dismiss | snooze | promote | undo
	Text   string // optional comment / free-form answer
	Option int    // for choose: index into item.Options
	Until  time.Time
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
	switch a.Kind {
	case "done", "dismiss":
		status := map[string]string{"done": "done", "dismiss": "dismissed"}[a.Kind]
		if err := s.st.SetStatus(id, status, a.Text, a.Kind); err != nil {
			return err
		}
		// A human-only task the agent is waiting on: tell it the task is done.
		if a.Kind == "done" && (it.Kind == "human_task" || it.Kind == "blocker") && it.SessionID != "" {
			return s.queue(it, fmt.Sprintf("%s marked this done: %q.%s", s.cfg.Person, it.Title, comment(a.Text)))
		}
		return nil
	case "snooze":
		return s.st.Snooze(id, a.Until.Unix())
	case "promote":
		return s.st.SetStatus(id, "open", "", "promote")
	case "undo":
		_, err := s.st.Undo(id)
		return err
	}

	// Answers that go back to the agent.
	if it.SessionID == "" {
		return errors.New("this item has no session to answer")
	}
	var answer string
	switch a.Kind {
	case "approve":
		if s.physical(it) {
			return errors.New("physical actions can't be approved from the queue; do it in person and mark it done")
		}
		answer = "Approved." + comment(a.Text)
	case "decline":
		answer = "Declined: don't do this." + comment(a.Text)
	case "choose":
		if a.Option < 0 || a.Option >= len(it.Options) {
			return errors.New("no such option")
		}
		if s.physical(it) && !looksNegative(it.Options[a.Option]) {
			return errors.New("physical actions can't be approved from the queue; do it in person and mark it done")
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

var negative = regexp.MustCompile(`(?i)^\s*(no|don'?t|do not|wait|stop|cancel|skip|hold|not now|later)\b`)

func looksNegative(s string) bool { return negative.MatchString(s) }

// physical reports whether an item asks for a physical action that must not
// be approved remotely (robot motion and the like).
func (s *Server) physical(it store.Item) bool {
	if it.Physical {
		return true
	}
	m, ok := s.cfg.Machine(it.Machine)
	if !ok {
		return false
	}
	text := strings.ToLower(it.Title + " " + it.Body)
	for _, k := range m.Physical {
		if k != "" && strings.Contains(text, strings.ToLower(k)) {
			return true
		}
	}
	return false
}

// queue creates a delivery of text to the item's session and kicks the deliverer.
func (s *Server) queue(it store.Item, text string) error {
	nonce := randomNonce()
	if m, ok := s.cfg.Machine(it.Machine); ok && len(m.Physical) > 0 {
		text += " This does not authorize any physical or robot motion."
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
		s.st.DeliveryFailed(d.ID, "machine "+d.Machine+" isn't in the config")
		return
	}
	sess, _, _ := s.st.GetSession(d.SessionID)
	if m.Mode == "push" {
		return // handed out on the machine's next `docket sync`
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
	body, _ := json.Marshal(mailbox.Delivery{ID: d.ID, ItemID: d.ItemID, SessionID: d.SessionID, Nonce: d.Nonce, Text: d.Text})
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
		name = "docket-" + d.SessionID[:8]
	}
	if _, err := s.remote(ctx, m, strings.NewReader(d.Text), "session", "resume", "--id", d.SessionID, "--cwd", sess.Cwd, "--name", name); err != nil {
		s.st.DeliveryFailed(d.ID, err.Error())
		return
	}
	s.st.DeliverySent(d.ID, "resume")
	s.st.PatchSession(d.SessionID, store.SessionPatch{State: store.Str("busy")})
}
