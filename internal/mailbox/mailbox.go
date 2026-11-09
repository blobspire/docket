// Package mailbox holds answers on the agent's machine until a hook hands
// them to the right session.
//
//	mailbox/inbox/<session>/<delivery>.json    waiting to be delivered
//	mailbox/claimed/<session>/<delivery>.json  handed to the session
//
// Claiming is an atomic rename, so when a background waiter and a
// PostToolUse hook race for the same answer, exactly one delivers it.
package mailbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Delivery is one answer for one session.
type Delivery struct {
	ID        string `json:"id"`
	ItemID    string `json:"item_id"`
	SessionID string `json:"session_id"`
	Nonce     string `json:"nonce"`
	Text      string `json:"text"`
}

var safe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Box struct{ Dir string }

func (b Box) inbox(sid string) string   { return filepath.Join(b.Dir, "inbox", sid) }
func (b Box) claimed(sid string) string { return filepath.Join(b.Dir, "claimed", sid) }

// Put stores a delivery for its session.
func (b Box) Put(d Delivery) error {
	if !safe.MatchString(d.SessionID) || !safe.MatchString(d.ID) {
		return fmt.Errorf("mailbox: unsafe session or delivery id")
	}
	if strings.TrimSpace(d.Text) == "" {
		return errors.New("mailbox: empty delivery")
	}
	if _, err := os.Stat(filepath.Join(b.claimed(d.SessionID), d.ID+".json")); err == nil {
		return nil // already delivered: a resend must not deliver twice
	}
	dir := b.inbox(d.SessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+d.ID+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, d.ID+".json"))
}

// Pending reports whether a session has anything waiting.
func (b Box) Pending(sid string) bool {
	if !safe.MatchString(sid) {
		return false
	}
	ents, _ := os.ReadDir(b.inbox(sid))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}

// Claim takes every waiting delivery for a session, oldest first.
func (b Box) Claim(sid string) ([]Delivery, error) {
	if !safe.MatchString(sid) {
		return nil, nil
	}
	ents, err := os.ReadDir(b.inbox(sid))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Delivery
	for _, n := range names {
		if err := os.MkdirAll(b.claimed(sid), 0o700); err != nil {
			return out, err
		}
		dst := filepath.Join(b.claimed(sid), n)
		if err := os.Rename(filepath.Join(b.inbox(sid), n), dst); err != nil {
			continue // someone else claimed it first
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			continue
		}
		var d Delivery
		if json.Unmarshal(data, &d) == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// Text joins deliveries into one message for the session.
func Text(ds []Delivery) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, d.Text)
	}
	return strings.Join(parts, "\n\n")
}

// GC deletes claimed deliveries older than age.
func (b Box) GC(age time.Duration) {
	root := filepath.Join(b.Dir, "claimed")
	sessions, _ := os.ReadDir(root)
	for _, s := range sessions {
		dir := filepath.Join(root, s.Name())
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if info, err := f.Info(); err == nil && time.Since(info.ModTime()) > age {
				os.Remove(filepath.Join(dir, f.Name()))
			}
		}
		os.Remove(dir) // only succeeds when empty
	}
}
