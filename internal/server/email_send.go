package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mail"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// Sending. Only the human sends: an approved draft is held for the Undo
// window ([email] send_hold), then docket builds the message from exactly
// the version that was shown and sends it over the account's SMTP.

// emailOutTick starts draft jobs and sends replies whose Undo window ended.
func (s *Server) emailOutTick(ctx context.Context) {
	if !s.cfg.Email.On() {
		return
	}
	s.followIdeas()
	s.startDrafts(ctx)
	s.sendDue(ctx)
}

// QueueSend holds a reply (kind "reply" or "holding") for the Undo window.
// version must be the draft the human was shown; a newer one means they
// approved something that changed under them, and nothing is sent.
func (s *Server) QueueSend(threadID string, version int, kind string) (store.Outgoing, error) {
	t, ok := s.st.Thread(threadID)
	if !ok {
		return store.Outgoing{}, errors.New("no such email")
	}
	d, ok := s.st.LatestDraft(t.ID)
	if !ok {
		return store.Outgoing{}, errors.New("there's no draft to send yet")
	}
	if d.Version != version {
		return store.Outgoing{}, fmt.Errorf("the draft changed since you opened it (now version %d); look it over again before sending", d.Version)
	}
	if j, ok := s.st.Job(t.ID); ok && (j.Status == "running" || j.Status == "queued") {
		return store.Outgoing{}, errors.New("a new version of the draft is being written; wait for it")
	}
	switch kind {
	case "reply":
		if strings.TrimSpace(mail.Text(d.HTML)) == "" {
			return store.Outgoing{}, errors.New("the draft is empty")
		}
	case "holding":
		if strings.TrimSpace(mail.Text(d.Holding)) == "" {
			return store.Outgoing{}, errors.New("there's no holding reply")
		}
	default:
		return store.Outgoing{}, errors.New("unknown kind of reply")
	}
	if len(d.To) == 0 {
		return store.Outgoing{}, errors.New("the draft has no one to send it to")
	}
	o := store.Outgoing{ID: ulid.New(time.Now()), ThreadID: t.ID, ItemID: t.ItemID, Version: d.Version, Kind: kind,
		HoldUntil: time.Now().Add(s.cfg.Email.SendHold.Duration).UnixMilli()}
	if err := s.st.HoldSend(o); err != nil {
		return store.Outgoing{}, err
	}
	time.AfterFunc(s.cfg.Email.SendHold.Duration+200*time.Millisecond, func() { kick(s.emailOutKick) })
	s.bumpEmail()
	return o, nil
}

// sendDue sends every held reply whose Undo window has passed. A send left
// half-done by a crash is never retried (it may have gone out): it's marked
// failed so the human can check.
func (s *Server) sendDue(ctx context.Context) {
	if stuck, err := s.st.StuckSends(time.Now().Add(-10 * time.Minute).UnixMilli()); err == nil {
		for _, o := range stuck {
			s.st.EndSend(o.ID, "failed", "", "docket stopped while sending; check your Sent folder before trying again")
			s.sendFailed(o, errors.New("docket stopped while sending; check your Sent folder before trying again"))
		}
	}
	due, err := s.st.DueSends(time.Now().UnixMilli())
	if err != nil {
		s.log.Printf("email send: %v", err)
		return
	}
	for _, o := range due {
		msgID, err := s.sendOne(ctx, o)
		if err != nil {
			s.st.EndSend(o.ID, "failed", "", clipErr(err.Error()))
			s.sendFailed(o, err)
			s.log.Printf("email send %s: %v", o.ThreadID, err)
			continue
		}
		s.st.EndSend(o.ID, "sent", msgID, "")
		s.sendDone(o)
	}
	if len(due) > 0 {
		s.bumpEmail()
	}
}

// builtReply builds the message for a draft version: the cleaned HTML with
// docket's styling, the account's signature and the quoted original.
func (s *Server) builtReply(t store.EmailThread, d store.Draft, kind string, sig mail.Signature) (mail.Built, mail.Account, error) {
	a, ok := s.cfg.Email.Account(t.Account)
	if !ok {
		return mail.Built{}, a, errors.New("the account isn't configured any more")
	}
	orig, err := s.st.EmailMessageByID(t.LastInbound)
	if err != nil {
		return mail.Built{}, a, errors.New("the original message is gone")
	}
	body := d.HTML
	if kind == "holding" {
		body = d.Holding
	}
	q := &mail.Quote{From: orig.From.String(), Sent: time.Unix(orig.Date, 0).In(s.cfg.loc).Format("Monday, January 2, 2006 3:04 PM"),
		To: joinDisplay(orig.To), Cc: joinDisplay(orig.Cc), Subject: orig.Subject, Text: orig.Text}
	if orig.Pruned {
		q = nil
	}
	b, err := a.Build(mail.Outgoing{To: d.To, Cc: d.Cc, Subject: d.Subject, HTML: body, InReplyTo: orig.MessageID, References: orig.References, Quote: q}, sig, time.Now().In(s.cfg.loc))
	return b, a, err
}

func joinDisplay(list []mail.Addr) string {
	var out []string
	for _, a := range list {
		out = append(out, a.String())
	}
	return strings.Join(out, ", ")
}

func (s *Server) sendOne(ctx context.Context, o store.Outgoing) (string, error) {
	t, ok := s.st.Thread(o.ThreadID)
	if !ok {
		return "", errors.New("the thread is gone")
	}
	d, ok := s.st.DraftVersion(t.ID, o.Version)
	if !ok {
		return "", errors.New("the approved draft is gone")
	}
	a, _ := s.cfg.Email.Account(t.Account)
	sig, err := a.LoadSignature(false)
	if err != nil {
		return "", err
	}
	b, a, err := s.builtReply(t, d, o.Kind, sig)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := a.Send(ctx, b); err != nil {
		return "", err
	}
	return b.MessageID, nil
}

// sendDone closes the reply item (a holding reply keeps it open: the real
// answer is still to come) and marks the thread answered.
func (s *Server) sendDone(o store.Outgoing) {
	t, ok := s.st.Thread(o.ThreadID)
	if !ok {
		return
	}
	a, _ := s.cfg.Email.Account(t.Account)
	s.st.MarkThreadReplied(t.ID, time.Now().Unix())
	if o.Kind == "holding" {
		s.st.AppendToItem(t.ItemID, "_Sent a holding reply "+time.Now().In(s.cfg.loc).Format("Mon 15:04")+". The full reply is still to come._")
		s.st.TouchItem(t.ItemID)
		return
	}
	if it, ok, _ := s.st.GetItem(t.ItemID); ok && (it.Status == "open" || it.Status == "snoozed" || it.Status == "answered") {
		s.st.SetStatusWith(it.ID, "done", "Sent from your "+a.Label+" mail", "sent", "")
	}
}

// sendFailed puts the failure on the item, which stays open.
func (s *Server) sendFailed(o store.Outgoing, err error) {
	t, ok := s.st.Thread(o.ThreadID)
	if !ok {
		return
	}
	s.st.TouchItem(t.ItemID)
}

// UndoSend withdraws a held reply.
func (s *Server) UndoSend(threadID, outID string) error {
	o, ok := s.st.PendingSend(threadID)
	if !ok || o.ID != outID {
		return errors.New("nothing to undo")
	}
	if err := s.st.UndoSend(outID); err != nil {
		return err
	}
	s.bumpEmail()
	return nil
}
