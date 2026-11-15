// Package turnstate keeps tiny per-session state on the agent's machine:
// which background waiter process is the current one for a session.
package turnstate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/blobspire/docket/internal/config"
)

var safe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

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
