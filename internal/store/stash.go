package store

import (
	"errors"
	"strconv"
	"strings"
)

// Phrases in the messages docket sends agents. Deliveries record their
// kind; for older rows (made before kinds), the phrase tells.
const (
	AnswerMark = "(from the docket queue):"      // the human's answer
	DoneMark   = "marked your docket item done:" // a done, or a stash that closed it
	NoiseMark  = "not worth their time"          // "not worth my time"
)

// kindIs matches deliveries of a kind, by their kind or (older rows) their
// text; isAnswer is the human's answers.
var kindIs = map[string]string{
	"answer": `(kind='answer' OR (kind='' AND live_only=0 AND instr(text, '` + AnswerMark + `')>0))`,
	"done":   `(kind='done' OR (kind='' AND (instr(text, '` + DoneMark + `')>0 OR instr(text, 'marked this done:')>0)))`,
	"noise":  `(kind='noise' OR (kind='' AND instr(text, '` + NoiseMark + `')>0))`,
}
var isAnswer = kindIs["answer"]

// The Stash keeps items the human has dealt with but wants close at hand
// (results to share, a quote to act on later) without leaving them on the
// docket. An item stays there for StashDays, then counts as done.
const (
	StashDays    = 30
	StashSeconds = StashDays * 24 * 60 * 60
)

// StashCutoff is the earliest stash time still on the Stash.
func StashCutoff() int64 { return now() - StashSeconds }

// Stash puts an item on the Stash. One still waiting on the human (open or
// snoozed) is closed as done at the same time; one waiting on its agent (an
// answer on its way or delivered) or already closed keeps its status. It is
// one logged decision, so Undo restores both. closed reports whether this
// closed the item.
func (s *Store) Stash(id string) (closed bool, err error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var prev string
	var was int64
	if err := tx.QueryRow(`SELECT status, stashed_at FROM items WHERE id=?`, id).Scan(&prev, &was); err != nil {
		return false, err
	}
	if was >= StashCutoff() && !needsHuman(prev) {
		return false, errors.New("it's already on your Stash")
	}
	t := now()
	status := prev
	if prev == "open" || prev == "snoozed" || prev == "maybe" {
		status, closed = "done", true
	}
	if _, err := tx.Exec(`UPDATE items SET status=?, stashed_at=?, updated_at=?,
		resolution=CASE WHEN ? THEN 'Stashed' ELSE resolution END,
		resolved_at=CASE WHEN ? THEN ? ELSE resolved_at END,
		snoozed_until=CASE WHEN ? THEN NULL ELSE snoozed_until END WHERE id=?`,
		status, t, t, closed, closed, t, closed, id); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, answer, prev_status) VALUES (?,?,'stash',?,?)`, id, t, strconv.FormatInt(was, 10), prev); err != nil {
		return false, err
	}
	return closed, tx.Commit()
}

// Unstash takes an item off the Stash. toDone closes it too (one whose
// answer is with its agent included), so it moves to Done; otherwise it keeps
// its status and goes back to wherever that puts it.
func (s *Store) Unstash(id string, toDone bool) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev string
	var was int64
	if err := tx.QueryRow(`SELECT status, stashed_at FROM items WHERE id=?`, id).Scan(&prev, &was); err != nil {
		return err
	}
	if was == 0 || needsHuman(prev) {
		return errors.New("it isn't on your Stash")
	}
	t := now()
	status := prev
	if toDone && !closedStatus(prev) {
		status = "done"
	}
	if _, err := tx.Exec(`UPDATE items SET status=?, stashed_at=0, updated_at=?,
		resolved_at=CASE WHEN ? <> ? THEN ? ELSE resolved_at END WHERE id=?`, status, t, status, prev, t, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, answer, prev_status) VALUES (?,?,'unstash',?,?)`, id, t, strconv.FormatInt(was, 10), prev); err != nil {
		return err
	}
	return tx.Commit()
}

func needsHuman(status string) bool {
	return status == "open" || status == "snoozed" || status == "maybe"
}

// ClearStaleStash forgets stash times on items that need the human again
// (an older docket's Undo or Reopen leaves them behind).
func (s *Store) ClearStaleStash() error {
	_, err := s.DB.Exec(`UPDATE items SET stashed_at=0 WHERE stashed_at<>0 AND status IN ('open','snoozed','maybe')`)
	return err
}

// onStash is what the Stash holds: stashed in the last 30 days, and not
// needing the human (an open item with a stash time, which an older docket
// can leave behind, is on the docket instead).
const onStash = `stashed_at >= ? AND status NOT IN ('open','snoozed','maybe')`

// Stashed lists the items on the Stash, the most recently stashed first.
func (s *Store) Stashed(limit int) ([]Item, error) {
	return s.items(`SELECT `+itemCols+` FROM items WHERE `+onStash+` ORDER BY stashed_at DESC LIMIT ?`, StashCutoff(), limit)
}

