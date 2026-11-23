package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mail"
)

// emailSchema holds docket's copy of the mail it triages. Bodies are kept
// for [email] keep_days (PruneEmail), then only headers stay, for threading.
const emailSchema = `
CREATE TABLE IF NOT EXISTS email_accounts (
  name TEXT PRIMARY KEY,
  sent_folder TEXT NOT NULL DEFAULT '',
  drafts_folder TEXT NOT NULL DEFAULT '',
  ok_at INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  error_at INTEGER NOT NULL DEFAULT 0,
  contacts_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS email_cursors (
  account TEXT NOT NULL,
  folder TEXT NOT NULL,
  uidvalidity INTEGER NOT NULL DEFAULT 0,
  last_uid INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account, folder)
);
CREATE TABLE IF NOT EXISTS email_messages (
  id TEXT PRIMARY KEY,
  account TEXT NOT NULL,
  folder TEXT NOT NULL,
  uid INTEGER NOT NULL,
  uidvalidity INTEGER NOT NULL,
  message_id TEXT NOT NULL DEFAULT '',
  thread TEXT NOT NULL,
  in_reply_to TEXT NOT NULL DEFAULT '',
  refs TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  from_addr TEXT NOT NULL DEFAULT '',
  reply_to TEXT NOT NULL DEFAULT '[]',
  to_json TEXT NOT NULL DEFAULT '[]',
  cc_json TEXT NOT NULL DEFAULT '[]',
  subject TEXT NOT NULL DEFAULT '',
  date INTEGER NOT NULL DEFAULT 0,
  text TEXT NOT NULL DEFAULT '',
  html TEXT NOT NULL DEFAULT '',
  attachments TEXT NOT NULL DEFAULT '[]',
  list_id TEXT NOT NULL DEFAULT '',
  bulk INTEGER NOT NULL DEFAULT 0,
  from_me INTEGER NOT NULL DEFAULT 0,
  fetched_at INTEGER NOT NULL,
  pruned INTEGER NOT NULL DEFAULT 0,
  seq INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS email_messages_mid ON email_messages(account, message_id);
CREATE INDEX IF NOT EXISTS email_messages_thread ON email_messages(account, thread, date);
CREATE TABLE IF NOT EXISTS email_threads (
  id TEXT PRIMARY KEY,
  account TEXT NOT NULL,
  thread TEXT NOT NULL,
  subject TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  from_addr TEXT NOT NULL DEFAULT '',
  last_inbound TEXT NOT NULL DEFAULT '',
  inbound_at INTEGER NOT NULL DEFAULT 0,
  replied_at INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL DEFAULT '',
  line TEXT NOT NULL DEFAULT '',
  why TEXT NOT NULL DEFAULT '',
  project TEXT NOT NULL DEFAULT '',
  work TEXT NOT NULL DEFAULT '',
  first_time INTEGER NOT NULL DEFAULT 0,
  needs_triage INTEGER NOT NULL DEFAULT 0,
  triaged_at INTEGER NOT NULL DEFAULT 0,
  pass_id TEXT NOT NULL DEFAULT '',
  item_id TEXT NOT NULL DEFAULT '',
  seen_at INTEGER NOT NULL DEFAULT 0,
  read_at INTEGER NOT NULL DEFAULT 0,
  until_at INTEGER NOT NULL DEFAULT 0,
  idea_id TEXT NOT NULL DEFAULT '',
  idea_status TEXT NOT NULL DEFAULT '',
  triage_fails INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE (account, thread)
);
CREATE INDEX IF NOT EXISTS email_threads_todo ON email_threads(needs_triage);
CREATE INDEX IF NOT EXISTS email_threads_verdict ON email_threads(verdict, until_at);
CREATE INDEX IF NOT EXISTS email_threads_item ON email_threads(item_id);
CREATE TABLE IF NOT EXISTS email_passes (
  id TEXT PRIMARY KEY,
  trigger TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  ended_at INTEGER NOT NULL DEFAULT 0,
  read INTEGER NOT NULL DEFAULT 0,
  triaged INTEGER NOT NULL DEFAULT 0,
  reply INTEGER NOT NULL DEFAULT 0,
  digest INTEGER NOT NULL DEFAULT 0,
  skip INTEGER NOT NULL DEFAULT 0,
  answered INTEGER NOT NULL DEFAULT 0,
  cost REAL NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS email_feedback (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  thread_id TEXT NOT NULL,
  was TEXT NOT NULL,
  should TEXT NOT NULL,
  line TEXT NOT NULL DEFAULT '',
  from_addr TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS email_contacts (
  account TEXT NOT NULL,
  address TEXT NOT NULL,
  sent INTEGER NOT NULL DEFAULT 0,
  last_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account, address)
);
`

