// Package agentctx is what an agent machine knows about the queue without
// asking the server: the projects that exist, which folders belong to which
// project, and the machine's own open items. The server writes it to
// ~/.docket/context.json on every machine (the same way it writes rules.md);
// the CLI reads it to check project names, pick a project for a folder and
// list open items. A small local overlay covers what this machine filed or
// closed since the last copy arrived.
package agentctx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/blobspire/docket/internal/config"
)

// Context is the file the server sends each machine.
type Context struct {
	AsOf     int64             `json:"as_of"`
	Person   string            `json:"person,omitempty"`
	Machine  string            `json:"machine,omitempty"`
	Timezone string            `json:"timezone,omitempty"` // the human's, for dates like "today"
	Projects []Project         `json:"projects"`
	Places   []string          `json:"places,omitempty"`
	Dirs     []Dir             `json:"dirs,omitempty"`  // this machine's folders only
	Leads    map[string]string `json:"leads,omitempty"` // lead name → project
	Open     []Item            `json:"open,omitempty"`  // this machine's open items only
	Seen     []string          `json:"seen,omitempty"`  // ids (items, aliases, done/project records) from this machine the server has processed lately
	Ideas    []Idea            `json:"ideas,omitempty"` // the human's saved ideas, every project (docket ideas)
}

// Idea is one idea the human dictated and saved for later.
type Idea struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	Note      string `json:"note,omitempty"` // what an agent found, if one looked
	CreatedAt int64  `json:"created_at"`
}

type Project struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Components  []string `json:"components,omitempty"`
	Places      []string `json:"places,omitempty"`
	Open        int      `json:"open"`
}

type Dir struct {
	Dir     string `json:"dir"`
	Project string `json:"project"`
}

type Item struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Kind      string   `json:"kind,omitempty"`
	Project   string   `json:"project,omitempty"`
	Urgency   string   `json:"urgency,omitempty"`
	Key       string   `json:"key,omitempty"` // as the agent gave it (without docket's scope prefix)
	SessionID string   `json:"session_id,omitempty"`
	CreatedAt int64    `json:"created_at"`
	Aliases   []string `json:"aliases,omitempty"` // other ids agents were told for it (re-filings)
	Seq       int64    `json:"seq,omitempty"`     // overlay only: its place among this machine's filings and closings
}

// Path is ~/.docket/context.json.
func Path() string { return filepath.Join(config.Home(), "context.json") }

// Load reads the context; ok is false when none has arrived yet.
func Load() (Context, bool) {
	var c Context
	b, err := os.ReadFile(Path())
	if err != nil || json.Unmarshal(b, &c) != nil {
		return Context{}, false
	}
	return c, true
}

// Write replaces the context atomically (the server calls this through
// `docket context put`, or directly for its own machine).
func Write(data []byte) error {
	var c Context
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path())
}

// Hash identifies a context's content.
func Hash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}

// CurrentHash is the hash of the context on disk ("" if none).
func CurrentHash() string {
	b, err := os.ReadFile(Path())
	if err != nil {
		return ""
	}
	return Hash(b)
}

