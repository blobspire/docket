package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	rows, err := tx.Query(`SELECT id, COALESCE(dedupe_key,''), status FROM items WHERE project=?`, from)
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
	oldPrefix, newPrefix := "key:"+from+":", "key:"+to+":"
	for _, r := range moved {
		key := r.key
		if strings.HasPrefix(key, oldPrefix) {
			key = newPrefix + strings.TrimPrefix(key, oldPrefix)
		}
		if key != r.key && activeStatus(r.status) {
			var other string
			if tx.QueryRow(`SELECT id FROM items WHERE dedupe_key=? AND status IN ('maybe','open','snoozed','answered')`, key).Scan(&other) == nil {
				if _, err := tx.Exec(`UPDATE items SET status='done', resolution=?, resolved_at=?, updated_at=? WHERE id=?`,
					"merged into "+other, t, t, r.id); err != nil {
					return err
				}
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

// LearnDir remembers that work in dir (on machine) belongs to project, unless
// that folder already has a mapping: the first explicit choice sticks.
func (s *Store) LearnDir(machine, dir, project string) error {
	dir = strings.TrimRight(dir, "/")
	if machine == "" || dir == "" || project == "" || project == Unsorted {
		return nil
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO project_dirs(machine, dir, project, updated_at) VALUES (?,?,?,?)`, machine, dir, project, now())
	return err
}

// SetDir sets a folder's project, replacing any earlier mapping.
func (s *Store) SetDir(machine, dir, project string) error {
	dir = strings.TrimRight(dir, "/")
	_, err := s.DB.Exec(`INSERT INTO project_dirs(machine, dir, project, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(machine, dir) DO UPDATE SET project=excluded.project, updated_at=excluded.updated_at`, machine, dir, project, now())
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

// AddAlias records that alias (an id an agent was told) means itemID.
func (s *Store) AddAlias(alias, itemID string) error {
	if alias == "" || alias == itemID {
		return nil
	}
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO item_aliases(alias, item_id) VALUES (?,?)`, alias, itemID)
	return err
}

// ResolveItem finds an item by id or by an alias of it.
func (s *Store) ResolveItem(id string) (Item, bool, error) {
	it, ok, err := s.GetItem(id)
	if ok || err != nil {
		return it, ok, err
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

// ActiveByAgentKey finds an active item filed with --key k (in any project)
// by the given session, or failing that from the given machine.
func (s *Store) ActiveByAgentKey(k, session, machine string) (Item, bool) {
	like := "key:%:" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(k)
	if session != "" {
		if it, err := scanItem(s.DB.QueryRow(`SELECT `+itemCols+` FROM items WHERE dedupe_key LIKE ? ESCAPE '\' AND session_id=? AND status IN ('maybe','open','snoozed','answered') ORDER BY created_at DESC LIMIT 1`, like, session)); err == nil {
			return it, true
		}
	}
	if machine != "" {
		if it, err := scanItem(s.DB.QueryRow(`SELECT `+itemCols+` FROM items WHERE dedupe_key LIKE ? ESCAPE '\' AND machine=? AND status IN ('maybe','open','snoozed','answered') ORDER BY created_at DESC LIMIT 1`, like, machine)); err == nil {
			return it, true
		}
	}
	return Item{}, false
}

// RecentlyNoised reports whether the newest item with this key was marked
// "not worth my time" since the given time, and when.
func (s *Store) RecentlyNoised(key string, since int64) (int64, bool) {
	var id string
	var resolved int64
	err := s.DB.QueryRow(`SELECT id, COALESCE(resolved_at,0) FROM items WHERE dedupe_key=? ORDER BY created_at DESC LIMIT 1`, key).Scan(&id, &resolved)
	if err != nil || resolved < since {
		return 0, false
	}
	var action string
	s.DB.QueryRow(`SELECT action FROM decisions WHERE item_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&action)
	return resolved, action == "noise"
}

// ResolveKeyPrefix closes active items whose key starts with prefix.
func (s *Store) ResolveKeyPrefix(prefix, why string) (int, error) {
	like := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(prefix) + "%"
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
	var status string
	if err := tx.QueryRow(`SELECT project, urgency, pinned, status FROM items WHERE id=?`, id).Scan(&prev.Project, &prev.Urgency, &prev.Pinned, &status); err != nil {
		return err
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
	_, err := tx.Exec(`UPDATE items SET project=?, urgency=?, pinned=?, updated_at=? WHERE id=?`, prev.Project, prev.Urgency, prev.Pinned, now(), id)
	return err
}

// Expire closes open or snoozed items whose expiry has passed.
func (s *Store) Expire(at int64) (int, error) {
	res, err := s.DB.Exec(`UPDATE items SET status='expired', resolution='expired: no longer relevant after its expiry time', resolved_at=?, updated_at=?
		WHERE expires_at > 0 AND expires_at <= ? AND status IN ('open','snoozed')`, at, at, at)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// QueueDigest changes whenever the active queue does, and only then.
func (s *Store) QueueDigest() string {
	var n, max int64
	s.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at),0) FROM items WHERE status IN ('open','snoozed','answered')`).Scan(&n, &max)
	var closed int64
	s.DB.QueryRow(`SELECT COALESCE(MAX(updated_at),0) FROM items`).Scan(&closed)
	return fmt.Sprintf("%d.%d.%d", n, max, closed)
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
