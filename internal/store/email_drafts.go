package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mail"
)

// Drafts, the jobs that write them, and the outbox that sends them.
const draftSchema = `
CREATE TABLE IF NOT EXISTS email_drafts (
  thread_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  subject TEXT NOT NULL DEFAULT '',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  html TEXT NOT NULL DEFAULT '',
  holding TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  questions TEXT NOT NULL DEFAULT '[]',
  work TEXT NOT NULL DEFAULT '',
  removed TEXT NOT NULL DEFAULT '[]',
  source TEXT NOT NULL DEFAULT '',
  instruction TEXT NOT NULL DEFAULT '',
  basis INTEGER NOT NULL DEFAULT 0,
  hand_recipients INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (thread_id, version)
);
CREATE TABLE IF NOT EXISTS email_jobs (
  thread_id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'draft',
  instruction TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  queued_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  ended_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS email_jobs_status ON email_jobs(status, queued_at);
CREATE TABLE IF NOT EXISTS email_outbox (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL,
  item_id TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL,
  kind TEXT NOT NULL,
  answers TEXT NOT NULL DEFAULT '',
  hold_until INTEGER NOT NULL,
  status TEXT NOT NULL,
  message_id TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  sent_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS email_outbox_status ON email_outbox(status, hold_until);
`

// Work is work an email asks for, as the drafting agent found it.
type Work struct {
	Needed  bool   `json:"needed"`
	Title   string `json:"title"`
	Plan    string `json:"plan"`
	Why     string `json:"why"`
	Project string `json:"project"`
}

// Draft is one version of a reply.
type Draft struct {
	ThreadID    string
	Version     int
	Subject     string
	To          []mail.Addr
	Cc          []mail.Addr
	HTML        string // cleaned (mail.Clean)
	Holding     string // a cleaned holding reply, when work is proposed
	Notes       string // what the agent checked and found, for you
	Questions   []string
	Work        Work
	Removed     []string // what mail.Clean took out of the agent's HTML
	Source      string   // agent | revision | edit
	Instruction string   // your words that asked for this version
	Basis       int64    // the last stored message it saw (email_messages.seq); later ones aren't in it
	// HandRecipients: you changed To/Cc in an edit of this reply, so later
	// versions of it keep them as you set them.
	HandRecipients bool
	CreatedAt      int64
}

const draftCols = `thread_id, version, subject, to_json, cc_json, html, holding, notes, questions, work, removed, source, instruction, basis, hand_recipients, created_at`

func scanDraft(r interface{ Scan(...any) error }) (Draft, error) {
	var d Draft
	var to, cc, qs, work, removed string
	err := r.Scan(&d.ThreadID, &d.Version, &d.Subject, &to, &cc, &d.HTML, &d.Holding, &d.Notes, &qs, &work, &removed, &d.Source, &d.Instruction, &d.Basis, &d.HandRecipients, &d.CreatedAt)
	json.Unmarshal([]byte(to), &d.To)
	json.Unmarshal([]byte(cc), &d.Cc)
	json.Unmarshal([]byte(qs), &d.Questions)
	json.Unmarshal([]byte(removed), &d.Removed)
	if work != "" {
		json.Unmarshal([]byte(work), &d.Work)
	}
	return d, err
}

// AddDraft stores a new version and returns its number.
func (s *Store) AddDraft(d Draft) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	v, err := addDraft(tx, d)
	if err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// AddEdit saves your edit of version `of` as the next version, unless the