// EmailMessage is one stored message.
type EmailMessage struct {
	ID          string
	Account     string
	Folder      string
	UID         uint32
	UIDValidity uint32
	MessageID   string
	Thread      string
	InReplyTo   []string
	References  []string
	From        mail.Addr
	ReplyTo     []mail.Addr
	To          []mail.Addr
	Cc          []mail.Addr
	Subject     string
	Date        int64
	Text        string
	HTML        string
	Attachments []mail.Attachment
	ListID      string
	Bulk        bool
	FromMe      bool
	FetchedAt   int64
	Pruned      bool
}

// EmailAccount is what docket knows about a mailbox's state.
type EmailAccount struct {
	Name         string
	SentFolder   string
	DraftsFolder string
	OKAt         int64
	Error        string
	ErrorAt      int64
	ContactsAt   int64
}

func messageRowID(account, folder string, validity, uid uint32) string {
	return fmt.Sprintf("%s/%s/%d/%d", account, folder, validity, uid)
}

// AddEmail stores fetched messages, giving each a thread, and returns the
// ones it hadn't seen. me reports your own addresses.
func (s *Store) AddEmail(account string, msgs []*mail.Message, me func(string) bool) ([]EmailMessage, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var added []EmailMessage
	for _, m := range msgs {
		id := messageRowID(account, m.Folder, m.UIDValidity, m.UID)
		var have int
		tx.QueryRow(`SELECT COUNT(*) FROM email_messages WHERE id=?`, id).Scan(&have)
		if have > 0 {
			continue
		}
		thread := threadOf(tx, account, m, id)
		e := EmailMessage{ID: id, Account: account, Folder: m.Folder, UID: m.UID, UIDValidity: m.UIDValidity,
			MessageID: m.MessageID, Thread: thread, InReplyTo: m.InReplyTo, References: m.References,
			From: m.From, ReplyTo: m.ReplyTo, To: m.To, Cc: m.Cc, Subject: m.Subject, Date: m.Date.Unix(),
			Text: m.Text, HTML: m.HTML, Attachments: m.Attachments, ListID: m.ListID, Bulk: m.Bulk,
			FromMe: me(m.From.Email), FetchedAt: now}
		if m.Date.IsZero() {
			e.Date = now
		}
		// seq orders messages by when docket stored them (a stored number,
		// unlike rowid, which a VACUUM may renumber).
		_, err := tx.Exec(`INSERT INTO email_messages(id, account, folder, uid, uidvalidity, message_id, thread, in_reply_to, refs,
			from_name, from_addr, reply_to, to_json, cc_json, subject, date, text, html, attachments, list_id, bulk, from_me, fetched_at, seq)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM email_messages))`,
			e.ID, e.Account, e.Folder, e.UID, e.UIDValidity, e.MessageID, e.Thread, strings.Join(e.InReplyTo, " "), strings.Join(e.References, " "),
			e.From.Name, e.From.Email, js(e.ReplyTo), js(e.To), js(e.Cc), e.Subject, e.Date, e.Text, e.HTML, js(e.Attachments),
			e.ListID, b2i(e.Bulk), b2i(e.FromMe), e.FetchedAt)
		if err != nil {
			return nil, fmt.Errorf("store email: %w", err)
		}
		// Replies that arrived before this message, and were filed under its
		// id or one of its ancestors' (their client sent no full
		// References), join its thread now.
		if keys := mergeable(tx, account, ownKeys(m, thread)); len(keys) > 0 {
			args := []any{thread, account}
			for _, k := range keys {
				args = append(args, k)
			}
			// Best effort: a failure here leaves a reply in its own thread,
			// never the message unstored.
			// They're new to this thread (a draft of it never saw them), so
			// they're numbered as stored into it now.
			tx.Exec(`UPDATE email_messages SET thread=?, seq=(SELECT MAX(seq) FROM email_messages)+1 WHERE account=? AND thread IN (?`+strings.Repeat(",?", len(keys)-1)+`)`, args...)
			mergeThreads(tx, account, keys, thread)
		}
		added = append(added, e)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Re-read threads: a later message in this batch may have moved an
	// earlier one.
	for i := range added {
		s.DB.QueryRow(`SELECT thread FROM email_messages WHERE id=?`, added[i].ID).Scan(&added[i].Thread)
	}
	return added, nil
}

// ownKeys are the thread keys a message's replies may have been filed
// under before it arrived: its own id and its ancestors', except its thread.
func ownKeys(m *mail.Message, thread string) []string {
	var out []string
	seen := map[string]bool{thread: true}
	for _, k := range append(append([]string{m.MessageID}, m.InReplyTo...), m.References...) {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// threadOf files a message under the thread of any stored message it
// replies to or references, else under its root reference, else its own id.
// Keys from roots converge: a reply that arrives before its original gets
// the original's id, which is what the original gets when it comes.
func threadOf(tx *sql.Tx, account string, m *mail.Message, rowID string) string {
	ids := append(append([]string{}, m.InReplyTo...), m.References...)
	if m.MessageID != "" {
		ids = append(ids, m.MessageID)
	}
	if len(ids) > 64 { // Parse keeps at most 20 References; stay well under SQLite's variable limit regardless
		ids = append(ids[:1], ids[len(ids)-63:]...)
	}
	if len(ids) > 0 {
		q := `SELECT thread FROM email_messages WHERE account=? AND message_id IN (?` + strings.Repeat(",?", len(ids)-1) + `) ORDER BY date LIMIT 1`
		args := []any{account}
		for _, id := range ids {
			args = append(args, id)
		}
		var t string
		if tx.QueryRow(q, args...).Scan(&t) == nil && t != "" {
			return t
		}
	}
	switch {
	case len(m.References) > 0:
		return m.References[0]
	case len(m.InReplyTo) > 0:
		return m.InReplyTo[0]
	case m.MessageID != "":
		return m.MessageID
	}
	return "row:" + rowID
}

const emailCols = `id, account, folder, uid, uidvalidity, message_id, thread, in_reply_to, refs, from_name, from_addr, reply_to,
	to_json, cc_json, subject, date, text, html, attachments, list_id, bulk, from_me, fetched_at, pruned`

func scanEmail(r interface{ Scan(...any) error }) (EmailMessage, error) {
	var e EmailMessage
	var irt, refs, rt, to, cc, att string
	var bulk, me, pruned int
	err := r.Scan(&e.ID, &e.Account, &e.Folder, &e.UID, &e.UIDValidity, &e.MessageID, &e.Thread, &irt, &refs, &e.From.Name, &e.From.Email,
		&rt, &to, &cc, &e.Subject, &e.Date, &e.Text, &e.HTML, &att, &e.ListID, &bulk, &me, &e.FetchedAt, &pruned)
	e.InReplyTo, e.References = strings.Fields(irt), strings.Fields(refs)
	json.Unmarshal([]byte(rt), &e.ReplyTo)
	json.Unmarshal([]byte(to), &e.To)
	json.Unmarshal([]byte(cc), &e.Cc)
	json.Unmarshal([]byte(att), &e.Attachments)
	e.Bulk, e.FromMe, e.Pruned = bulk != 0, me != 0, pruned != 0
	return e, err
}

// ThreadMessages lists a thread's messages, oldest first, one per
// Message-ID (the same message can sit in two folders).
func (s *Store) ThreadMessages(account, thread string) ([]EmailMessage, error) {
	rows, err := s.DB.Query(`SELECT `+emailCols+` FROM email_messages WHERE account=? AND thread=? ORDER BY date, id`, account, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailMessage
	seen := map[string]bool{}
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		if e.MessageID != "" {
			if seen[e.MessageID] {
				continue
			}
			seen[e.MessageID] = true
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EmailMessageByID loads one stored message.
func (s *Store) EmailMessageByID(id string) (EmailMessage, error) {
	return scanEmail(s.DB.QueryRow(`SELECT `+emailCols+` FROM email_messages WHERE id=?`, id))
}

// SenderHistory lists earlier messages in other threads to or from addr,
// newest first: the context a draft gets about the person.
func (s *Store) SenderHistory(account, addr, exceptThread string, limit int) ([]EmailMessage, error) {
	addr = strings.ToLower(addr)
	like := `%"email":"` + strings.ReplaceAll(addr, `"`, ``) + `"%`
	rows, err := s.DB.Query(`SELECT `+emailCols+` FROM email_messages WHERE account=? AND thread<>? AND (from_addr=? OR to_json LIKE ? OR cc_json LIKE ?)
		ORDER BY date DESC LIMIT ?`, account, exceptThread, addr, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailMessage
	for rows.Next() {
		e, err := scanEmail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EmailCursor is how far a folder has been read.
func (s *Store) EmailCursor(account, folder string) mail.Cursor {
	var c mail.Cursor
	s.DB.QueryRow(`SELECT uidvalidity, last_uid FROM email_cursors WHERE account=? AND folder=?`, account, folder).Scan(&c.UIDValidity, &c.LastUID)
	return c
}

// SetEmailCursor records how far a folder has been read.
func (s *Store) SetEmailCursor(account, folder string, c mail.Cursor) error {
	_, err := s.DB.Exec(`INSERT INTO email_cursors(account, folder, uidvalidity, last_uid) VALUES (?,?,?,?)
		ON CONFLICT(account, folder) DO UPDATE SET uidvalidity=excluded.uidvalidity, last_uid=excluded.last_uid`, account, folder, c.UIDValidity, c.LastUID)
	return err
}

// AddContacts records addresses you've written to (from your Sent folder).
func (s *Store) AddContacts(account string, sent map[string]int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for addr, at := range sent {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO email_contacts(account, address, sent, last_at) VALUES (?,?,1,?)
			ON CONFLICT(account, address) DO UPDATE SET sent=sent+1, last_at=MAX(last_at, excluded.last_at)`, account, addr, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Known reports whether you've written to addr from this account.
func (s *Store) Known(account, addr string) bool {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM email_contacts WHERE account=? AND address=?`, account, strings.ToLower(strings.TrimSpace(addr))).Scan(&n)
	return n > 0
}

// EmailAccountState loads a mailbox's state.
func (s *Store) EmailAccountState(name string) EmailAccount {
	a := EmailAccount{Name: name}
	s.DB.QueryRow(`SELECT sent_folder, drafts_folder, ok_at, error, error_at, contacts_at FROM email_accounts WHERE name=?`, name).
		Scan(&a.SentFolder, &a.DraftsFolder, &a.OKAt, &a.Error, &a.ErrorAt, &a.ContactsAt)
	return a
}

// SetEmailFolders records the account's Sent and Drafts folders.
func (s *Store) SetEmailFolders(name, sent, drafts string) error {
	_, err := s.DB.Exec(`INSERT INTO email_accounts(name, sent_folder, drafts_folder) VALUES (?,?,?)
		ON CONFLICT(name) DO UPDATE SET sent_folder=excluded.sent_folder, drafts_folder=excluded.drafts_folder`, name, sent, drafts)
	return err
}

// EmailAccountResult records a read that worked (err nil) or failed.
func (s *Store) EmailAccountResult(name string, err error) {
	now := time.Now().Unix()
	if err == nil {
		s.DB.Exec(`INSERT INTO email_accounts(name, ok_at) VALUES (?,?) ON CONFLICT(name) DO UPDATE SET ok_at=excluded.ok_at, error='', error_at=0`, name, now)
		return
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	s.DB.Exec(`INSERT INTO email_accounts(name, error, error_at) VALUES (?,?,?) ON CONFLICT(name) DO UPDATE SET error=excluded.error,
		error_at=CASE WHEN email_accounts.error_at=0 THEN excluded.error_at ELSE email_accounts.error_at END`, name, msg, now)
}

// SetContactsScanned marks the Sent history as read.
func (s *Store) SetContactsScanned(name string) {
	s.DB.Exec(`INSERT INTO email_accounts(name, contacts_at) VALUES (?,?) ON CONFLICT(name) DO UPDATE SET contacts_at=excluded.contacts_at`, name, time.Now().Unix())
}

// PruneEmail drops message bodies fetched before cutoff; headers stay.
func (s *Store) PruneEmail(cutoff time.Time) (int64, error) {
	r, err := s.DB.Exec(`UPDATE email_messages SET text='', html='', pruned=1 WHERE pruned=0 AND fetched_at<?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func js(v any) string {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ThreadID is a thread's short, URL-safe id.
func ThreadID(account, thread string) string {
	h := sha256.Sum256([]byte(account + "\x00" + thread))
	return "et-" + hex.EncodeToString(h[:6])
}

// EmailThread is a conversation as triage sees it.
type EmailThread struct {
	ID          string
	Account     string
	Thread      string
	Subject     string
	From        mail.Addr // the newest message to you
	LastInbound string    // that message's row id
	InboundAt   int64
	RepliedAt   int64  // your newest message in the thread
	Verdict     string // reply | digest | skip ("" until triaged)
	Line        string // the one line shown: a digest fact, or the reply's title
	Why         string
	Project     string // a project it's about, if any
	Work        string // work it asks for, if any
	FirstTime   bool   // you've never written to the sender
	NeedsTriage bool
	TriagedAt   int64
	PassID      string
	ItemID      string // the reply item
	SeenAt      int64  // a digest line scrolled into view
	ReadAt      int64  // ticked off, or opened
	UntilAt     int64  // a digest line fades then
	CreatedAt   int64
	UpdatedAt   int64
}

const threadCols = `id, account, thread, subject, from_name, from_addr, last_inbound, inbound_at, replied_at, verdict, line, why,
	project, work, first_time, needs_triage, triaged_at, pass_id, item_id, seen_at, read_at, until_at, created_at, updated_at`

func scanThread(r interface{ Scan(...any) error }) (EmailThread, error) {
	var t EmailThread
	var first, needs int
	err := r.Scan(&t.ID, &t.Account, &t.Thread, &t.Subject, &t.From.Name, &t.From.Email, &t.LastInbound, &t.InboundAt, &t.RepliedAt,
		&t.Verdict, &t.Line, &t.Why, &t.Project, &t.Work, &first, &needs, &t.TriagedAt, &t.PassID, &t.ItemID, &t.SeenAt, &t.ReadAt,
		&t.UntilAt, &t.CreatedAt, &t.UpdatedAt)
	t.FirstTime, t.NeedsTriage = first != 0, needs != 0
	return t, err
}

func (s *Store) threads(where string, args ...any) ([]EmailThread, error) {
	rows, err := s.DB.Query(`SELECT `+threadCols+` FROM email_threads WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailThread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Thread loads one thread by id.
func (s *Store) Thread(id string) (EmailThread, bool) {
	t, err := scanThread(s.DB.QueryRow(`SELECT `+threadCols+` FROM email_threads WHERE id=?`, id))
	return t, err == nil
}

// ThreadByItem finds the thread a reply item belongs to.
func (s *Store) ThreadByItem(itemID string) (EmailThread, bool) {
	if itemID == "" {
		return EmailThread{}, false
	}
	t, err := scanThread(s.DB.QueryRow(`SELECT `+threadCols+` FROM email_threads WHERE item_id=?`, itemID))
	return t, err == nil
}

// ThreadActivity is what new messages changed in one thread.
type ThreadActivity struct {
	ID         string
	NewInbound bool // a message to you newer than any seen before
	Answered   bool // your newest message is newer than the newest to you
}

// NoteMessages updates threads from newly stored messages. Messages to you
// are the ones in a triage folder (inbound) and not from you; a thread with
// a newer one needs triage again. Your own messages (from Sent, or anywhere)
// mark the thread answered when they're newer than the newest message to
// you.
func (s *Store) NoteMessages(account string, msgs []EmailMessage, inbound func(folder string) bool) ([]ThreadActivity, error) {
	// A holding reply docket sent isn't an answer: the real reply is still
	// to come, so it never closes the reply item.
	var holding []bool
	for i := range msgs {
		holding = append(holding, msgs[i].FromMe && s.SentByDocketAsHolding(msgs[i].MessageID))
	}
	type agg struct {
		in      *EmailMessage
		repl    int64
		subject string
	}
	by := map[string]*agg{}
	var order []string
	for i := range msgs {
		m := &msgs[i]
		a := by[m.Thread]
		if a == nil {
			a = &agg{}
			by[m.Thread] = a
			order = append(order, m.Thread)
		}
		if m.FromMe {
			if holding[i] || IsForward(m.Subject) {
				continue // a holding reply or a forward isn't an answer
			}
			if m.Date > a.repl {
				a.repl = m.Date
			}
			if a.subject == "" {
				a.subject = m.Subject
			}
			continue
		}
		if !inbound(m.Folder) {
			continue
		}
		if a.in == nil || m.Date > a.in.Date {
			a.in = m
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var out []ThreadActivity
	for _, th := range order {
		a := by[th]
		id := ThreadID(account, th)
		var inAt, replAt int64
		exists := tx.QueryRow(`SELECT inbound_at, replied_at FROM email_threads WHERE id=?`, id).Scan(&inAt, &replAt) == nil
		if !exists {
			if a.in == nil && a.repl == 0 {
				continue
			}
			if a.in == nil {
				// Only your own message: remember when you wrote, so a later
				// message to you is judged against it.
				if _, err := tx.Exec(`INSERT INTO email_threads(id, account, thread, subject, replied_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
					id, account, th, a.subject, a.repl, now, now); err != nil {
					return nil, err
				}
				continue
			}
			if _, err := tx.Exec(`INSERT INTO email_threads(id, account, thread, created_at, updated_at) VALUES (?,?,?,?,?)`, id, account, th, now, now); err != nil {
				return nil, err
			}
		}
		act := ThreadActivity{ID: id}
		if a.in != nil {
			// A message to you that docket hadn't seen: triage again,
			// whatever its (sender-written) Date header says.
			act.NewInbound = true
			if a.in.Date >= inAt {
				inAt = a.in.Date
				if _, err := tx.Exec(`UPDATE email_threads SET subject=?, from_name=?, from_addr=?, last_inbound=?, inbound_at=?, needs_triage=1, triage_fails=0, updated_at=? WHERE id=?`,
					a.in.Subject, a.in.From.Name, a.in.From.Email, a.in.ID, a.in.Date, now, id); err != nil {
					return nil, err
				}
			} else if _, err := tx.Exec(`UPDATE email_threads SET needs_triage=1, updated_at=? WHERE id=?`, now, id); err != nil {
				return nil, err
			}
		}
		if a.repl > replAt {
			replAt = a.repl
			if _, err := tx.Exec(`UPDATE email_threads SET replied_at=?, updated_at=? WHERE id=?`, a.repl, now, id); err != nil {
				return nil, err
			}
		}
		act.Answered = replAt > 0 && replAt >= inAt
		if act.Answered {
			tx.Exec(`UPDATE email_threads SET needs_triage=0 WHERE id=?`, id)
		}
		if act.NewInbound || act.Answered {
			out = append(out, act)
		}
	}
	return out, tx.Commit()
}

// ThreadsToTriage lists threads with a message to you not yet triaged,
// oldest first.
func (s *Store) ThreadsToTriage(limit int) ([]EmailThread, error) {
	return s.threads(`needs_triage=1 AND inbound_at>replied_at AND triage_fails<2 ORDER BY inbound_at, id LIMIT ?`, limit)
}

// TriageFails is how many times the model failed on a thread alone.
func (s *Store) TriageFails(id string) int {
	var n int
	s.DB.QueryRow(`SELECT triage_fails FROM email_threads WHERE id=?`, id).Scan(&n)
	return n
}

// TriageFailed counts a thread the model couldn't sort on its own; after
// two failures it's no longer retried (the pass files it for the human).
func (s *Store) TriageFailed(id string) int {
	s.DB.Exec(`UPDATE email_threads SET triage_fails=triage_fails+1 WHERE id=?`, id)
	var n int
	s.DB.QueryRow(`SELECT triage_fails FROM email_threads WHERE id=?`, id).Scan(&n)
	return n
}

// Verdict is triage's call on one thread.
type Verdict struct {
	Verdict, Line, Why, Project, Work string
	FirstTime                         bool
	UntilAt                           int64
}

// SetVerdict records triage's call.
func (s *Store) SetVerdict(id, passID string, v Verdict) error {
	now := time.Now().Unix()
	_, err := s.DB.Exec(`UPDATE email_threads SET verdict=?, line=?, why=?, project=?, work=?, first_time=?, needs_triage=0, triaged_at=?, pass_id=?,
		until_at=?, seen_at=CASE WHEN ?='digest' THEN 0 ELSE seen_at END, read_at=CASE WHEN ?='digest' THEN 0 ELSE read_at END, updated_at=? WHERE id=?`,
		v.Verdict, v.Line, v.Why, v.Project, v.Work, b2i(v.FirstTime), now, passID, v.UntilAt, v.Verdict, v.Verdict, now, id)
	return err
}

// SetThreadItem links a thread to its reply item.
func (s *Store) SetThreadItem(id, itemID string) error {
	_, err := s.DB.Exec(`UPDATE email_threads SET item_id=?, updated_at=? WHERE id=?`, itemID, time.Now().Unix(), id)
	return err
}

// DigestLines lists the digest: lines not ticked off that haven't faded,
// newest first.
func (s *Store) DigestLines(now time.Time) ([]EmailThread, error) {
	return s.threads(`verdict='digest' AND read_at=0 AND until_at>? ORDER BY inbound_at DESC`, now.Unix())
}

// SkippedIn lists what a pass left out.
func (s *Store) SkippedIn(passID string) ([]EmailThread, error) {
	return s.threads(`verdict='skip' AND pass_id=? ORDER BY inbound_at DESC`, passID)
}

// MarkSeen records digest lines scrolled into view (first time only).
func (s *Store) MarkSeen(ids []string) error {
	now := time.Now().Unix()
	for _, id := range ids {
		if _, err := s.DB.Exec(`UPDATE email_threads SET seen_at=? WHERE id=? AND seen_at=0`, now, id); err != nil {
			return err
		}
	}
	return nil
}

// MarkRead ticks a digest line off (or brings it back, with read false).
func (s *Store) MarkRead(id string, read bool) error {
	v := int64(0)
	if read {
		v = time.Now().Unix()
	}
	_, err := s.DB.Exec(`UPDATE email_threads SET read_at=?, seen_at=CASE WHEN seen_at=0 THEN ? ELSE seen_at END, updated_at=? WHERE id=?`, v, v, time.Now().Unix(), id)
	return err
}

// MarkAllRead ticks off every digest line showing.
func (s *Store) MarkAllRead(now time.Time) (int64, error) {
	r, err := s.DB.Exec(`UPDATE email_threads SET read_at=?, seen_at=CASE WHEN seen_at=0 THEN ? ELSE seen_at END WHERE verdict='digest' AND read_at=0 AND until_at>?`,
		now.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// EmailPass is one triage pass.
type EmailPass struct {
	ID        string
	Trigger   string
	StartedAt int64
	EndedAt   int64
	Read      int
	Triaged   int
	Reply     int
	Digest    int
	Skip      int
	Answered  int
	Cost      float64
	Error     string
}

// StartPass records a pass starting.
func (s *Store) StartPass(id, trigger string) error {
	_, err := s.DB.Exec(`INSERT INTO email_passes(id, trigger, started_at) VALUES (?,?,?)`, id, trigger, time.Now().Unix())
	return err
}

// EndPass records what a pass did.
func (s *Store) EndPass(p EmailPass) error {
	_, err := s.DB.Exec(`UPDATE email_passes SET ended_at=?, read=?, triaged=?, reply=?, digest=?, skip=?, answered=?, cost=?, error=? WHERE id=?`,
		time.Now().Unix(), p.Read, p.Triaged, p.Reply, p.Digest, p.Skip, p.Answered, p.Cost, p.Error, p.ID)
	return err
}

// LastPass is the most recent finished pass.
func (s *Store) LastPass() (EmailPass, bool) {
	var p EmailPass
	err := s.DB.QueryRow(`SELECT id, trigger, started_at, ended_at, read, triaged, reply, digest, skip, answered, cost, error FROM email_passes
		WHERE ended_at>0 ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&p.ID, &p.Trigger, &p.StartedAt, &p.EndedAt, &p.Read, &p.Triaged, &p.Reply,
		&p.Digest, &p.Skip, &p.Answered, &p.Cost, &p.Error)
	return p, err == nil
}

// EndStalePasses closes passes left unfinished when the server stopped.
func (s *Store) EndStalePasses() {
	s.DB.Exec(`UPDATE email_passes SET ended_at=started_at, error=CASE WHEN error='' THEN 'docket stopped during this check' ELSE error END WHERE ended_at=0`)
}

// LastDigestPass is when the latest pass that added digest lines started.
func (s *Store) LastDigestPass() int64 {
	var t int64
	s.DB.QueryRow(`SELECT COALESCE(MAX(started_at),0) FROM email_passes WHERE ended_at>0 AND digest>0`).Scan(&t)
	return t
}

// MarkReadIDs ticks off the digest lines given (the ones on your screen).
func (s *Store) MarkReadIDs(ids []string) {
	now := time.Now().Unix()
	for _, id := range ids {
		s.DB.Exec(`UPDATE email_threads SET read_at=?, seen_at=CASE WHEN seen_at=0 THEN ? ELSE seen_at END WHERE id=? AND verdict='digest' AND read_at=0`, now, now, id)
	}
}

// RunningPass reports a pass that started and hasn't finished (in the
// last hour; an older one died with the server).
func (s *Store) RunningPass() bool {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM email_passes WHERE ended_at=0 AND started_at>?`, time.Now().Add(-time.Hour).Unix()).Scan(&n)
	return n > 0
}

// EmailFeedback is a correction to triage: what it said, and what it
// should have said.
type EmailFeedback struct {
	ThreadID, Was, Should, Line, From, Note string
	At                                      int64
}

// AddEmailFeedback records a correction.
func (s *Store) AddEmailFeedback(f EmailFeedback) error {
	_, err := s.DB.Exec(`INSERT INTO email_feedback(thread_id, was, should, line, from_addr, note, at) VALUES (?,?,?,?,?,?,?)`,
		f.ThreadID, f.Was, f.Should, f.Line, f.From, f.Note, time.Now().Unix())
	return err
}

// RecentEmailFeedback lists the latest corrections, newest first.
func (s *Store) RecentEmailFeedback(limit int) ([]EmailFeedback, error) {
	return s.emailFeedback(s.DB.Query(`SELECT thread_id, was, should, line, from_addr, note, at FROM email_feedback ORDER BY id DESC LIMIT ?`, limit))
}

// TriageFeedback is the corrections triage hears: the latest ones, and
// the latest of those the human explained in a note even once they're
// older. A note says why ("I'm only cc'd so I can fix the site; editors
// answer these"), the context that matters most, so it doesn't roll off
// with the plain corrections. Newest first.
func (s *Store) TriageFeedback(recent, noted int) ([]EmailFeedback, error) {
	return s.emailFeedback(s.DB.Query(`SELECT thread_id, was, should, line, from_addr, note, at FROM email_feedback
		WHERE id IN (SELECT id FROM email_feedback ORDER BY id DESC LIMIT ?)
		   OR id IN (SELECT id FROM email_feedback WHERE note<>'' ORDER BY id DESC LIMIT ?)
		ORDER BY id DESC`, recent, noted))
}

func (s *Store) emailFeedback(rows *sql.Rows, err error) ([]EmailFeedback, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmailFeedback
	for rows.Next() {
		var f EmailFeedback
		if err := rows.Scan(&f.ThreadID, &f.Was, &f.Should, &f.Line, &f.From, &f.Note, &f.At); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EmailVersion changes whenever something the Email sections show changes,
// so open pages refresh.
func (s *Store) EmailVersion() string {
	var n, mx int64
	var p string
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at),0) FROM email_threads WHERE verdict<>'skip'`).Scan(&n, &mx)
	s.DB.QueryRow(`SELECT COALESCE(MAX(id),'') FROM email_passes`).Scan(&p)
	var r, se int64
	s.DB.QueryRow(`SELECT COALESCE(SUM(read_at),0), COALESCE(SUM(seen_at>0),0) FROM email_threads WHERE verdict='digest'`).Scan(&r, &se)
	return fmt.Sprintf("%d.%d.%s.%d", n, mx, p, r)
}

// mergeable keeps the thread keys whose threads hold no state: never
// triaged, no reply item, no draft, job or send. A thread that holds any of
// that stays its own thread, so nothing approved, drafted or on its way out
// ever moves under the human. (With Sent read before the inbox this only
// happens when your reply reached Sent after theirs reached the inbox.)
func mergeable(tx *sql.Tx, account string, keys []string) []string {
	var out []string
	for _, k := range keys {
		id := ThreadID(account, k)
		var state int
		tx.QueryRow(`SELECT (SELECT COUNT(*) FROM email_threads WHERE id=? AND (verdict<>'' OR item_id<>'' OR idea_id<>''))
			+ (SELECT COUNT(*) FROM email_drafts WHERE thread_id=?) + (SELECT COUNT(*) FROM email_jobs WHERE thread_id=?)
			+ (SELECT COUNT(*) FROM email_outbox WHERE thread_id=?)`, id, id, id, id).Scan(&state)
		if state == 0 {
			out = append(out, k)
		}
	}
	return out
}

// mergeThreads folds the rows of untriaged threads (see mergeable) that
// turned out to be part of thread `into` into its row: the newest message to
// you wins and a pending triage stays pending.
func mergeThreads(tx *sql.Tx, account string, keys []string, into string) error {
	to := ThreadID(account, into)
	now := time.Now().Unix()
	for _, k := range keys {
		from := ThreadID(account, k)
		if from == to {
			continue
		}
		var exists int
		tx.QueryRow(`SELECT COUNT(*) FROM email_threads WHERE id=?`, from).Scan(&exists)
		if exists == 0 {
			continue
		}
		var have int
		tx.QueryRow(`SELECT COUNT(*) FROM email_threads WHERE id=?`, to).Scan(&have)
		if have == 0 {
			if _, err := tx.Exec(`UPDATE email_threads SET id=?, thread=?, updated_at=? WHERE id=?`, to, into, now, from); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`UPDATE email_threads AS t SET
				subject = CASE WHEN o.inbound_at > t.inbound_at THEN o.subject ELSE t.subject END,
				from_name = CASE WHEN o.inbound_at > t.inbound_at THEN o.from_name ELSE t.from_name END,
				from_addr = CASE WHEN o.inbound_at > t.inbound_at THEN o.from_addr ELSE t.from_addr END,
				last_inbound = CASE WHEN o.inbound_at > t.inbound_at THEN o.last_inbound ELSE t.last_inbound END,
				inbound_at = MAX(t.inbound_at, o.inbound_at),
				replied_at = MAX(t.replied_at, o.replied_at),
				needs_triage = MAX(t.needs_triage, o.needs_triage),
				updated_at = ?
				FROM (SELECT * FROM email_threads WHERE id=?) AS o WHERE t.id=?`, now, from, to); err != nil {
				return err
			}
			tx.Exec(`DELETE FROM email_threads WHERE id=?`, from)
		}
	}
	return nil
}

// SetThreadIdea links a thread to the work idea its email asked for, and
// records the idea's status the draft last reflected.
func (s *Store) SetThreadIdea(id, ideaID, status string) error {
	_, err := s.DB.Exec(`UPDATE email_threads SET idea_id=?, idea_status=?, updated_at=? WHERE id=?`, ideaID, status, time.Now().Unix(), id)
	return err
}

// ThreadIdea is a thread's work idea and the status its draft reflects.
func (s *Store) ThreadIdea(id string) (ideaID, status string) {
	s.DB.QueryRow(`SELECT idea_id, idea_status FROM email_threads WHERE id=?`, id).Scan(&ideaID, &status)
	return
}

// ThreadsWithIdeas lists threads waiting on work, with the status their
// draft last reflected.
func (s *Store) ThreadsWithIdeas() (map[string][2]string, error) {
	rows, err := s.DB.Query(`SELECT id, idea_id, idea_status FROM email_threads WHERE idea_id<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var id, idea, st string
		if err := rows.Scan(&id, &idea, &st); err != nil {
			return nil, err
		}
		out[id] = [2]string{idea, st}
	}
	return out, rows.Err()
}

// EmailIdeasProposed lists ideas filed from email that wait on their
// proposal.
func (s *Store) EmailIdeasProposed(mark string) ([]Idea, error) {
	return s.ideaList(`WHERE status=? AND dictation_id='' AND reason LIKE ? ORDER BY created_at`, IdeaProposed, mark+"%")
}

// OpenReplyThreads lists threads whose reply item is still open.
func (s *Store) OpenReplyThreads() ([]EmailThread, error) {
	return s.threads(`item_id<>'' AND EXISTS (SELECT 1 FROM items i WHERE i.id=email_threads.item_id AND i.status IN ('open','snoozed','answered'))`)
}
