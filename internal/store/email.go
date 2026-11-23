package store

import (
	"database/sql"
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
  pruned INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS email_messages_mid ON email_messages(account, message_id);
CREATE INDEX IF NOT EXISTS email_messages_thread ON email_messages(account, thread, date);
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
		_, err := tx.Exec(`INSERT INTO email_messages(id, account, folder, uid, uidvalidity, message_id, thread, in_reply_to, refs,
			from_name, from_addr, reply_to, to_json, cc_json, subject, date, text, html, attachments, list_id, bulk, from_me, fetched_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.ID, e.Account, e.Folder, e.UID, e.UIDValidity, e.MessageID, e.Thread, strings.Join(e.InReplyTo, " "), strings.Join(e.References, " "),
			e.From.Name, e.From.Email, js(e.ReplyTo), js(e.To), js(e.Cc), e.Subject, e.Date, e.Text, e.HTML, js(e.Attachments),
			e.ListID, b2i(e.Bulk), b2i(e.FromMe), e.FetchedAt)
		if err != nil {
			return nil, fmt.Errorf("store email: %w", err)
		}
		added = append(added, e)
	}
	return added, tx.Commit()
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
