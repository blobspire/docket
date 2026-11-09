// Package turnstate keeps tiny per-session state on the agent's machine:
// when the session last filed something, when its last turn ended, which
// sentences it was already nudged about, and which waiter process is current.
package turnstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/config"
)

var safe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Pending struct {
	Sentence string `json:"sentence"`
	Pattern  string `json:"pattern"`
	At       int64  `json:"at"` // unix nanoseconds
}

type State struct {
	LastStop int64    `json:"last_stop"` // unix nanoseconds
	Nudged   []string `json:"nudged,omitempty"`
	Pending  *Pending `json:"pending,omitempty"`
}

type Session struct {
	ID  string
	dir string
}

// For returns the state handle for a session (ok=false for unsafe ids).
func For(sid string) (Session, bool) {
	if !safe.MatchString(sid) {
		return Session{}, false
	}
	return Session{ID: sid, dir: filepath.Join(config.Home(), "state")}, true
}

func (s Session) path(ext string) string { return filepath.Join(s.dir, s.ID+ext) }

// MarkFiled records that the session just filed an item.
func (s Session) MarkFiled() {
	os.MkdirAll(s.dir, 0o700)
	now := time.Now()
	f := s.path(".filed")
	if err := os.WriteFile(f, []byte(strconv.FormatInt(now.UnixNano(), 10)), 0o600); err == nil {
		os.Chtimes(f, now, now)
	}
}

// LastFiled is when the session last filed (0 if never).
func (s Session) LastFiled() int64 {
	b, err := os.ReadFile(s.path(".filed"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func (s Session) Load() State {
	var st State
	if b, err := os.ReadFile(s.path(".json")); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (s Session) Save(st State) error {
	if len(st.Nudged) > 50 {
		st.Nudged = st.Nudged[len(st.Nudged)-50:]
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	os.MkdirAll(s.dir, 0o700)
	tmp := s.path(".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(".json"))
}

// SetWaiter records pid as the session's current answer waiter.
func (s Session) SetWaiter(pid int) {
	os.MkdirAll(s.dir, 0o700)
	os.WriteFile(s.path(".waiter"), []byte(strconv.Itoa(pid)), 0o600)
}

// Waiter is the current waiter's pid (0 if none).
func (s Session) Waiter() int {
	b, err := os.ReadFile(s.path(".waiter"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// GC removes state for sessions untouched for age.
func GC(age time.Duration) {
	dir := filepath.Join(config.Home(), "state")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > age {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