// StashCount is how many items are on the Stash.
func (s *Store) StashCount() int {
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM items WHERE `+onStash, StashCutoff()).Scan(&n)
	return n
}

// DropStash takes an item off the Stash without logging it.
func (s *Store) DropStash(id string) error {
	_, err := s.DB.Exec(`UPDATE items SET stashed_at=0 WHERE id=? AND stashed_at<>0`, id)
	return err
}

// TopDecision is the latest logged decision on an item: its id, action and
// time, and the status the item had before it.
func (s *Store) TopDecision(id string) (did int64, action, prev string, ts int64) {
	s.DB.QueryRow(`SELECT id, action, prev_status, ts FROM decisions WHERE item_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&did, &action, &prev, &ts)
	return
}

// DeliveredBySession finds the session's own item with a delivered answer
// filed with this key: the exact project-scoped key first, then (unless the
// project was named) the agent's key in any project.
func (s *Store) DeliveredBySession(projectKey, k, session string, anyProject bool) (Item, bool) {
	if session == "" {
		return Item{}, false
	}
	rs, _ := s.items(`SELECT `+itemCols+` FROM items WHERE dedupe_key LIKE 'key:%' AND status='delivered' AND session_id=? ORDER BY updated_at DESC, rowid DESC`, session)
	for _, it := range rs {
		if it.DedupeKey == projectKey {
			return it, true
		}
	}
	if anyProject {
		for _, it := range rs {
			if AgentKey(it.DedupeKey) == k {
				return it, true
			}
		}
	}
	return Item{}, false
}

// DeliveredOnMachine finds an item with a delivered answer filed with
// exactly this project-scoped key from the same machine within a week (a
// session that was cleared or resumed under a new id). Never by the bare
// agent key: another session's item in another project could match.
func (s *Store) DeliveredOnMachine(projectKey, machine string) (Item, bool) {
	if machine == "" || projectKey == "" {
		return Item{}, false
	}
	it, err := scanItem(s.DB.QueryRow(`SELECT `+itemCols+` FROM items WHERE dedupe_key=? AND status='delivered' AND machine=? AND updated_at>=?
		ORDER BY updated_at DESC, rowid DESC LIMIT 1`, projectKey, machine, now()-7*24*60*60))
	return it, err == nil
}

// WaitWindow is how long an item whose answer reached its agent stays on the
// docket (under "Waiting on agents") for the agent to close it.
const WaitWindow = 24 * 60 * 60

// DocketItems lists what the docket shows: items that need the human (open,
// snoozed), and ones waiting on their agent (an answer on its way, or
// delivered within WaitWindow) unless they're on the Stash.
func (s *Store) DocketItems() ([]Item, error) {
	return s.items(`SELECT `+itemCols+` FROM items WHERE status IN ('open','snoozed')
		OR (stashed_at < ? AND (status='answered' OR (status='delivered' AND updated_at >= ?))) ORDER BY created_at DESC`, StashCutoff(), now()-WaitWindow)
}

func (s *Store) items(q string, args ...any) ([]Item, error) {
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

// Touch marks an item as just changed (a delivered answer brought back from
// the Stash waits on the docket for another day).
func (s *Store) Touch(id string) error {
	_, err := s.DB.Exec(`UPDATE items SET updated_at=? WHERE id=?`, now(), id)
	return err
}

// in builds "?,?,?" and its arguments for a list of ids.
func in(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// EvidenceFor returns the evidence of each of these items.
func (s *Store) EvidenceFor(ids []string) map[string][]Evidence {
	out := map[string][]Evidence{}
	if len(ids) == 0 {
		return out
	}
	q, args := in(ids)
	rows, err := s.DB.Query(`SELECT sha256, item_id, name, mime, size, fetched FROM evidence WHERE item_id IN (`+q+`) ORDER BY item_id, name`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var e Evidence
		var fetched int
		if rows.Scan(&e.SHA256, &e.ItemID, &e.Name, &e.Mime, &e.Size, &fetched) == nil {
			e.Fetched, e.Gone = fetched > 0, fetched < 0
			out[e.ItemID] = append(out[e.ItemID], e)
		}
	}
	return out
}

// LatestDeliveries returns each item's most recent answer delivery (not the
// quiet notes docket sends on its own).
func (s *Store) LatestDeliveries(ids []string) map[string]Delivery {
	out := map[string]Delivery{}
	if len(ids) == 0 {
		return out
	}
	q, args := in(ids)
	rows, err := s.DB.Query(`SELECT `+deliveryCols+` FROM deliveries WHERE live_only=0 AND `+isAnswer+` AND item_id IN (`+q+`) ORDER BY created_at, rowid`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		if d, err := scanDelivery(rows); err == nil {
			out[d.ItemID] = d // oldest first, so the latest wins
		}
	}
	return out
}

// LastDecisions returns the most recent logged action on each of these items.
func (s *Store) LastDecisions(ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	q, args := in(ids)
	rows, err := s.DB.Query(`SELECT item_id, action FROM decisions WHERE item_id IN (`+q+`) ORDER BY id`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, a string
		if rows.Scan(&id, &a) == nil {
			out[id] = a
		}
	}
	return out
}
