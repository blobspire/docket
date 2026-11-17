package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---- projects ---------------------------------------------------------------

// Project groups items. Agents choose (or create) one for everything they file.
type Project struct {
	Name        string
	Description string
	CreatedAt   int64
	UpdatedAt   int64
	Archived    bool
}

// Unsorted is where items land when nothing says which project they belong to.
const Unsorted = "unsorted"

var slugBad = regexp.MustCompile(`[^a-z0-9]+`)

// Slug normalises a project, component or place name: lowercase letters,
// digits and dashes, at most 40 characters.
func Slug(s string) string {
	s = strings.Trim(slugBad.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// EnsureProject creates the project if it doesn't exist. A description fills
// in an empty one but never replaces what is there.
func (s *Store) EnsureProject(name, desc string) error {
	if name == "" || name == Unsorted {
		return nil
	}
	t := now()
	if _, err := s.DB.Exec(`INSERT OR IGNORE INTO projects(name, description, created_at, updated_at) VALUES (?,?,?,?)`, name, desc, t, t); err != nil {
		return err
	}
	if desc != "" {
		_, err := s.DB.Exec(`UPDATE projects SET description=?, updated_at=? WHERE name=? AND description=''`, desc, t, name)
		return err
	}
	return nil
}

// SetProject updates a project's description and archived flag.
func (s *Store) SetProject(name, desc string, archived bool) error {
	_, err := s.DB.Exec(`UPDATE projects SET description=?, archived=?, updated_at=? WHERE name=?`, desc, boolInt(archived), now(), name)
	return err
}

func (s *Store) Projects(includeArchived bool) ([]Project, error) {
	q := `SELECT name, description, created_at, updated_at, archived FROM projects`
	if !includeArchived {
		q += ` WHERE archived=0`
	}
	rows, err := s.DB.Query(q + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var arch int
		if err := rows.Scan(&p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt, &arch); err != nil {
			return nil, err
		}
		p.Archived = arch != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(name string) (Project, bool) {
	var p Project
	var arch int
	err := s.DB.QueryRow(`SELECT name, description, created_at, updated_at, archived FROM projects WHERE name=?`, name).Scan(&p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt, &arch)
	p.Archived = arch != 0
	return p, err == nil
}

// RenameProject renames from to to, or merges it into to when to exists. Its
// items, folder mappings and project-scoped keys move along. When both
// projects have an active item with the same key, the moved one is closed as
// merged.
func (s *Store) RenameProject(from, to string) error {
	if from == "" || to == "" || from == to {
		return errors.New("rename needs two different project names")
	}
	if to == Unsorted {
		return errors.New(`"unsorted" is where items without a project land; pick a real project name`)
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	var exists int
	tx.QueryRow(`SELECT COUNT(*) FROM projects WHERE name=?`, to).Scan(&exists)
	if exists == 0 {
		if _, err := tx.Exec(`INSERT INTO projects(name, description, created_at, updated_at) SELECT ?, description, created_at, ? FROM projects WHERE name=?`, to, t, from); err != nil {
			return err
		}
		tx.Exec(`INSERT OR IGNORE INTO projects(name, created_at, updated_at) VALUES (?,?,?)`, to, t, t)
	}
	q := `SELECT id, COALESCE(dedupe_key,''), status FROM items WHERE project=?`
	if from == Unsorted {
		q = `SELECT id, COALESCE(dedupe_key,''), status FROM items WHERE project IN ('', ?)`
	}
	rows, err := tx.Query(q, from)
	if err != nil {
		return err
	}
	type row struct{ id, key, status string }
	var moved []row
	for rows.Next() {
		var r row
		rows.Scan(&r.id, &r.key, &r.status)
		moved = append(moved, r)
	}
	rows.Close()
	for _, r := range moved {
		// Every moved key joins the new project's scope (legacy keys from an
		// older docket included), so later filings there find it.
		key := r.key
		if k := AgentKey(key); k != "" {
			key = "key:" + to + ":" + k
		}
		if key != r.key && activeStatus(r.status) {
			var other string
			if tx.QueryRow(`SELECT id FROM items WHERE dedupe_key=? AND status IN ('maybe','open','snoozed','answered')`, key).Scan(&other) == nil {
				if _, err := tx.Exec(`UPDATE items SET status='done', resolution=?, resolved_at=?, updated_at=? WHERE id=?`,
					"merged into "+other, t, t, r.id); err != nil {
					return err
				}
				// The agent that filed the merged item still knows its id.
				tx.Exec(`INSERT OR IGNORE INTO item_aliases(alias, item_id, created_at) VALUES (?,?,?)`, r.id, other, t)
				r.status = "done"
			}
		}
		var k any
		if key != "" {
			k = key
		}
		if _, err := tx.Exec(`UPDATE items SET project=?, dedupe_key=?, updated_at=? WHERE id=?`, to, k, t, r.id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE project_dirs SET project=?, updated_at=? WHERE project=?`, to, t, from); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM projects WHERE name=?`, from); err != nil {
		return err
	}
	return tx.Commit()
}

func activeStatus(st string) bool {
	return st == "maybe" || st == "open" || st == "snoozed" || st == "answered"
}

// ---- folder → project ---------------------------------------------------------

type ProjectDir struct {
	Machine, Dir, Project string
}

// LearnDir counts a filing's project for its folder. The mapping follows
// the majority: an explicit choice of the current project, or a filing that
// used it as the default, adds a vote; an explicit different choice takes
// one away and wins the folder only when none are left. So one errand filed
// from a repo doesn't re-home it.
func (s *Store) LearnDir(machine, dir, project string, explicit bool) error {
	dir = strings.TrimRight(dir, "/")
	if machine == "" || dir == "" || project == "" || project == Unsorted {
		return nil
	}
	t := now()
	var cur string
	var votes int
	err := s.DB.QueryRow(`SELECT project, votes FROM project_dirs WHERE machine=? AND dir=?`, machine, dir).Scan(&cur, &votes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !explicit {
			return nil // a default from elsewhere (a lead, a parent folder) teaches nothing
		}
		_, err = s.DB.Exec(`INSERT OR IGNORE INTO project_dirs(machine, dir, project, updated_at, votes) VALUES (?,?,?,?,1)`, machine, dir, project, t)
	case err != nil:
	case cur == project:
		_, err = s.DB.Exec(`UPDATE project_dirs SET votes=MIN(votes+1, 20) WHERE machine=? AND dir=?`, machine, dir)
	case !explicit:
	case votes <= 1:
		_, err = s.DB.Exec(`UPDATE project_dirs SET project=?, votes=1, updated_at=? WHERE machine=? AND dir=?`, project, t, machine, dir)
	default:
		_, err = s.DB.Exec(`UPDATE project_dirs SET votes=votes-1 WHERE machine=? AND dir=?`, machine, dir)
	}
	return err
}

// SetDir sets a folder's project, replacing any earlier mapping.
func (s *Store) SetDir(machine, dir, project string) error {
	dir = strings.TrimRight(dir, "/")
	_, err := s.DB.Exec(`INSERT INTO project_dirs(machine, dir, project, updated_at, votes) VALUES (?,?,?,?,5)
		ON CONFLICT(machine, dir) DO UPDATE SET project=excluded.project, updated_at=excluded.updated_at, votes=5`, machine, dir, project, now())
	return err
}

// ProjectDirs lists folder mappings, for one machine or all ("").
func (s *Store) ProjectDirs(machine string) ([]ProjectDir, error) {
	q := `SELECT machine, dir, project FROM project_dirs`
	var args []any
	if machine != "" {
		q += ` WHERE machine=?`
		args = append(args, machine)
	}
	rows, err := s.DB.Query(q+` ORDER BY machine, dir`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectDir
	for rows.Next() {
		var d ProjectDir
		if err := rows.Scan(&d.Machine, &d.Dir, &d.Project); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ProjectForDir finds the project of the deepest mapped folder containing cwd.
func ProjectForDir(dirs []ProjectDir, cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	best, bestLen := "", -1
	for _, d := range dirs {
		if (cwd == d.Dir || strings.HasPrefix(cwd, d.Dir+"/")) && len(d.Dir) > bestLen {
			best, bestLen = d.Project, len(d.Dir)
		}
	}
	return best
}

// ---- aliases and lookups --------------------------------------------------------

// AddAlias records that alias (an id an agent session on machine was told)
// means itemID.
func (s *Store) AddAlias(alias, itemID, machine, session string) error {
	if alias == "" || alias == itemID {
		return nil
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO item_aliases(alias, item_id, machine, created_at, session_id) VALUES (?,?,?,?,?)`, alias, itemID, machine, now(), session)
	return err
}

// LatestAlias is the id this session was most recently told for the item,
// when that differs from the item's own id ("" otherwise).
func (s *Store) LatestAlias(itemID, session string) string {
	if session == "" {
		return ""
	}
	var a string
	s.DB.QueryRow(`SELECT alias FROM item_aliases WHERE item_id=? AND session_id=? ORDER BY rowid DESC LIMIT 1`, itemID, session).Scan(&a)
	return a
}

// SeenIDs lists ids the server has processed from a machine since a time:
// its items and the aliases of re-filings. A machine's local record of what
// it filed can then defer to the server for exactly those.
func (s *Store) SeenIDs(machine string, since int64) []string {
	var out []string
	// Items and re-filing aliases, plus the done and project records the
	// server has applied, so the machine can drop its own copies of those.
	ts := time.Unix(since, 0).UTC().Format(time.RFC3339)
	// Every filing this machine made (its item id, even if another machine
	// has since re-filed the item), plus items and aliases recorded for it.
	rows, err := s.DB.Query(`SELECT id FROM items WHERE machine=? AND created_at>=?
		UNION SELECT alias FROM item_aliases WHERE machine=? AND created_at>=?
		UNION SELECT id FROM events WHERE machine=? AND event IN ('cli.done','cli.project') AND processed=1 AND ts>=?
		UNION SELECT json_extract(body,'$.item.id') FROM events WHERE machine=? AND event='cli.file' AND processed=1 AND ts>=? AND json_extract(body,'$.item.id') IS NOT NULL
		ORDER BY 1`, machine, since, machine, since, machine, ts, machine, ts)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

// Aliases lists the other ids agents were told for an item.
func (s *Store) Aliases(itemID string) []string {
	var out []string
	rows, err := s.DB.Query(`SELECT alias FROM item_aliases WHERE item_id=? ORDER BY rowid`, itemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

// AgentKey is the --key an agent gave, without docket's project scope.
func AgentKey(dedupe string) string {
	if !strings.HasPrefix(dedupe, "key:") {
		return ""
	}
	rest := strings.TrimPrefix(dedupe, "key:")
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// ResolveItem finds an item by id or by an alias of it.
func (s *Store) ResolveItem(id string) (Item, bool, error) {
	it, ok, err := s.GetItem(id)
	if err != nil {
		return it, ok, err
	}
	if ok {
		// Closed by a project merge: the agent means the item it merged into.
		if it.Status == "done" && strings.HasPrefix(it.Resolution, "merged into ") {
			if into, ok2, _ := s.GetItem(strings.TrimPrefix(it.Resolution, "merged into ")); ok2 {
				return into, true, nil
			}
		}
		return it, true, nil
	}
	var real string
	if s.DB.QueryRow(`SELECT item_id FROM item_aliases WHERE alias=?`, id).Scan(&real) != nil {
		return Item{}, false, nil
	}
	return s.GetItem(real)
}

// ActiveByKey returns the active item with this exact dedupe key.
func (s *Store) ActiveByKey(key string) (Item, bool) {
	it, err := scanItem(s.DB.QueryRow(`SELECT `+itemCols+` FROM items WHERE dedupe_key=? AND status IN ('maybe','open','snoozed','answered')`, key))
	return it, err == nil
}

// ActiveByAgentKey finds an active item filed with exactly --key k (in any
// project) by the given session, or failing that from the given machine.
func (s *Store) ActiveByAgentKey(k, session, machine string) (Item, bool) {
	find := func(col, val string) (Item, bool) {
		rows, err := s.DB.Query(`SELECT `+itemCols+` FROM items WHERE dedupe_key LIKE 'key:%' AND `+col+`=? AND status IN ('maybe','open','snoozed','answered') ORDER BY created_at DESC`, val)
		if err != nil {
			return Item{}, false
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err == nil && AgentKey(it.DedupeKey) == k {
				return it, true
			}
		}
		return Item{}, false
	}
	if session != "" {
		if it, ok := find("session_id", session); ok {
			return it, true
		}
	}
	if machine != "" {
		return find("machine", machine)
	}
	return Item{}, false
}

// RecentlyNoised reports whether any item with this key was marked "not
// worth my time" since the given time, and when.
func (s *Store) RecentlyNoised(key string, since int64) (int64, bool) {
	var ts int64
	err := s.DB.QueryRow(`SELECT COALESCE(MAX(d.ts),0) FROM decisions d JOIN items i ON i.id = d.item_id
		WHERE i.dedupe_key=? AND d.action='noise' AND d.ts>=?`, key, since).Scan(&ts)
	return ts, err == nil && ts > 0
}

// ResolveKeyPrefix closes active items whose key starts with prefix.
func (s *Store) ResolveKeyPrefix(prefix, why string) (int, error) {
	like := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(prefix) + "%"
	res, err := s.DB.Exec(`UPDATE items SET status='done', resolution=?, resolved_at=?, updated_at=? WHERE dedupe_key LIKE ? ESCAPE '\' AND status IN ('open','snoozed')`, why, now(), now(), like)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---- reclassify, expiry, digest ---------------------------------------------------

type reclassified struct {
	Project string `json:"project"`
	Urgency string `json:"urgency"`
	Pinned  string `json:"pinned"`
	Key     string `json:"key,omitempty"`
}

// Reclassify moves an item to another project and/or urgency by hand. The
// changed fields are pinned so agent re-files don't undo them, and the
// previous values are logged so Undo can restore them. A human raising an item
// to "now" doesn't push their own phone.
func (s *Store) Reclassify(id, project, urgency string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev reclassified
	var status, key string
	if err := tx.QueryRow(`SELECT project, urgency, pinned, status, COALESCE(dedupe_key,'') FROM items WHERE id=?`, id).Scan(&prev.Project, &prev.Urgency, &prev.Pinned, &status, &key); err != nil {
		return err
	}
	prev.Key = key
	if (project == "" || project == prev.Project) && (urgency == "" || urgency == prev.Urgency) {
		return nil // nothing to move; nothing to undo later
	}
	pins := map[string]bool{}
	for _, p := range strings.Split(prev.Pinned, ",") {
		if p != "" {
			pins[p] = true
		}
	}
	t := now()
	if project != "" && project != prev.Project {
		if _, err := tx.Exec(`UPDATE items SET project=? WHERE id=?`, project, id); err != nil {
			return err
		}
		// The key stays as filed, so the agent's next filing (still in its
		// own project) finds the item; MovedByKey covers one in the new one.
		pins["project"] = true
	}
	if urgency != "" && urgency != prev.Urgency {
		q := `UPDATE items SET urgency=? WHERE id=?`
		if urgency == "now" {
			q = `UPDATE items SET urgency=?, urgent_since=` + fmt.Sprint(t) + `, pushed_at=` + fmt.Sprint(t) + ` WHERE id=?`
		}
		if _, err := tx.Exec(q, urgency, id); err != nil {
			return err
		}
		pins["urgency"] = true
	}
	var list []string
	for p := range pins {
		list = append(list, p)
	}
	sort.Strings(list)
	if _, err := tx.Exec(`UPDATE items SET pinned=?, updated_at=? WHERE id=?`, strings.Join(list, ","), t, id); err != nil {
		return err
	}
	b, _ := json.Marshal(prev)
	if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, answer, prev_status) VALUES (?,?,'reclassify',?,?)`, id, t, string(b), status); err != nil {
		return err
	}
	return tx.Commit()
}

func undoReclassify(tx *sql.Tx, id, answer string) error {
	var prev reclassified
	if err := json.Unmarshal([]byte(answer), &prev); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE items SET project=?, urgency=?, pinned=?, updated_at=? WHERE id=?`, prev.Project, prev.Urgency, prev.Pinned, now(), id); err != nil {
		return err
	}
	if prev.Key != "" {
		tx.Exec(`UPDATE OR IGNORE items SET dedupe_key=? WHERE id=?`, prev.Key, id)
	}
	return nil
}

// Expire closes items whose expiry has passed, logging each so it can be
// undone from the Done page.
func (s *Store) Expire(at int64) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, status FROM items WHERE expires_at > 0 AND expires_at <= ? AND status IN ('open','snoozed','answered')`, at)
	if err != nil {
		return 0, err
	}
	type row struct{ id, status string }
	var due []row
	for rows.Next() {
		var r row
		rows.Scan(&r.id, &r.status)
		due = append(due, r)
	}
	rows.Close()
	for _, r := range due {
		if _, err := tx.Exec(`UPDATE items SET status='expired', resolution='expired: no longer relevant after its expiry time', resolved_at=?, updated_at=? WHERE id=?`, at, at, r.id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, prev_status) VALUES (?,?,'expire',?)`, r.id, at, r.status); err != nil {
			return 0, err
		}
	}
	return len(due), tx.Commit()
}

// QueueDigest changes whenever what the dashboard shows does, and only
// then: every docket item's visible fields, whether its agent is waiting or
// working, how its answer's delivery stands, and what's on the Stash.
func (s *Store) QueueDigest() string {
	rows, err := s.DB.Query(`SELECT i.id, i.status, i.title, i.urgency, i.project, i.component, i.place, i.due, i.expires_at, i.updated_at, i.stashed_at,
		COALESCE(se.state,''), COALESCE((SELECT d.status||d.error FROM deliveries d WHERE d.item_id=i.id AND d.live_only=0 ORDER BY d.created_at DESC LIMIT 1),'')
		FROM items i LEFT JOIN sessions se ON se.id = i.session_id AND (i.blocking=1 OR i.status IN ('answered','delivered'))
		WHERE i.status IN ('open','snoozed','answered') OR (i.status='delivered' AND i.updated_at >= ?) OR i.stashed_at >= ?
		ORDER BY i.id`, now()-WaitWindow, StashCutoff())
	if err != nil {
		return ""
	}
	defer rows.Close()
	h := sha256.New()
	n := 0
	for rows.Next() {
		var id, status, title, urgency, project, component, place, due, state, delivery string
		var exp, upd, stashed int64
		rows.Scan(&id, &status, &title, &urgency, &project, &component, &place, &due, &exp, &upd, &stashed, &state, &delivery)
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\n", id, status, title, urgency, project, component, place, due, exp, upd, stashed, state, delivery)
		n++
	}
	return fmt.Sprintf("%d.%x", n, h.Sum(nil)[:8])
}

// Noise is one item the human marked "not worth my time".
type Noise struct {
	ItemID, Title, Project, Kind, Note string
	TS                                 int64
}

// RecentNoise lists the latest "not worth my time" marks, newest first;
// agentOnly keeps just items agents filed (not docket's own health checks).
func (s *Store) RecentNoise(limit int, agentOnly bool) ([]Noise, error) {
	q := `SELECT d.item_id, COALESCE(i.title,''), COALESCE(i.project,''), COALESCE(i.kind,''), d.answer, d.ts
		FROM decisions d LEFT JOIN items i ON i.id = d.item_id WHERE d.action='noise'`
	if agentOnly {
		q += ` AND i.source='cli'`
	}
	rows, err := s.DB.Query(q+` ORDER BY d.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Noise
	for rows.Next() {
		var n Noise
		if err := rows.Scan(&n.ItemID, &n.Title, &n.Project, &n.Kind, &n.Note, &n.TS); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AdoptLegacyKey finds an active item on machine filed with --key k before
// keys were scoped by project (or while it had no project), and moves it
// under key, so the next filing updates it instead of adding a duplicate.
// Only one filed by the same session or from the same repository counts:
// another repo's unsorted item with the same key is a different thing.
func (s *Store) AdoptLegacyKey(k, machine, session, root, project, key string) bool {
	// Rows from before keys were scoped (project '') come first; a filing
	// that is itself unsorted only takes one of those.
	candidates := []any{machine, Unsorted}
	if project == Unsorted {
		candidates = []any{machine, ""}
	}
	rows, err := s.DB.Query(`SELECT id, COALESCE(dedupe_key,''), session_id, cwd FROM items WHERE machine=? AND project IN ('', ?) AND dedupe_key LIKE 'key:%'
		AND status IN ('maybe','open','snoozed','answered') ORDER BY project<>'', created_at DESC`, candidates...)
	if err != nil {
		return false
	}
	root = strings.TrimRight(root, "/")
	var id string
	for rows.Next() {
		var cand, dk, sess, cwd string
		rows.Scan(&cand, &dk, &sess, &cwd)
		sameRepo := root != "" && (cwd == root || strings.HasPrefix(cwd, root+"/"))
		if AgentKey(dk) == k && ((session != "" && sess == session) || sameRepo) {
			id = cand
			break
		}
	}
	rows.Close()
	if id == "" {
		return false
	}
	_, err = s.DB.Exec(`UPDATE OR IGNORE items SET project=?, dedupe_key=?, updated_at=? WHERE id=?`, project, key, now(), id)
	return err == nil
}

// MovedByKey finds an active item filed with --key k that the human has
// since moved into project, so a filing or done there with that key (the
// agent sees it under its new project) still reaches it. Its key keeps the
// original scope, so filings in the original project find it directly.
// This session's item first, then this machine's.
func (s *Store) MovedByKey(k, project, session, machine string) (Item, bool) {
	if project == "" || project == Unsorted {
		return Item{}, false
	}
	for _, q := range []struct{ col, val string }{{"session_id", session}, {"machine", machine}} {
		if q.val == "" {
			continue
		}
		rows, err := s.DB.Query(`SELECT `+itemCols+` FROM items WHERE dedupe_key LIKE 'key:%' AND instr(pinned,'project')>0 AND project=? AND `+q.col+`=?
			AND status IN ('maybe','open','snoozed','answered') ORDER BY created_at DESC`, project, q.val)
		if err != nil {
			return Item{}, false
		}
		for rows.Next() {
			it, err := scanItem(rows)
			if err == nil && AgentKey(it.DedupeKey) == k {
				rows.Close()
				return it, true
			}
		}
		rows.Close()
	}
	return Item{}, false
}

// LastDecision is the most recent logged action on an item ("" if none).
func (s *Store) LastDecision(itemID string) string {
	var a string
	s.DB.QueryRow(`SELECT action FROM decisions WHERE item_id=? ORDER BY id DESC LIMIT 1`, itemID).Scan(&a)
	return a
}

// Reopen brings back an item closed without a logged decision (an agent's
// done, a condition that cleared), with its closing note and expiry gone.
func (s *Store) Reopen(id string) error {
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
	if _, err := tx.Exec(`UPDATE items SET status='open', resolution='', resolved_at=NULL, expires_at=0, stashed_at=0, updated_at=? WHERE id=?`, t, id); err != nil {
		if isDedupeConflict(err) {
			return errors.New("can't reopen: an open item with the same key has been filed since")
		}
		return err
	}
	if _, err := tx.Exec(`INSERT INTO decisions(item_id, ts, action, prev_status) VALUES (?,?,'reopen',?)`, id, t, prev); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimUnsorted gives an item without a project (an older docket's, or one
// filed before a project was known) the project a filing named for it,
// unless the human placed it by hand.
func (s *Store) ClaimUnsorted(id, project string) error {
	if project == "" || project == Unsorted {
		return nil
	}
	_, err := s.DB.Exec(`UPDATE items SET project=?, updated_at=? WHERE id=? AND project IN ('', ?) AND instr(pinned,'project')=0`, project, now(), id, Unsorted)
	return err
}
