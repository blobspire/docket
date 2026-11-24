package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Ideas: what the human dictates (or types) into docket. A dictation is one
// capture; the router splits it into ideas, and each idea is saved, sent to a
// running session, or researched by a session docket starts for it.

// Dictation statuses.
const (
	DictRecording    = "recording"    // audio chunks are arriving
	DictTranscribing = "transcribing" // waiting for (or in) speech-to-text
	DictDraft        = "draft"        // transcribed; waits for the human to send it
	DictSent         = "sent"         // waits for the router
	DictRouting      = "routing"
	DictRouted       = "routed"
	DictFailed       = "failed"
	DictDeleted      = "deleted"
)

// Idea statuses.
const (
	IdeaQueued      = "queued"          // waits for a slot and a place to run
	IdeaWaiting     = "waiting_machine" // its only home is a machine that's asleep
	IdeaStarting    = "starting"        // a session was asked to start
	IdeaResearching = "researching"
	IdeaProposed    = "proposed" // the proposal waits for the human
	IdeaBuilding    = "building"
	IdeaBuilt       = "built"
	IdeaSaved       = "saved" // on the backlog
	IdeaSent        = "sent"  // handed to a session that was already running
	IdeaDismissed   = "dismissed"
	IdeaFailed      = "failed"
)

// IdeaTerminal reports whether an idea needs nothing more from docket.
func IdeaTerminal(status string) bool {
	switch status {
	case IdeaBuilt, IdeaSaved, IdeaSent, IdeaDismissed:
		return true
	}
	return false
}

// IdeaRunning reports whether an idea holds one of the running-session slots.
func IdeaRunning(status string) bool {
	return status == IdeaStarting || status == IdeaResearching || status == IdeaBuilding
}

type Dictation struct {
	ID, Login, Source, Status, AudioSHA, Transcript, Text, ProjectHint, Error string
	AudioMS, NextSeq                                                          int64
	AutoSend                                                                  bool
	Attempts                                                                  int
	CreatedAt, UpdatedAt, SentAt                                              int64
}

const dictCols = `id, login, source, status, audio_sha, audio_ms, next_seq, transcript, text, project_hint, auto_send, error, attempts, created_at, updated_at, sent_at`

func scanDict(sc interface{ Scan(...any) error }) (Dictation, error) {
	var d Dictation
	var auto int
	err := sc.Scan(&d.ID, &d.Login, &d.Source, &d.Status, &d.AudioSHA, &d.AudioMS, &d.NextSeq, &d.Transcript, &d.Text, &d.ProjectHint, &auto, &d.Error, &d.Attempts, &d.CreatedAt, &d.UpdatedAt, &d.SentAt)
	d.AutoSend = auto != 0
	return d, err
}

// AddDictation stores a new dictation.
func (s *Store) AddDictation(d Dictation) error {
	t := now()
	if d.CreatedAt == 0 {
		d.CreatedAt = t
	}
	_, err := s.DB.Exec(`INSERT INTO dictations(`+dictCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Login, d.Source, d.Status, d.AudioSHA, d.AudioMS, d.NextSeq, d.Transcript, d.Text, d.ProjectHint, boolInt(d.AutoSend), d.Error, d.Attempts, d.CreatedAt, t, d.SentAt)
	if err == nil {
		s.bumpIdeas()
	}
	return err
}