// ProjectForDir finds the project of the deepest mapped folder containing dir.
func (c Context) ProjectForDir(dirs ...string) string {
	for _, d := range dirs {
		d = strings.TrimRight(d, "/")
		if d == "" {
			continue
		}
		best, bestLen := "", -1
		for _, m := range c.Dirs {
			if (d == m.Dir || strings.HasPrefix(d, m.Dir+"/")) && len(m.Dir) > bestLen {
				best, bestLen = m.Project, len(m.Dir)
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// Location is the human's time zone, for dates like "today" and "tomorrow"
// (the agent's machine may be on UTC).
func (c Context) Location() *time.Location {
	if c.Timezone != "" {
		if l, err := time.LoadLocation(c.Timezone); err == nil {
			return l
		}
	}
	return time.Local
}

// HasProject reports whether a project by that name exists.
func (c Context) HasProject(name string) bool {
	for _, p := range c.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// ---- local overlay ------------------------------------------------------------

// Overlay is what this machine did that the server may not have processed
// yet. Each entry carries the id of the filing it came from; once that id is
// in the context's Seen list, the server's copy is authoritative.
type Overlay struct {
	Filed    []Item     `json:"filed,omitempty"`
	Closed   []Closed   `json:"closed,omitempty"`
	Projects []NewThing `json:"projects,omitempty"`
	Dirs     []NewThing `json:"dirs,omitempty"`
	// Seq numbers filings and closings in the order this machine made them
	// (times are whole seconds, so two in one second would tie).
	Seq int64 `json:"seq,omitempty"`
}

// Closed is an item this machine closed with `docket done`: the item's id
// when the CLI could tell which one, else the key (and project) it gave.
type Closed struct {
	ID       string `json:"id,omitempty"`
	Key      string `json:"key,omitempty"`
	Project  string `json:"project,omitempty"`
	RecordID string `json:"record"` // the done record; once seen, the server's copy is the truth
	At       int64  `json:"at"`
	Seq      int64  `json:"seq,omitempty"` // its place among this machine's filings and closings (0: an older docket's)
}

// Retention is how long overlay entries count. It is shorter than the window
// the server reports as seen (at least 48 hours), so an entry never outlives
// the server's acknowledgement of it.
const Retention = 36 * time.Hour

// NewThing is a project, or a folder's default project, this machine
// introduced with a filing.
type NewThing struct {
	Name        string `json:"name"` // project name; for a folder, the folder
	Project     string `json:"project,omitempty"`
	Description string `json:"description,omitempty"`
	ID          string `json:"id"` // the filing that introduced it
	At          int64  `json:"at"`
}

func overlayPath() string { return filepath.Join(config.Home(), "state", "local.json") }

func LoadOverlay() Overlay {
	var o Overlay
	if b, err := os.ReadFile(overlayPath()); err == nil {
		json.Unmarshal(b, &o)
	}
	return o
}

// Remember applies f to the overlay and saves it, under a lock so parallel
// filings don't lose each other's entries. Entries older than two days are
// dropped: by then the server has long caught up.
func Remember(f func(*Overlay)) error {
	dir := filepath.Dir(overlayPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "local.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Filing must never hang: if another docket holds the lock for long
	// (stopped mid-write, say), give up on the local record.
	locked := false
	for deadline := time.Now().Add(3 * time.Second); !locked && time.Now().Before(deadline); {
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			locked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		return errors.New("another docket process holds the local record")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	o := LoadOverlay()
	nf, nc := len(o.Filed), len(o.Closed)
	f(&o)
	// Number what f added, in order: Merged replays filings and closings
	// in the order they were made.
	for i := nf; i < len(o.Filed); i++ {
		o.Seq++
		o.Filed[i].Seq = o.Seq
	}
	for i := nc; i < len(o.Closed); i++ {
		o.Seq++
		o.Closed[i].Seq = o.Seq
	}
	cut := time.Now().Add(-Retention).Unix()
	o.Filed = keep(o.Filed, func(it Item) bool { return it.CreatedAt >= cut })
	o.Closed = keep(o.Closed, func(c Closed) bool { return c.At >= cut })
	o.Projects = keep(o.Projects, func(p NewThing) bool { return p.At >= cut })
	o.Dirs = keep(o.Dirs, func(d NewThing) bool { return d.At >= cut })
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".local-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), overlayPath())
}

func keep[T any](xs []T, ok func(T) bool) []T {
	var out []T
	for _, x := range xs {
		if ok(x) {
			out = append(out, x)
		}
	}
	return out
}

// Merged is the context with this machine's not-yet-processed filings,
// closings, new projects and folder defaults applied on top. Entries the
// server has processed (their id is in Seen) or that are past Retention
// are ignored: the server's copy is the truth for those.
func (c Context) Merged(o Overlay) Context {
	seen := map[string]bool{}
	for _, id := range c.Seen {
		seen[id] = true
	}
	cut := time.Now().Add(-Retention).Unix()
	out := c
	out.Projects = append([]Project(nil), c.Projects...)
	for _, p := range o.Projects {
		if p.At >= cut && !seen[p.ID] && !out.HasProject(p.Name) {
			out.Projects = append(out.Projects, Project{Name: p.Name, Description: p.Description})
		}
	}
	// The server's mapping wins a tie on the same folder; an overlay entry
	// still wins for a deeper folder (ProjectForDir picks the deepest).
	out.Dirs = append([]Dir(nil), c.Dirs...)
	for _, d := range o.Dirs {
		if d.At >= cut && !seen[d.ID] && !c.HasDir(d.Name) {
			out.Dirs = append(out.Dirs, Dir{Dir: d.Name, Project: d.Project})
		}
	}

	// The open items are the server's, then this machine's filings and
	// closings it hasn't seen yet, replayed in the order they were made:
	//   - a filing with the key of an open item updates that item (the
	//     server merges it in), so it's one entry, not two. The entry goes
	//     by the newest filing's id: if the server merges it, that id is an
	//     alias of the open item; if this copy is stale and the item was
	//     closed meanwhile, it's the new item. Either way docket done and
	//     docket add with it reach the right one.
	//   - a closing removes the entry it names (by any of its ids), or the
	//     entries with its key that existed before it.
	type entry struct {
		Item
		ids   []string // every id it's known by here
		local bool     // filed on this machine, unseen by the server
		gone  bool
	}
	have := map[string]bool{}
	var entries []*entry
	for _, it := range c.Open {
		have[it.ID] = true
		for _, a := range it.Aliases {
			have[a] = true
		}
		entries = append(entries, &entry{Item: it, ids: append([]string{it.ID}, it.Aliases...)})
	}
	type event struct {
		seq, at int64
		filing  *Item
		closing *Closed
	}
	var events []event
	for i := range o.Filed {
		it := &o.Filed[i]
		if it.CreatedAt >= cut && !seen[it.ID] && !have[it.ID] {
			events = append(events, event{seq: it.Seq, at: it.CreatedAt, filing: it})
		}
	}
	for i := range o.Closed {
		x := &o.Closed[i]
		if x.At >= cut && (x.RecordID == "" || !seen[x.RecordID]) {
			events = append(events, event{seq: x.Seq, at: x.At, closing: x})
		}
	}
	// An older docket's entries (no seq) came first; among themselves, by
	// time, a filing before a closing in the same second.
	sort.SliceStable(events, func(a, b int) bool {
		ea, eb := events[a], events[b]
		if ea.seq != eb.seq {
			return ea.seq < eb.seq
		}
		if ea.at != eb.at {
			return ea.at < eb.at
		}
		return ea.filing != nil && eb.closing != nil
	})
	live := func(key, project string) *entry {
		for _, e := range entries {
			if !e.gone && key != "" && e.Key == key && e.Project == project {
				return e
			}
		}
		return nil
	}
	for _, ev := range events {
		if it := ev.filing; it != nil {
			if e := live(it.Key, it.Project); e != nil {
				e.Aliases = append(e.Aliases, e.ID)
				e.ID, e.Title, e.local = it.ID, it.Title, true
				e.ids = append(e.ids, it.ID)
				if it.Urgency != "" { // empty: not given, the old one stands
					e.Urgency = it.Urgency
				}
				continue
			}
			n := *it
			if n.Urgency == "" {
				n.Urgency = "normal"
			}
			entries = append(entries, &entry{Item: n, ids: []string{it.ID}, local: true})
			continue
		}
		x := ev.closing
		for _, e := range entries {
			if e.gone {
				continue
			}
			if x.ID != "" {
				e.gone = slices.Contains(e.ids, x.ID)
			} else if x.Key != "" && x.Key == e.Key && (x.Project == "" || x.Project == e.Project) {
				// One the server made after this closing (filed again from
				// elsewhere) isn't the one it closed.
				e.gone = e.local || e.CreatedAt < x.At
			}
		}
	}
	var open []Item
	for _, e := range entries {
		if !e.gone {
			e.Item.Seq = 0
			open = append(open, e.Item)
		}
	}
	out.Open = open
	return out
}

// HasDir reports whether this exact folder has a project.
func (c Context) HasDir(dir string) bool {
	dir = strings.TrimRight(dir, "/")
	for _, d := range c.Dirs {
		if d.Dir == dir {
			return true
		}
	}
	return false
}

// KeyTarget is the open item `docket done --key k` will close, as the server
// will pick it: in the given project first; unless the project was named
// explicitly, then this session's newest, then the machine's newest.
func (c Context) KeyTarget(k, project string, explicit bool, session string) (Item, bool) {
	var best Item
	found := false
	pick := func(ok func(Item) bool) bool {
		for _, it := range c.Open {
			if it.Key == k && ok(it) && (!found || it.CreatedAt > best.CreatedAt) {
				best, found = it, true
			}
		}
		return found
	}
	if project != "" && pick(func(it Item) bool { return it.Project == project }) {
		return best, true
	}
	if explicit {
		return Item{}, false
	}
	if session != "" && pick(func(it Item) bool { return it.SessionID == session }) {
		return best, true
	}
	return best, pick(func(Item) bool { return true })
}