// draft moved on or a new version is being written. Both are checked in
// the transaction that writes it, so no job can start in between and then
// overwrite your edit with a version written without it.
func (s *Store) AddEdit(d Draft, of int) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var latest int
	tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM email_drafts WHERE thread_id=?`, d.ThreadID).Scan(&latest)
	if latest != of {
		return 0, fmt.Errorf("the draft changed while you were editing (now version %d); copy your text, reopen it and edit again", latest)
	}
	var busy int
	tx.QueryRow(`SELECT COUNT(*) FROM email_jobs WHERE thread_id=? AND status IN ('queued','running')`, d.ThreadID).Scan(&busy)
	if busy > 0 {
		return 0, errors.New("a new version is being written; wait for it, then edit")
	}
	v, err := addDraft(tx, d)
	if err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

func addDraft(tx *sql.Tx, d Draft) (int, error) {
	var v int
	tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM email_drafts WHERE thread_id=?`, d.ThreadID).Scan(&v)
	v++
	work := ""
	if d.Work.Needed {
		b, _ := json.Marshal(d.Work)
		work = string(b)
	}
	qs, _ := json.Marshal(d.Questions)
	if d.Questions == nil {
		qs = []byte("[]")
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO email_drafts(`+draftCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ThreadID, v, d.Subject, js(d.To), js(d.Cc), d.HTML, d.Holding, d.Notes, string(qs), work, js(d.Removed), d.Source, d.Instruction, d.Basis, d.HandRecipients, now); err != nil {
		return 0, err
	}
	return v, nil
}

// LatestDraft is a thread's newest draft.
func (s *Store) LatestDraft(threadID string) (Draft, bool) {
	d, err := scanDraft(s.DB.QueryRow(`SELECT `+draftCols+` FROM email_drafts WHERE thread_id=? ORDER BY version DESC LIMIT 1`, threadID))
	return d, err == nil
}

// DraftVersion loads one version.
func (s *Store) DraftVersion(threadID string, v int) (Draft, bool) {
	d, err := scanDraft(s.DB.QueryRow(`SELECT `+draftCols+` FROM email_drafts WHERE thread_id=? AND version=?`, threadID, v))
	return d, err == nil
}

// EmailJob is a draft being written or revised.
type EmailJob struct {
	ThreadID    string
	Status      string // queued | running | failed | done
	Kind        string // draft | revise
	Instruction string
	Session     string // the drafting session, resumed for revisions
	Attempts    int
	Error       string
	QueuedAt    int64
	StartedAt   int64
	EndedAt     int64
}

const emailJobCols = `thread_id, status, kind, instruction, session, attempts, error, queued_at, started_at, ended_at`

func scanEmailJob(r interface{ Scan(...any) error }) (EmailJob, error) {
	var j EmailJob
	err := r.Scan(&j.ThreadID, &j.Status, &j.Kind, &j.Instruction, &j.Session, &j.Attempts, &j.Error, &j.QueuedAt, &j.StartedAt, &j.EndedAt)
	return j, err
}

// QueueDraft asks for a draft (kind "draft") or a revision ("revise", with
// your instruction). A thread has one job at a time; queueing over a
// running job is refused.
func (s *Store) QueueDraft(threadID, kind, instruction string) error {
	now := time.Now().Unix()
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := scanEmailJob(tx.QueryRow(`SELECT `+emailJobCols+` FROM email_jobs WHERE thread_id=?`, threadID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO email_jobs(thread_id, status, kind, instruction, queued_at) VALUES (?,?,?,?,?)`, threadID, "queued", kind, instruction, now)
	case err != nil:
		return err
	case j.Status == "running":
		return errors.New("a draft is being written; try again when it's ready")
	case j.Status == "queued":
		// Still waiting: add the new request to it, so neither is lost. A
		// revision stays one (it builds on the latest version and sees the
		// whole thread), and the asks are combined.
		if kind == "revise" {
			j.Kind = "revise"
		}
		_, err = tx.Exec(`UPDATE email_jobs SET kind=?, instruction=? WHERE thread_id=?`, j.Kind, CombineAsks(j.Instruction, instruction), threadID)
	case j.Status == "failed" && failedAskHolds(tx, threadID, j.EndedAt):
		// It failed, so what it was asked never made it into a version: the
		// new request carries it too (a Redraft after a failed change keeps
		// the change), while it's still the same reply.
		if j.Kind == "revise" {
			kind = "revise"
		}
		_, err = tx.Exec(`UPDATE email_jobs SET status='queued', kind=?, instruction=?, attempts=0, error='', queued_at=?, started_at=0, ended_at=0 WHERE thread_id=?`,
			kind, CombineAsks(j.Instruction, instruction), now, threadID)
	default:
		_, err = tx.Exec(`UPDATE email_jobs SET status='queued', kind=?, instruction=?, attempts=0, error='', queued_at=?, started_at=0, ended_at=0 WHERE thread_id=?`,
			kind, instruction, now, threadID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// askSep joins asks combined into one: several made while a job waits,
// or carried from version to version within a reply.
const askSep = "\n\nAlso: "

// WorkNote starts docket's own note about the work an email asked for
// (built, saved for later, dropped). Only the newest one holds, so where
// asks are combined it replaces an earlier one.
const WorkNote = "About the work this email asked for: "

// CombineAsks adds the asks in ask to those already combined in have.
// Asks are compared whole, so a shorter one inside a longer one ("mention
// the price" in "Don't mention the price") is still added, and only an
// exact repeat is dropped (asked twice: said once). docket's newest note
// about the work replaces its earlier one.
func CombineAsks(have, ask string) string {
	parts := Asks(have)
	for _, a := range Asks(ask) {
		if strings.HasPrefix(a, WorkNote) {
			parts = slices.DeleteFunc(parts, func(p string) bool { return strings.HasPrefix(p, WorkNote) })
		}
		if !slices.Contains(parts, a) {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, askSep)
}

// Asks splits combined asks into each one, in the order they were made.
func Asks(s string) []string {
	var out []string
	for _, a := range strings.Split(s, askSep) {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// IsWorkNote reports docket's own note about the requested work.
func IsWorkNote(ask string) bool { return strings.HasPrefix(ask, WorkNote) }

// YouAsked reports changes you asked for in this version's line (more
// than docket's own note about the work).
func (d Draft) YouAsked() bool {
	return slices.ContainsFunc(Asks(d.Instruction), func(a string) bool { return !strings.HasPrefix(a, WorkNote) })
}

// failedAskHolds reports whether what a job that failed at `failed` was
// asked still applies: you haven't saved your own edit since (it replaces
// the ask; while a job is failed nothing else can write a version, so a
// version from the same second is the one it was asked on), and you
// haven't replied since the latest version (a reply ends the reply it was
// asked for).
func failedAskHolds(tx *sql.Tx, threadID string, failed int64) bool {
	var d Draft
	switch err := tx.QueryRow(`SELECT version, basis, source, created_at FROM email_drafts WHERE thread_id=? ORDER BY version DESC LIMIT 1`, threadID).Scan(&d.Version, &d.Basis, &d.Source, &d.CreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return true
	case err != nil:
		return false
	}
	return !(d.Source == "edit" && d.CreatedAt >= failed) && !repliedAfter(tx, threadID, d)
}

// Job loads a thread's job.
func (s *Store) Job(threadID string) (EmailJob, bool) {
	j, err := scanEmailJob(s.DB.QueryRow(`SELECT `+emailJobCols+` FROM email_jobs WHERE thread_id=?`, threadID))
	return j, err == nil
}

// NextJobs claims up to n queued jobs, oldest first, marking them running.
func (s *Store) NextJobs(n int) ([]EmailJob, error) {
	if n <= 0 {
		return nil, nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+emailJobCols+` FROM email_jobs WHERE status='queued' ORDER BY queued_at LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	var out []EmailJob
	for rows.Next() {
		j, err := scanEmailJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	rows.Close()
	now := time.Now().Unix()
	for i := range out {
		out[i].Status, out[i].StartedAt = "running", now
		out[i].Attempts++
		if _, err := tx.Exec(`UPDATE email_jobs SET status='running', started_at=?, attempts=attempts+1 WHERE thread_id=?`, now, out[i].ThreadID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// RunningJobs counts jobs being written now.
func (s *Store) RunningJobs() int {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM email_jobs WHERE status='running'`).Scan(&n)
	return n
}

// EndJob records how a job ended, and the session to resume next time.
func (s *Store) EndJob(threadID, status, session, errMsg string) error {
	_, err := s.DB.Exec(`UPDATE email_jobs SET status=?, session=CASE WHEN ?<>'' THEN ? ELSE session END, error=?, ended_at=? WHERE thread_id=?`,
		status, session, session, errMsg, time.Now().Unix(), threadID)
	return err
}

// RequeueStale puts jobs back in the queue that were running when the
// server stopped (started before the server did).
func (s *Store) RequeueStale(startedBefore int64) {
	s.DB.Exec(`UPDATE email_jobs SET status='queued' WHERE status='running' AND started_at<?`, startedBefore)
}

// Outgoing is a reply on its way out: held for the Undo window, then sent.
type Outgoing struct {
	ID        string
	ThreadID  string
	ItemID    string
	Version   int
	Kind      string // reply | holding
	Answers   string // the message it replies to (row id), when it was approved
	HoldUntil int64  // unix ms
	Status    string // held | sending | sent | failed | undone
	MessageID string
	Error     string
	CreatedAt int64
	SentAt    int64
}

const outCols = `id, thread_id, item_id, version, kind, answers, hold_until, status, message_id, error, created_at, sent_at`

func scanOut(r interface{ Scan(...any) error }) (Outgoing, error) {
	var o Outgoing
	err := r.Scan(&o.ID, &o.ThreadID, &o.ItemID, &o.Version, &o.Kind, &o.Answers, &o.HoldUntil, &o.Status, &o.MessageID, &o.Error, &o.CreatedAt, &o.SentAt)
	return o, err
}

// HoldSend queues a send, held until holdUntil (unix ms). A thread can't
// have two sends waiting.
func (s *Store) HoldSend(o Outgoing) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM email_outbox WHERE thread_id=? AND status IN ('held','sending')`, o.ThreadID).Scan(&n)
	if n > 0 {
		return errors.New("a reply to this email is already on its way")
	}
	tx.QueryRow(`SELECT COUNT(*) FROM email_outbox WHERE thread_id=? AND version=? AND kind=? AND status='sent'`, o.ThreadID, o.Version, o.Kind).Scan(&n)
	if n > 0 {
		return errors.New("this version was already sent")
	}
	if _, err := tx.Exec(`INSERT INTO email_outbox(`+outCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.ThreadID, o.ItemID, o.Version, o.Kind, o.Answers, o.HoldUntil, "held", "", "", time.Now().Unix(), 0); err != nil {
		return err
	}
	return tx.Commit()
}

// UndoSend withdraws a held send. It fails once the send has started.
func (s *Store) UndoSend(id string) error {
	r, err := s.DB.Exec(`UPDATE email_outbox SET status='undone' WHERE id=? AND status='held'`, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errors.New("too late: it has already gone out")
	}
	return nil
}

// DueSends claims held sends whose Undo window has passed.
func (s *Store) DueSends(nowMs int64) ([]Outgoing, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+outCols+` FROM email_outbox WHERE status='held' AND hold_until<=? ORDER BY hold_until`, nowMs)
	if err != nil {
		return nil, err
	}
	var out []Outgoing
	for rows.Next() {
		o, err := scanOut(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, o)
	}
	rows.Close()
	for i := range out {
		out[i].Status = "sending"
		if _, err := tx.Exec(`UPDATE email_outbox SET status='sending' WHERE id=?`, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// EndSend records a send's result.
func (s *Store) EndSend(id, status, messageID, errMsg string) error {
	sent := int64(0)
	if status == "sent" {
		sent = time.Now().Unix()
	}
	_, err := s.DB.Exec(`UPDATE email_outbox SET status=?, message_id=?, error=?, sent_at=? WHERE id=?`, status, messageID, errMsg, sent, id)
	return err
}

// PendingSend is the thread's send waiting in its Undo window, if any.
func (s *Store) PendingSend(threadID string) (Outgoing, bool) {
	o, err := scanOut(s.DB.QueryRow(`SELECT `+outCols+` FROM email_outbox WHERE thread_id=? AND status IN ('held','sending') ORDER BY created_at DESC LIMIT 1`, threadID))
	return o, err == nil
}

// LastSend is the thread's most recent send attempt.
func (s *Store) LastSend(threadID string) (Outgoing, bool) {
	o, err := scanOut(s.DB.QueryRow(`SELECT `+outCols+` FROM email_outbox WHERE thread_id=? ORDER BY created_at DESC LIMIT 1`, threadID))
	return o, err == nil
}

// StuckSends finds sends left "sending" by a crash. They may or may not
// have gone out, so they are marked failed for the human to check, never
// sent twice.
func (s *Store) StuckSends(before int64) ([]Outgoing, error) {
	rows, err := s.DB.Query(`SELECT `+outCols+` FROM email_outbox WHERE status='sending' AND hold_until<?`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outgoing
	for rows.Next() {
		o, err := scanOut(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// TouchItem marks an item changed (its row shows "updated", open pages
// refresh).
func (s *Store) TouchItem(id string) {
	if id != "" {
		s.DB.Exec(`UPDATE items SET updated_at=? WHERE id=?`, now(), id)
	}
}

// MarkThreadReplied records that you replied (from docket) at a time.
func (s *Store) MarkThreadReplied(id string, at int64) {
	s.DB.Exec(`UPDATE email_threads SET replied_at=MAX(replied_at, ?), needs_triage=0, updated_at=? WHERE id=?`, at, time.Now().Unix(), id)
}

// SentKinds reports which kinds of reply (reply, holding) went out for a
// draft version, and when the latest did.
func (s *Store) SentKinds(threadID string, version int) (kinds map[string]bool, at int64) {
	kinds = map[string]bool{}
	rows, err := s.DB.Query(`SELECT kind, sent_at FROM email_outbox WHERE thread_id=? AND version=? AND status='sent'`, threadID, version)
	if err != nil {
		return kinds, 0
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var t int64
		rows.Scan(&k, &t)
		kinds[k] = true
		if t > at {
			at = t
		}
	}
	return kinds, at
}

// SentByDocketAsHolding reports a message docket sent as a holding reply:
// it doesn't answer the email (the real reply is still to come).
func (s *Store) SentByDocketAsHolding(messageID string) bool {
	if messageID == "" {
		return false
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM email_outbox WHERE kind='holding' AND status='sent' AND message_id=?`, messageID).Scan(&n)
	return n > 0
}

// WroteSince reports a message to you, in one of the inbound folders,
// stored after the given one (email_messages.seq, so by arrival order:
// no clock or sender-written Date involved). A copy of a message already
// seen (another folder, a renumbered folder) doesn't count.
func (s *Store) WroteSince(account, thread string, sinceRow int64, inbound []string) bool {
	if len(inbound) == 0 {
		return false
	}
	q := `SELECT COUNT(*) FROM email_messages m WHERE m.account=? AND m.thread=? AND m.from_me=0 AND m.seq>? AND m.folder IN (?` +
		strings.Repeat(",?", len(inbound)-1) + `) AND (m.message_id='' OR NOT EXISTS (SELECT 1 FROM email_messages o WHERE o.account=m.account
		AND o.message_id=m.message_id AND o.seq<=?))`
	args := []any{account, thread, sinceRow}
	for _, f := range inbound {
		args = append(args, f)
	}
	args = append(args, sinceRow)
	var n int
	s.DB.QueryRow(q, args...).Scan(&n)
	return n > 0
}

// IsForward reports a forward ("Fwd:", "FW:"): passing an email on to
// someone isn't answering it.
func IsForward(subject string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	return strings.HasPrefix(s, "fwd:") || strings.HasPrefix(s, "fw:")
}

// notForward is IsForward for message m, in SQL.
const notForward = `NOT (LOWER(LTRIM(m.subject)) LIKE 'fwd:%' OR LOWER(LTRIM(m.subject)) LIKE 'fw:%')`

// RepliedSince reports a message you wrote outside docket (your own mail
// app; any folder) in the thread, first stored after the given one: you've
// replied since. A copy of a message stored before (another folder, a
// label) isn't new, and docket's own sends never count here: the outbox
// says which version went out, and a holding reply isn't an answer.
func (s *Store) RepliedSince(account, thread string, sinceSeq int64) bool {
	return repliedSince(s.DB, account, thread, sinceSeq)
}

// querier is a database or a transaction.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func repliedSince(q querier, account, thread string, sinceSeq int64) bool {
	var n int
	q.QueryRow(`SELECT COUNT(*) FROM email_messages m WHERE m.account=? AND m.thread=? AND m.from_me=1 AND m.seq>? AND `+notForward+`
		AND (m.message_id='' OR (NOT EXISTS (SELECT 1 FROM email_outbox o WHERE o.status='sent' AND o.message_id=m.message_id)
			AND NOT EXISTS (SELECT 1 FROM email_messages c WHERE c.account=m.account AND c.message_id=m.message_id AND c.seq<=?)))`,
		account, thread, sinceSeq, sinceSeq).Scan(&n)
	return n > 0
}

// RepliedAfter reports a full reply of yours after version d of a
// thread's draft: d itself went out from docket, or you wrote one outside
// docket since d was written (RepliedSince). It ends the reply d is in.
func (s *Store) RepliedAfter(threadID string, d Draft) bool { return repliedAfter(s.DB, threadID, d) }

func repliedAfter(q querier, threadID string, d Draft) bool {
	var sent int
	q.QueryRow(`SELECT COUNT(*) FROM email_outbox WHERE thread_id=? AND version=? AND kind='reply' AND status='sent'`, threadID, d.Version).Scan(&sent)
	if sent > 0 {
		return true
	}
	var account, thread string
	if q.QueryRow(`SELECT account, thread FROM email_threads WHERE id=?`, threadID).Scan(&account, &thread) != nil {
		return false
	}
	return repliedSince(q, account, thread, d.Basis)
}

// LastMessageRow is the newest stored message (its seq): what a draft
// written now has seen.
func (s *Store) LastMessageRow() int64 {
	var n int64
	s.DB.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM email_messages`).Scan(&n)
	return n
}
