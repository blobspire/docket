// Package store is the server's SQLite database: raw events pulled from
// every machine, the sessions they describe, and the items in the queue.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS machines (
  name TEXT PRIMARY KEY,
  last_seen_at INTEGER,
  last_error TEXT,
  last_error_at INTEGER,
  failing_since INTEGER
);
CREATE TABLE IF NOT EXISTS cursors (
  machine TEXT PRIMARY KEY,
  cursor TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  machine TEXT NOT NULL,
  ts TEXT,
  event TEXT,
  session_id TEXT,
  body TEXT NOT NULL,
  processed INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_todo ON events(processed, id);
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  machine TEXT,
  cwd TEXT,
  lead TEXT,
  name TEXT,
  tmux TEXT,
  bridge_id TEXT,
  pid INTEGER,
  state TEXT,
  waiting_for TEXT,
  transcript_path TEXT,
  started_at INTEGER,
  last_event_at INTEGER,
  ended_at INTEGER,
  pending_permission TEXT,
  last_message TEXT,
  has_plugin INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL DEFAULT '',
  nudges INTEGER NOT NULL DEFAULT 0,
  nudge_hits INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS items (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  status TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  options TEXT NOT NULL DEFAULT '[]',
  recommend TEXT NOT NULL DEFAULT '',
  blocking INTEGER NOT NULL DEFAULT 0,
  blocks TEXT NOT NULL DEFAULT '',
  physical INTEGER NOT NULL DEFAULT 0,
  due TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT,
  source TEXT NOT NULL DEFAULT '',
  machine TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  lead TEXT NOT NULL DEFAULT '',
  cwd TEXT NOT NULL DEFAULT '',
  quote TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  mention_count INTEGER NOT NULL DEFAULT 1,
  snoozed_until INTEGER,
  resolved_at INTEGER,
  resolution TEXT NOT NULL DEFAULT '',
  answer TEXT NOT NULL DEFAULT '',
  answered_at INTEGER,
  pushed_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS items_active_dedupe ON items(dedupe_key)
  WHERE dedupe_key IS NOT NULL AND status IN ('maybe','open','snoozed','answered');
CREATE INDEX IF NOT EXISTS items_status ON items(status);
CREATE INDEX IF NOT EXISTS items_session ON items(session_id);
CREATE TABLE IF NOT EXISTS evidence (
  sha256 TEXT NOT NULL,
  item_id TEXT NOT NULL,
  name TEXT NOT NULL,
  mime TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  fetched INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (sha256, item_id)
);
CREATE TABLE IF NOT EXISTS deliveries (
  id TEXT PRIMARY KEY,
  item_id TEXT NOT NULL,
  machine TEXT NOT NULL,
  session_id TEXT NOT NULL,
  text TEXT NOT NULL,
  nonce TEXT NOT NULL,
  method TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  sent_at INTEGER,
  confirmed_at INTEGER,
  attempts INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS deliveries_status ON deliveries(status);
CREATE TABLE IF NOT EXISTS decisions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  item_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  action TEXT NOT NULL,
  answer TEXT NOT NULL DEFAULT '',
  prev_status TEXT NOT NULL DEFAULT ''
);
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(id UNINDEXED, title, body, quote, answer, resolution, tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS items_fts_ai AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(id, title, body, quote, answer, resolution) VALUES (new.id, new.title, new.body, new.quote, new.answer, new.resolution);
END;
CREATE TRIGGER IF NOT EXISTS items_fts_au AFTER UPDATE OF title, body, quote, answer, resolution ON items BEGIN
  DELETE FROM items_fts WHERE id = old.id;
  INSERT INTO items_fts(id, title, body, quote, answer, resolution) VALUES (new.id, new.title, new.body, new.quote, new.answer, new.resolution);
END;
CREATE TRIGGER IF NOT EXISTS items_fts_ad AFTER DELETE ON items BEGIN
  DELETE FROM items_fts WHERE id = old.id;
END;
CREATE TABLE IF NOT EXISTS kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`

// Active statuses: the item still needs someone.
var Active = []string{"maybe", "open", "snoozed", "answered"}

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer; keeps transactions simple
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	// Databases created before search existed: index their items once.
	var indexed, items int
	db.QueryRow(`SELECT COUNT(*) FROM items_fts`).Scan(&indexed)
	db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&items)
	if indexed == 0 && items > 0 {
		db.Exec(`INSERT INTO items_fts(id, title, body, quote, answer, resolution) SELECT id, title, body, quote, answer, resolution FROM items`)
	}
	return &Store{DB: db}, nil
}

// Search finds items (open or closed) matching every word of q, best first.
func (s *Store) Search(q string, limit int) ([]Item, error) {
	var terms []string
	for _, w := range strings.Fields(q) {
		w = strings.Map(func(r rune) rune {
			if r == '"' || r == '*' || r == '(' || r == ')' || r == ':' || r == '^' {
				return -1
			}
			return r
		}, w)
		if w != "" {
			terms = append(terms, `"`+w+`"*`)
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}
	rows, err := s.DB.Query(`SELECT `+prefixed("i.", itemCols)+` FROM items_fts f JOIN items i ON i.id = f.id WHERE items_fts MATCH ? ORDER BY bm25(items_fts, 0, 8, 2, 2, 4, 1) LIMIT ?`, strings.Join(terms, " "), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// prefixed qualifies a column list (handling COALESCE(...) wrappers).
func prefixed(p, cols string) string {
	var out []string
	for _, c := range strings.Split(cols, ", ") {
		c = strings.TrimSpace(c)
		if strings.HasPrefix(c, "COALESCE(") {
			out = append(out, "COALESCE("+p+strings.TrimPrefix(c, "COALESCE("))
		} else {
			out = append(out, p+c)
		}
	}
	return strings.Join(out, ", ")
}

// EvidenceRow is evidence with the item it belongs to.
type EvidenceRow struct {
	Evidence
	ItemTitle  string
	ItemStatus string
	ItemKind   string
	Machine    string
	CreatedAt  int64
}

// AllEvidence lists evidence across items, newest first, optionally
// filtered by words in the file name or item title.
func (s *Store) AllEvidence(q string, limit int) ([]EvidenceRow, error) {
	query := `SELECT e.sha256, e.item_id, e.name, e.mime, e.size, e.fetched, i.title, i.status, i.kind, i.machine, i.created_at
		FROM evidence e JOIN items i ON i.id = e.item_id`
	var args []any
	for _, w := range strings.Fields(q) {
		if len(args) == 0 {
			query += ` WHERE`
		} else {
			query += ` AND`
		}
		query += ` (e.name LIKE ? OR i.title LIKE ?)`
		args = append(args, "%"+w+"%", "%"+w+"%")
	}
	query += ` ORDER BY i.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceRow
	for rows.Next() {
		var r EvidenceRow
		var f int
		if err := rows.Scan(&r.SHA256, &r.ItemID, &r.Name, &r.Mime, &r.Size, &f, &r.ItemTitle, &r.ItemStatus, &r.ItemKind, &r.Machine, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Fetched = f != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// HumanTasks lists open things only the human can do: due dates first.
func (s *Store) HumanTasks() ([]Item, error) {
	rows, err := s.DB.Query(`SELECT ` + itemCols + ` FROM items WHERE kind='human_task' AND status IN ('open','snoozed','answered')
		ORDER BY CASE WHEN due='' THEN 1 ELSE 0 END, due, blocking DESC, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) Close() error { return s.DB.Close() }

func now() int64 { return time.Now().Unix() }

// ---- ingest ---------------------------------------------------------------

// Cursor returns the stored spool cursor for a machine ("" = start).
func (s *Store) Cursor(machine string) (string, error) {
	var c string
	err := s.DB.QueryRow(`SELECT cursor FROM cursors WHERE machine=?`, machine).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c, err
}

// RawEvent is one spool record as received.
type RawEvent struct {
	ID        string
	Machine   string
	TS        string
	Event     string
	SessionID string
	Body      []byte
}

// IngestBatch stores records and the new cursor in one transaction, so a
// crash can neither lose nor duplicate records. It returns how many were new.
func (s *Store) IngestBatch(machine string, evs []RawEvent, cursor string) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT OR IGNORE INTO events(id, machine, ts, event, session_id, body) VALUES (?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	n := 0
	for _, e := range evs {
		res, err := ins.Exec(e.ID, machine, e.TS, e.Event, e.SessionID, string(e.Body))
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	if cursor != "" {
		if _, err := tx.Exec(`INSERT INTO cursors(machine, cursor) VALUES (?,?) ON CONFLICT(machine) DO UPDATE SET cursor=excluded.cursor`, machine, cursor); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO machines(name, last_seen_at, failing_since) VALUES (?,?,NULL)
		ON CONFLICT(name) DO UPDATE SET last_seen_at=excluded.last_seen_at, failing_since=NULL`, machine, now()); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// MachineError records a failed pull or push for a machine.
func (s *Store) MachineError(machine, msg string) error {
	t := now()
	_, err := s.DB.Exec(`INSERT INTO machines(name, last_error, last_error_at, failing_since) VALUES (?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET last_error=excluded.last_error, last_error_at=excluded.last_error_at,
		failing_since=COALESCE(machines.failing_since, excluded.failing_since)`, machine, msg, t, t)
	return err
}

// MachineSeen records a successful contact with a machine.
func (s *Store) MachineSeen(machine string) error {
	_, err := s.DB.Exec(`INSERT INTO machines(name, last_seen_at) VALUES (?,?)
		ON CONFLICT(name) DO UPDATE SET last_seen_at=excluded.last_seen_at, failing_since=NULL`, machine, now())
	return err
}

type Machine struct {
	Name         string
	LastSeenAt   int64
	LastError    string
	LastErrorAt  int64
	FailingSince int64
}

func (s *Store) Machines() ([]Machine, error) {
	rows, err := s.DB.Query(`SELECT name, COALESCE(last_seen_at,0), COALESCE(last_error,''), COALESCE(last_error_at,0), COALESCE(failing_since,0) FROM machines ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.Name, &m.LastSeenAt, &m.LastError, &m.LastErrorAt, &m.FailingSince); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Unprocessed returns events not yet turned into items, oldest first.
func (s *Store) Unprocessed(limit int) ([]RawEvent, error) {
	rows, err := s.DB.Query(`SELECT id, machine, COALESCE(ts,''), COALESCE(event,''), COALESCE(session_id,''), body FROM events WHERE processed=0 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RawEvent
	for rows.Next() {
		var e RawEvent
		var body string
		if err := rows.Scan(&e.ID, &e.Machine, &e.TS, &e.Event, &e.SessionID, &body); err != nil {
			return nil, err
		}
		e.Body = []byte(body)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) MarkProcessed(id string) error {
	_, err := s.DB.Exec(`UPDATE events SET processed=1 WHERE id=?`, id)
	return err
}

// ---- sessions -------------------------------------------------------------

type Session struct {
	ID                string
	Machine           string
	Cwd               string
	Lead              string
	Name              string
	Tmux              string
	BridgeID          string
	PID               int64
	State             string // busy | idle | waiting | ended
	WaitingFor        string
	TranscriptPath    string
	StartedAt         int64
	LastEventAt       int64
	EndedAt           int64
	PendingPermission string
	LastMessage       string
	HasPlugin         bool
	Kind              string // interactive | bg | … (from the session registry)
	Nudges            int
	NudgeHits         int
}

const sessionCols = `id, COALESCE(machine,''), COALESCE(cwd,''), COALESCE(lead,''), COALESCE(name,''), COALESCE(tmux,''), COALESCE(bridge_id,''), COALESCE(pid,0), COALESCE(state,''), COALESCE(waiting_for,''), COALESCE(transcript_path,''), COALESCE(started_at,0), COALESCE(last_event_at,0), COALESCE(ended_at,0), COALESCE(pending_permission,''), COALESCE(last_message,''), has_plugin, kind, nudges, nudge_hits`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var x Session
	var plugin int
	err := sc.Scan(&x.ID, &x.Machine, &x.Cwd, &x.Lead, &x.Name, &x.Tmux, &x.BridgeID, &x.PID, &x.State, &x.WaitingFor, &x.TranscriptPath, &x.StartedAt, &x.LastEventAt, &x.EndedAt, &x.PendingPermission, &x.LastMessage, &plugin, &x.Kind, &x.Nudges, &x.NudgeHits)
	x.HasPlugin = plugin != 0
	return x, err
}

// GetSession returns the session, or ok=false.
func (s *Store) GetSession(id string) (Session, bool, error) {
	x, err := scanSession(s.DB.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	return x, err == nil, err
}

// SessionPatch updates only the fields that are set.
type SessionPatch struct {
	Machine, Cwd, Lead, Name, Tmux, BridgeID, State, WaitingFor, TranscriptPath, PendingPermission, LastMessage, Kind *string
	PID, StartedAt, LastEventAt, EndedAt                                                                              *int64
	HasPlugin                                                                                                         *bool
	AddNudge, AddNudgeHit                                                                                             bool
}

func Str(s string) *string { return &s }
func Int(i int64) *int64   { return &i }
func Bool(b bool) *bool    { return &b }

// PatchSession creates the session if needed and applies the patch.
func (s *Store) PatchSession(id string, p SessionPatch) error {
	if id == "" {
		return nil
	}
	if _, err := s.DB.Exec(`INSERT OR IGNORE INTO sessions(id) VALUES (?)`, id); err != nil {
		return err
	}
	var sets []string
	var args []any
	add := func(col string, v any) { sets = append(sets, col+"=?"); args = append(args, v) }
	for col, v := range map[string]*string{"machine": p.Machine, "cwd": p.Cwd, "lead": p.Lead, "name": p.Name, "tmux": p.Tmux, "bridge_id": p.BridgeID, "state": p.State, "waiting_for": p.WaitingFor, "transcript_path": p.TranscriptPath, "pending_permission": p.PendingPermission, "last_message": p.LastMessage, "kind": p.Kind} {
		if v != nil {
			add(col, *v)
		}
	}
	for col, v := range map[string]*int64{"pid": p.PID, "started_at": p.StartedAt, "last_event_at": p.LastEventAt, "ended_at": p.EndedAt} {
		if v != nil {
			add(col, *v)
		}
	}
	if p.HasPlugin != nil {
		add("has_plugin", boolInt(*p.HasPlugin))
	}
	if p.AddNudge {
		sets = append(sets, "nudges=nudges+1")
	}
	if p.AddNudgeHit {
		sets = append(sets, "nudge_hits=nudge_hits+1")
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := s.DB.Exec(`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	return err
}

// Sessions lists sessions, most recently active first.
func (s *Store) Sessions(includeEnded bool, limit int) ([]Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions`
	if !includeEnded {
		q += ` WHERE COALESCE(state,'') != 'ended'`
	}
	q += ` ORDER BY COALESCE(last_event_at,0) DESC LIMIT ?`
	rows, err := s.DB.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- items ----------------------------------------------------------------

type Item struct {
	ID           string
	Kind         string
	Status       string
	Title        string
	Body         string
	Options      []string
	Recommend    string
	Blocking     bool
	Blocks       string
	Physical     bool
	Due          string
	DedupeKey    string
	Source       string
	Machine      string
	SessionID    string
	Lead         string
	Cwd          string
	Quote        string
	CreatedAt    int64
	UpdatedAt    int64
	LastSeenAt   int64
	MentionCount int
	SnoozedUntil int64
	ResolvedAt   int64
	Resolution   string
	Answer       string
	AnsweredAt   int64
	PushedAt     int64
}

const itemCols = `id, kind, status, title, body, options, recommend, blocking, blocks, physical, due, COALESCE(dedupe_key,''), source, machine, session_id, lead, cwd, quote, created_at, updated_at, last_seen_at, mention_count, COALESCE(snoozed_until,0), COALESCE(resolved_at,0), resolution, answer, COALESCE(answered_at,0), COALESCE(pushed_at,0)`

func scanItem(sc interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var opts string
	var blocking, physical int
	err := sc.Scan(&it.ID, &it.Kind, &it.Status, &it.Title, &it.Body, &opts, &it.Recommend, &blocking, &it.Blocks, &physical, &it.Due, &it.DedupeKey, &it.Source, &it.Machine, &it.SessionID, &it.Lead, &it.Cwd, &it.Quote, &it.CreatedAt, &it.UpdatedAt, &it.LastSeenAt, &it.MentionCount, &it.SnoozedUntil, &it.ResolvedAt, &it.Resolution, &it.Answer, &it.AnsweredAt, &it.PushedAt)
	it.Blocking, it.Physical = blocking != 0, physical != 0
	_ = json.Unmarshal([]byte(opts), &it.Options)
	return it, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UpsertItem inserts it, or, when an active item already has the same
// dedupe key, records another mention of that item instead. It returns the
// stored item's id and whether it was newly created.
func (s *Store) UpsertItem(it Item) (string, bool, error) {
	t := now()
	if it.CreatedAt == 0 {
		it.CreatedAt = t
	}
	if it.Status == "" {
		it.Status = "open"
	}
	if it.DedupeKey != "" {
		var id string
		err := s.DB.QueryRow(`SELECT id FROM items WHERE dedupe_key=? AND status IN ('maybe','open','snoozed','answered')`, it.DedupeKey).Scan(&id)
		if err == nil {
			// Same thing again: count the mention and refresh the wording.
			_, err = s.DB.Exec(`UPDATE items SET mention_count=mention_count+1, last_seen_at=?, updated_at=?,
				title=CASE WHEN ?<>'' THEN ? ELSE title END,
				body=CASE WHEN ?<>'' THEN ? ELSE body END,
				status=CASE WHEN status='maybe' AND ?='open' THEN 'open' ELSE status END
				WHERE id=?`, t, t, it.Title, it.Title, it.Body, it.Body, it.Status, id)
			return id, false, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	opts, _ := json.Marshal(nonNil(it.Options))
	var dedupe any
	if it.DedupeKey != "" {
		dedupe = it.DedupeKey
	}
	res, err := s.DB.Exec(`INSERT OR IGNORE INTO items(id, kind, status, title, body, options, recommend, blocking, blocks, physical, due, dedupe_key, source, machine, session_id, lead, cwd, quote, created_at, updated_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.ID, it.Kind, it.Status, it.Title, it.Body, string(opts), it.Recommend, boolInt(it.Blocking), it.Blocks, boolInt(it.Physical), it.Due, dedupe, it.Source, it.Machine, it.SessionID, it.Lead, it.Cwd, it.Quote, it.CreatedAt, t, t)
	if err != nil {
		return "", false, err
	}
	n, _ := res.RowsAffected()
	return it.ID, n > 0, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Store) GetItem(id string) (Item, bool, error) {
	it, err := scanItem(s.DB.QueryRow(`SELECT `+itemCols+` FROM items WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	return it, err == nil, err
}

// Items lists items with one of the given statuses (all if none given).
func (s *Store) Items(statuses ...string) ([]Item, error) {
	q := `SELECT ` + itemCols + ` FROM items`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",") + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RecentlyClosed lists items resolved since `since`.
func (s *Store) RecentlyClosed(since int64, limit int) ([]Item, error) {
	rows, err := s.DB.Query(`SELECT `+itemCols+` FROM items WHERE status IN ('done','dismissed','delivered') AND COALESCE(resolved_at, answered_at, updated_at) >= ? ORDER BY updated_at DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetStatus changes an item's status, logging the change so it can be undone.
func (s *Store) SetStatus(id, status, resolution, action string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev string
	if err := tx.QueryRow(`SELECT status FROM items WHERE id=?`, id).Scan(&prev); err != nil {
		return err
	}
	t := now()
	var resolvedAt any
	if status == "done" || status == "dismissed" {
		resolvedAt = t
	}
	if _, err := tx.Exec(`UPDATE items SET status=?, resolution=CASE WHEN ?<>'' THEN ? ELSE resolution END, resolved_at=?, updated_at=?, snoozed_until=CASE WHEN ?='snoozed' THEN snoozed_until ELSE NULL END WHERE id=?`,
		status, resolution, resolution, resolvedAt, t, status, id); err != nil {
		if isDedupeConflict(err) {
			return fmt.Errorf("an active item with the same key already exists")
		}
		return err
	}
	if action != "" {
		if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, prev_status) VALUES (?,?,?,?)`, id, t, action, prev); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isDedupeConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}

// Snooze hides an item until t.
func (s *Store) Snooze(id string, until int64) error {
	if err := s.SetStatus(id, "snoozed", "", "snooze"); err != nil {
		return err
	}
	_, err := s.DB.Exec(`UPDATE items SET snoozed_until=? WHERE id=?`, until, id)
	return err
}

// Wake returns snoozed items whose time has come to the queue.
func (s *Store) Wake() (int, error) {
	res, err := s.DB.Exec(`UPDATE items SET status='open', snoozed_until=NULL, updated_at=? WHERE status='snoozed' AND snoozed_until <= ?`, now(), now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetAnswer records the human's answer and marks the item answered.
func (s *Store) SetAnswer(id, answer, action string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev string
	if err := tx.QueryRow(`SELECT status FROM items WHERE id=?`, id).Scan(&prev); err != nil {
		return err
	}
	t := now()
	if _, err := tx.Exec(`UPDATE items SET status='answered', answer=?, answered_at=?, updated_at=? WHERE id=?`, answer, t, t, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, answer, prev_status) VALUES (?,?,?,?,?)`, id, t, action, answer, prev); err != nil {
		return err
	}
	return tx.Commit()
}

// Undo reverts the most recent decision on an item. It returns the action undone.
func (s *Store) Undo(id string) (string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var did int64
	var action, prev string
	err = tx.QueryRow(`SELECT id, action, prev_status FROM decisions WHERE item_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&did, &action, &prev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("nothing to undo")
	}
	if err != nil {
		return "", err
	}
	var undeliverable int
	tx.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE item_id=? AND status IN ('sent','confirmed')`, id).Scan(&undeliverable)
	if undeliverable > 0 && (action == "answer" || action == "approve" || action == "decline" || action == "choose") {
		return "", errors.New("the answer has already been delivered to the agent; send a correction instead")
	}
	if _, err := tx.Exec(`UPDATE items SET status=?, resolved_at=NULL, snoozed_until=NULL, updated_at=?, answer=CASE WHEN ? IN ('answer','approve','decline','choose') THEN '' ELSE answer END WHERE id=?`, prev, now(), action, id); err != nil {
		return "", err
	}
	tx.Exec(`DELETE FROM deliveries WHERE item_id=? AND status='pending'`, id)
	if _, err := tx.Exec(`DELETE FROM decisions WHERE id=?`, did); err != nil {
		return "", err
	}
	return action, tx.Commit()
}

// ResolveBySession closes active items from the given sources for a session
// (for example a permission prompt, once the session has moved on).
func (s *Store) ResolveBySession(sessionID, resolution string, sources ...string) (int, error) {
	if sessionID == "" || len(sources) == 0 {
		return 0, nil
	}
	q := `UPDATE items SET status='done', resolution=?, resolved_at=?, updated_at=? WHERE session_id=? AND status IN ('open','maybe','snoozed') AND source IN (` + strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",") + `)`
	args := []any{resolution, now(), now(), sessionID}
	for _, src := range sources {
		args = append(args, src)
	}
	res, err := s.DB.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkPushed records that a push went out for these items.
func (s *Store) MarkPushed(ids []string) error {
	for _, id := range ids {
		if _, err := s.DB.Exec(`UPDATE items SET pushed_at=? WHERE id=?`, now(), id); err != nil {
			return err
		}
	}
	return nil
}

// ---- evidence -------------------------------------------------------------

type Evidence struct {
	SHA256, ItemID, Name, Mime string
	Size                       int64
	Fetched                    bool
}

func (s *Store) AddEvidence(e Evidence) error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO evidence(sha256, item_id, name, mime, size) VALUES (?,?,?,?,?)`, e.SHA256, e.ItemID, e.Name, e.Mime, e.Size)
	return err
}

func (s *Store) Evidence(itemID string) ([]Evidence, error) {
	rows, err := s.DB.Query(`SELECT sha256, item_id, name, mime, size, fetched FROM evidence WHERE item_id=? ORDER BY name`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		var e Evidence
		var f int
		if err := rows.Scan(&e.SHA256, &e.ItemID, &e.Name, &e.Mime, &e.Size, &f); err != nil {
			return nil, err
		}
		e.Fetched = f != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// MissingBlobs lists evidence not yet copied to the server, by machine.
func (s *Store) MissingBlobs(machine string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT e.sha256 FROM evidence e JOIN items i ON i.id=e.item_id WHERE e.fetched=0 AND i.machine=?`, machine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

func (s *Store) BlobFetched(sha string) error {
	_, err := s.DB.Exec(`UPDATE evidence SET fetched=1 WHERE sha256=?`, sha)
	return err
}

// ---- deliveries -----------------------------------------------------------

type Delivery struct {
	ID, ItemID, Machine, SessionID, Text, Nonce, Method, Status, Error string
	CreatedAt, SentAt, ConfirmedAt                                     int64
	Attempts                                                           int
}

const deliveryCols = `id, item_id, machine, session_id, text, nonce, method, status, error, created_at, COALESCE(sent_at,0), COALESCE(confirmed_at,0), attempts`

func scanDelivery(sc interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	err := sc.Scan(&d.ID, &d.ItemID, &d.Machine, &d.SessionID, &d.Text, &d.Nonce, &d.Method, &d.Status, &d.Error, &d.CreatedAt, &d.SentAt, &d.ConfirmedAt, &d.Attempts)
	return d, err
}

func (s *Store) AddDelivery(d Delivery) error {
	_, err := s.DB.Exec(`INSERT INTO deliveries(id, item_id, machine, session_id, text, nonce, status, created_at) VALUES (?,?,?,?,?,?,'pending',?)`,
		d.ID, d.ItemID, d.Machine, d.SessionID, d.Text, d.Nonce, now())
	return err
}

// Deliveries lists deliveries with the given status, oldest first.
func (s *Store) Deliveries(status string) ([]Delivery, error) {
	rows, err := s.DB.Query(`SELECT `+deliveryCols+` FROM deliveries WHERE status=? ORDER BY created_at`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) ItemDeliveries(itemID string) ([]Delivery, error) {
	rows, err := s.DB.Query(`SELECT `+deliveryCols+` FROM deliveries WHERE item_id=? ORDER BY created_at`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeliverySent records a send attempt by method.
func (s *Store) DeliverySent(id, method string) error {
	_, err := s.DB.Exec(`UPDATE deliveries SET status='sent', method=?, sent_at=?, attempts=attempts+1, error='' WHERE id=?`, method, now(), id)
	return err
}

// DeliveryFailed records a failed attempt; the delivery stays pending.
func (s *Store) DeliveryFailed(id, msg string) error {
	_, err := s.DB.Exec(`UPDATE deliveries SET attempts=attempts+1, error=? WHERE id=?`, msg, id)
	return err
}

// ConfirmDelivery marks the delivery with this nonce confirmed and its item delivered.
func (s *Store) ConfirmDelivery(nonce string) (bool, error) {
	var id, itemID string
	err := s.DB.QueryRow(`SELECT id, item_id FROM deliveries WHERE nonce=?`, nonce).Scan(&id, &itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	t := now()
	if _, err := s.DB.Exec(`UPDATE deliveries SET status='confirmed', confirmed_at=? WHERE id=? AND status!='confirmed'`, t, id); err != nil {
		return false, err
	}
	_, err = s.DB.Exec(`UPDATE items SET status='delivered', updated_at=? WHERE id=? AND status='answered'`, t, itemID)
	return true, err
}

// ---- key/value ------------------------------------------------------------

func (s *Store) Get(k string) string {
	var v string
	s.DB.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	return v
}

func (s *Store) Set(k, v string) error {
	_, err := s.DB.Exec(`INSERT INTO kv(k, v) VALUES (?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// Version is a counter bumped on every change the UI should show.
func (s *Store) Bump() {
	s.DB.Exec(`INSERT INTO kv(k, v) VALUES ('version', '1') ON CONFLICT(k) DO UPDATE SET v=CAST(CAST(v AS INTEGER)+1 AS TEXT)`)
}
