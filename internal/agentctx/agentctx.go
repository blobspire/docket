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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/config"
)

// Context is the file the server sends each machine.
type Context struct {
	AsOf     int64             `json:"as_of"`
	Person   string            `json:"person,omitempty"`
	Machine  string            `json:"machine,omitempty"`
	Projects []Project         `json:"projects"`
	Places   []string          `json:"places,omitempty"`
	Dirs     []Dir             `json:"dirs,omitempty"`  // this machine's folders only
	Leads    map[string]string `json:"leads,omitempty"` // lead name → project
	Open     []Item            `json:"open,omitempty"`  // this machine's open items only
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
	ID        string `json:"id"`
	Title     string `json:"title"`
	Kind      string `json:"kind,omitempty"`
	Project   string `json:"project,omitempty"`
	Urgency   string `json:"urgency,omitempty"`
	Key       string `json:"key,omitempty"` // as the agent gave it (without docket's scope prefix)
	SessionID string `json:"session_id,omitempty"`
	CreatedAt int64  `json:"created_at"`
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

// Overlay is what this machine did since its context copy was made.
type Overlay struct {
	Filed    []Item    `json:"filed,omitempty"`
	Closed   []Closed  `json:"closed,omitempty"`
	Projects []Project `json:"projects,omitempty"`
}

type Closed struct {
	ID  string `json:"id,omitempty"`
	Key string `json:"key,omitempty"`
	At  int64  `json:"at"`
}

func overlayPath() string { return filepath.Join(config.Home(), "state", "local.json") }

func LoadOverlay() Overlay {
	var o Overlay
	if b, err := os.ReadFile(overlayPath()); err == nil {
		json.Unmarshal(b, &o)
	}
	return o
}

// Remember applies f to the overlay and saves it, dropping entries older than
// two days (by then the server's copy has long caught up).
func Remember(f func(*Overlay)) error {
	o := LoadOverlay()
	f(&o)
	cut := time.Now().Add(-48 * time.Hour).Unix()
	var filed []Item
	for _, it := range o.Filed {
		if it.CreatedAt >= cut {
			filed = append(filed, it)
		}
	}
	var closed []Closed
	for _, c := range o.Closed {
		if c.At >= cut {
			closed = append(closed, c)
		}
	}
	o.Filed, o.Closed = filed, closed
	if len(o.Projects) > 50 {
		o.Projects = o.Projects[len(o.Projects)-50:]
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(overlayPath()), 0o700); err != nil {
		return err
	}
	tmp := overlayPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, overlayPath())
}

// Merged is the context with this machine's recent filings, closings and new
// projects applied on top.
func (c Context) Merged(o Overlay) Context {
	out := c
	out.Projects = append([]Project(nil), c.Projects...)
	for _, p := range o.Projects {
		if !out.HasProject(p.Name) {
			out.Projects = append(out.Projects, p)
		}
	}
	closedID, closedKey := map[string]bool{}, map[string]bool{}
	for _, x := range o.Closed {
		if x.ID != "" {
			closedID[x.ID] = true
		}
		if x.Key != "" {
			closedKey[x.Key] = true
		}
	}
	seen, seenKey := map[string]bool{}, map[string]bool{}
	var open []Item
	for _, it := range c.Open {
		if closedID[it.ID] || (it.Key != "" && closedKey[it.Key]) {
			continue
		}
		seen[it.ID] = true
		if it.Key != "" {
			seenKey[it.Key] = true
		}
		open = append(open, it)
	}
	for _, it := range o.Filed {
		// Filed well before this copy was made: the server has processed it
		// (possibly merging it into an existing item, or the human already
		// closed it), so the copy is authoritative. The margin covers records
		// the server had received but not yet processed.
		if seen[it.ID] || closedID[it.ID] || it.CreatedAt < c.AsOf-10*60 || (it.Key != "" && (seenKey[it.Key] || closedKey[it.Key])) {
			continue
		}
		open = append(open, it)
	}
	out.Open = open
	return out
}