// GetDictation returns the dictation, or ok=false.
func (s *Store) GetDictation(id string) (Dictation, bool, error) {
	d, err := scanDict(s.DB.QueryRow(`SELECT `+dictCols+` FROM dictations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Dictation{}, false, nil
	}
	return d, err == nil, err
}

// EditDictation changes a dictation in one transaction: fn sees the current
// row and edits it; returning an error leaves it unchanged. fn must not use
// the store (the transaction holds its only connection).
func (s *Store) EditDictation(id string, fn func(*Dictation) error) (Dictation, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return Dictation{}, err
	}
	defer tx.Rollback()
	d, err := scanDict(tx.QueryRow(`SELECT `+dictCols+` FROM dictations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Dictation{}, errors.New("no such dictation")
	}
	if err != nil {
		return Dictation{}, err
	}
	if err := fn(&d); err != nil {
		return d, err
	}
	d.UpdatedAt = now()
	if _, err := tx.Exec(`UPDATE dictations SET login=?, source=?, status=?, audio_sha=?, audio_ms=?, next_seq=?, transcript=?, text=?, project_hint=?,
		auto_send=?, error=?, attempts=?, updated_at=?, sent_at=? WHERE id=?`,
		d.Login, d.Source, d.Status, d.AudioSHA, d.AudioMS, d.NextSeq, d.Transcript, d.Text, d.ProjectHint, boolInt(d.AutoSend), d.Error, d.Attempts, d.UpdatedAt, d.SentAt, d.ID); err != nil {
		return d, err
	}
	if err := tx.Commit(); err != nil {
		return d, err
	}
	s.bumpIdeas()
	return d, nil
}

// Dictations lists dictations with any of the statuses (all when none),
// oldest first.
func (s *Store) Dictations(statuses ...string) ([]Dictation, error) {
	q := `SELECT ` + dictCols + ` FROM dictations`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	rows, err := s.DB.Query(q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dictation
	for rows.Next() {
		d, err := scanDict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecentDictations lists dictations created since t, newest first.
func (s *Store) RecentDictations(since int64) ([]Dictation, error) {
	rows, err := s.DB.Query(`SELECT `+dictCols+` FROM dictations WHERE created_at>=? AND status<>? ORDER BY created_at DESC, id DESC`, since, DictDeleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dictation
	for rows.Next() {
		d, err := scanDict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// OldAudio lists dictations whose audio was kept since before t.
func (s *Store) OldAudio(before int64) ([]Dictation, error) {
	rows, err := s.DB.Query(`SELECT `+dictCols+` FROM dictations WHERE audio_sha<>'' AND created_at<? AND status NOT IN (?,?,?)`, before, DictRecording, DictTranscribing, DictFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dictation
	for rows.Next() {
		d, err := scanDict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- ideas ----------------------------------------------------------------

type Idea struct {
	ID, DictationID, Project, Title, Body, Route, Reason, Status string
	Machine, Dir, Place, SessionName, SessionID, ItemID, Note    string
	Error                                                        string
	Attempts                                                     int
	CreatedAt, UpdatedAt, StartedAt, EndedAt, StoppedAt          int64
	ResetAt                                                      int64 // the human asked for a fresh try: earlier failed starts no longer count
}

const ideaCols = `id, dictation_id, project, title, body, route, reason, status, machine, dir, place, session_name, session_id, item_id, note, error, attempts, created_at, updated_at, started_at, ended_at, stopped_at, reset_at`

func scanIdea(sc interface{ Scan(...any) error }) (Idea, error) {
	var i Idea
	err := sc.Scan(&i.ID, &i.DictationID, &i.Project, &i.Title, &i.Body, &i.Route, &i.Reason, &i.Status, &i.Machine, &i.Dir, &i.Place, &i.SessionName, &i.SessionID, &i.ItemID, &i.Note, &i.Error, &i.Attempts, &i.CreatedAt, &i.UpdatedAt, &i.StartedAt, &i.EndedAt, &i.StoppedAt, &i.ResetAt)
	return i, err
}

// AddIdea stores a new idea.
func (s *Store) AddIdea(i Idea) error {
	t := now()
	if i.CreatedAt == 0 {
		i.CreatedAt = t
	}
	if IdeaTerminal(i.Status) && i.EndedAt == 0 {
		i.EndedAt = t
	}
	_, err := s.DB.Exec(`INSERT INTO ideas(`+ideaCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		i.ID, i.DictationID, i.Project, i.Title, i.Body, i.Route, i.Reason, i.Status, i.Machine, i.Dir, i.Place, i.SessionName, i.SessionID, i.ItemID, i.Note, i.Error, i.Attempts, i.CreatedAt, t, i.StartedAt, i.EndedAt, i.StoppedAt, i.ResetAt)
	if err == nil {
		s.bumpIdeas()
	}
	return err
}

// GetIdea returns the idea, or ok=false.
func (s *Store) GetIdea(id string) (Idea, bool, error) {
	i, err := scanIdea(s.DB.QueryRow(`SELECT `+ideaCols+` FROM ideas WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Idea{}, false, nil
	}
	return i, err == nil, err
}

// EditIdea changes an idea in one transaction (see EditDictation). It keeps
// ended_at in step with the status.
func (s *Store) EditIdea(id string, fn func(*Idea) error) (Idea, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return Idea{}, err
	}
	defer tx.Rollback()
	i, err := scanIdea(tx.QueryRow(`SELECT `+ideaCols+` FROM ideas WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Idea{}, errors.New("no such idea")
	}
	if err != nil {
		return Idea{}, err
	}
	was := i.Status
	if err := fn(&i); err != nil {
		return i, err
	}
	t := now()
	i.UpdatedAt = t
	switch {
	case IdeaTerminal(i.Status) && (!IdeaTerminal(was) || i.EndedAt == 0):
		i.EndedAt = t
	case !IdeaTerminal(i.Status):
		i.EndedAt = 0
	}
	if _, err := tx.Exec(`UPDATE ideas SET dictation_id=?, project=?, title=?, body=?, route=?, reason=?, status=?, machine=?, dir=?, place=?, session_name=?,
		session_id=?, item_id=?, note=?, error=?, attempts=?, updated_at=?, started_at=?, ended_at=?, stopped_at=?, reset_at=? WHERE id=?`,
		i.DictationID, i.Project, i.Title, i.Body, i.Route, i.Reason, i.Status, i.Machine, i.Dir, i.Place, i.SessionName,
		i.SessionID, i.ItemID, i.Note, i.Error, i.Attempts, i.UpdatedAt, i.StartedAt, i.EndedAt, i.StoppedAt, i.ResetAt, i.ID); err != nil {
		return i, err
	}
	if err := tx.Commit(); err != nil {
		return i, err
	}
	s.bumpIdeas()
	return i, nil
}

func (s *Store) ideaList(where string, args ...any) ([]Idea, error) {
	rows, err := s.DB.Query(`SELECT `+ideaCols+` FROM ideas `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Idea
	for rows.Next() {
		i, err := scanIdea(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Ideas lists ideas with any of the statuses (all when none), oldest first.
func (s *Store) Ideas(statuses ...string) ([]Idea, error) {
	if len(statuses) == 0 {
		return s.ideaList(`ORDER BY created_at, id`)
	}
	args := make([]any, len(statuses))
	for i, st := range statuses {
		args[i] = st
	}
	return s.ideaList(`WHERE status IN (`+placeholders(len(statuses))+`) ORDER BY created_at, id`, args...)
}

// IdeasOf lists a dictation's ideas.
func (s *Store) IdeasOf(dictationID string) ([]Idea, error) {
	return s.ideaList(`WHERE dictation_id=? ORDER BY created_at, id`, dictationID)
}

// RecentIdeas lists ideas that are still going or changed since t, newest first.
func (s *Store) RecentIdeas(since int64) ([]Idea, error) {
	return s.ideaList(`WHERE status NOT IN (?,?,?,?) OR updated_at>=? ORDER BY created_at DESC, id DESC`, IdeaBuilt, IdeaSent, IdeaDismissed, IdeaSaved, since)
}

// IdeaByItem finds the idea a proposal item belongs to.
func (s *Store) IdeaByItem(itemID string) (Idea, bool) {
	if itemID == "" {
		return Idea{}, false
	}
	l, _ := s.ideaList(`WHERE item_id=? LIMIT 1`, itemID)
	if len(l) == 0 {
		return Idea{}, false
	}
	return l[0], true
}

// IdeaItems maps proposal items to their ideas, for marking rows.
func (s *Store) IdeaItems() map[string]string {
	out := map[string]string{}
	rows, err := s.DB.Query(`SELECT item_id, id FROM ideas WHERE item_id<>''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var item, id string
		if rows.Scan(&item, &id) == nil {
			out[item] = id
		}
	}
	return out
}

// IdeaSessionSeen links an idea to the session docket started for it the
// first time that session reports in. It returns whether anything changed.
func (s *Store) IdeaSessionSeen(ideaID, sessionID, machine, run string) (bool, error) {
	if ideaID == "" || sessionID == "" {
		return false, nil
	}
	i, ok, err := s.GetIdea(ideaID)
	if err != nil || !ok || i.SessionID == sessionID || i.SessionID != "" {
		return false, err
	}
	if run != "" && i.SessionName != "" && run != i.SessionName {
		return false, nil // an earlier run of this idea; the current one is another session
	}
	_, err = s.EditIdea(ideaID, func(i *Idea) error {
		if i.SessionID != "" {
			return nil
		}
		i.SessionID = sessionID
		if machine != "" {
			i.Machine = machine
		}
		if i.Status == IdeaStarting || i.Status == IdeaQueued || i.Status == IdeaWaiting {
			if i.Route == "build" {
				i.Status = IdeaBuilding
			} else {
				i.Status = IdeaResearching
			}
			i.Error = ""
		}
		return nil
	})
	return err == nil, err
}

func (s *Store) bumpIdeas() {
	s.DB.Exec(`INSERT INTO kv(k, v) VALUES ('ideas:version', '1') ON CONFLICT(k) DO UPDATE SET v=CAST(CAST(v AS INTEGER)+1 AS TEXT)`)
}

// Version changes whenever the queue or the ideas do: the web pages poll it
// to refresh in place.
func (s *Store) Version() string {
	return s.QueueDigest() + "." + s.Get("ideas:version") + "." + s.Get("email:bump") + "." + s.progressDigest()
}

// ---- where a project's code lives -----------------------------------------

// Home is one folder that holds a project's code.
type Home struct {
	Machine, Dir, Remote string
	Clone                bool // docket cloned it for ideas
	Votes                int
}

// ProjectHomes lists the folders mapped to a project: originals first, then
// the most voted.
func (s *Store) ProjectHomes(project string) ([]Home, error) {
	rows, err := s.DB.Query(`SELECT machine, dir, remote, clone, votes FROM project_dirs WHERE project=? ORDER BY clone, votes DESC, updated_at DESC, machine, dir`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Home
	for rows.Next() {
		var h Home
		var clone int
		if err := rows.Scan(&h.Machine, &h.Dir, &h.Remote, &clone, &h.Votes); err != nil {
			return nil, err
		}
		h.Clone = clone != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetDirRemote records a mapped folder's git remote.
func (s *Store) SetDirRemote(machine, dir, remote string) error {
	dir = strings.TrimRight(dir, "/")
	if machine == "" || dir == "" || remote == "" {
		return nil
	}
	_, err := s.DB.Exec(`UPDATE project_dirs SET remote=? WHERE machine=? AND dir=? AND remote<>?`, remote, machine, dir, remote)
	return err
}

// AddClone maps a folder docket cloned a project's repository into.
func (s *Store) AddClone(machine, dir, project, remote string) error {
	dir = strings.TrimRight(dir, "/")
	_, err := s.DB.Exec(`INSERT INTO project_dirs(machine, dir, project, updated_at, votes, remote, clone) VALUES (?,?,?,?,1,?,1)
		ON CONFLICT(machine, dir) DO UPDATE SET remote=excluded.remote`, machine, dir, project, now(), remote)
	return err
}

// ---- session starts handed to push machines --------------------------------

// Start is a session docket wants started (or stopped) on a machine it can't
// reach; the machine picks it up at its next sync.
type Start struct {
	ID, Machine, Op, Name, Cwd, IdeaID, Prompt, Status, Error string
	Resume                                                    string // a session to resume (and hand Prompt to)
	Args                                                      []string
	CreatedAt, SentAt                                         int64
}

const startCols = `id, machine, op, name, cwd, idea_id, prompt, args, status, error, created_at, sent_at, resume`

func scanStart(sc interface{ Scan(...any) error }) (Start, error) {
	var st Start
	var args string
	err := sc.Scan(&st.ID, &st.Machine, &st.Op, &st.Name, &st.Cwd, &st.IdeaID, &st.Prompt, &args, &st.Status, &st.Error, &st.CreatedAt, &st.SentAt, &st.Resume)
	_ = json.Unmarshal([]byte(args), &st.Args)
	return st, err
}

// AddStart queues a start (or stop) for a machine.
func (s *Store) AddStart(st Start) error {
	args, _ := json.Marshal(st.Args)
	if st.Args == nil {
		args = []byte("[]")
	}
	if st.Op == "" {
		st.Op = "start"
	}
	_, err := s.DB.Exec(`INSERT INTO starts(`+startCols+`) VALUES (?,?,?,?,?,?,?,?,'pending','',?,0,?)`,
		st.ID, st.Machine, st.Op, st.Name, st.Cwd, st.IdeaID, st.Prompt, string(args), now(), st.Resume)
	return err
}

// Starts lists starts with a status (all when ""), for a machine (all when "").
func (s *Store) Starts(machine, status string) ([]Start, error) {
	q := `SELECT ` + startCols + ` FROM starts WHERE 1=1`
	var args []any
	if machine != "" {
		q += ` AND machine=?`
		args = append(args, machine)
	}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	rows, err := s.DB.Query(q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Start
	for rows.Next() {
		st, err := scanStart(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StartsForIdea lists an idea's starts, newest first.
func (s *Store) StartsForIdea(ideaID string) ([]Start, error) {
	rows, err := s.DB.Query(`SELECT `+startCols+` FROM starts WHERE idea_id=? ORDER BY created_at DESC, id DESC`, ideaID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Start
	for rows.Next() {
		st, err := scanStart(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// StartSent marks a start handed to its machine. It reports whether this
// call was the one that did (a start is only ever handed out once).
func (s *Store) StartSent(id string) bool {
	res, err := s.DB.Exec(`UPDATE starts SET status='sent', sent_at=? WHERE id=? AND status='pending'`, now(), id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// ExpireStart withdraws a start no machine has picked up yet. It reports
// whether it did (a start already handed out can't be withdrawn).
func (s *Store) ExpireStart(id string) bool {
	res, err := s.DB.Exec(`UPDATE starts SET status='expired', error='withdrawn: never picked up' WHERE id=? AND status='pending'`, id)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// StartResult records what the machine reported. Only a start that is still
// waiting for its answer changes.
func (s *Store) StartResult(id string, ok bool, msg string) (Start, bool, error) {
	status := "started"
	if !ok {
		status = "failed"
	}
	res, err := s.DB.Exec(`UPDATE starts SET status=?, error=? WHERE id=? AND status IN ('pending','sent')`, status, msg, id)
	if err != nil {
		return Start{}, false, err
	}
	n, _ := res.RowsAffected()
	st, err := scanStart(s.DB.QueryRow(`SELECT `+startCols+` FROM starts WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Start{}, false, nil
	}
	return st, n > 0, err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
