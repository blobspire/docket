// Package usage reports, on each machine, how close the Claude account is to
// its limits and how many tokens each session used. It reads files Claude
// Code already writes: no API calls and no model involved.
package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/config"
)

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// ---- quota ----------------------------------------------------------------

type Window struct {
	UsedPct  float64 `json:"used_percentage"`
	ResetsAt int64   `json:"resets_at"`
}

type Quota struct {
	Account    string            `json:"account"`
	Windows    map[string]Window `json:"windows"` // five_hour, seven_day
	CapturedAt int64             `json:"captured_at"`
}

type limitsFile struct {
	RateLimits map[string]Window `json:"rate_limits"`
	CapturedAt float64           `json:"captured_at"`
}

// Record stores the rate limits from a statusline payload.
func Record(statusline []byte) error {
	var in struct {
		RateLimits map[string]Window `json:"rate_limits"`
	}
	if json.Unmarshal(statusline, &in) != nil || len(in.RateLimits) == 0 {
		return nil
	}
	b, _ := json.Marshal(limitsFile{RateLimits: in.RateLimits, CapturedAt: float64(time.Now().Unix())})
	os.MkdirAll(config.Home(), 0o700)
	tmp := filepath.Join(config.Home(), "quota.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(config.Home(), "quota.json"))
}

// Read returns the newest quota snapshot on this machine: docket's own, or
// one a statusline script already writes (~/.claude/nightly_rate_limits.json).
func Read() (Quota, bool) {
	var best limitsFile
	for _, p := range []string{filepath.Join(config.Home(), "quota.json"), filepath.Join(claudeDir(), "nightly_rate_limits.json")} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var f limitsFile
		if json.Unmarshal(b, &f) == nil && len(f.RateLimits) > 0 && f.CapturedAt > best.CapturedAt {
			best = f
		}
	}
	if len(best.RateLimits) == 0 {
		return Quota{}, false
	}
	return Quota{Account: account(), Windows: best.RateLimits, CapturedAt: int64(best.CapturedAt)}, true
}

// account names the Claude account this machine uses (organization, else email).
func account() string {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	var c struct {
		OAuth struct {
			Email string `json:"emailAddress"`
			Org   string `json:"organizationName"`
		} `json:"oauthAccount"`
	}
	json.Unmarshal(b, &c)
	if c.OAuth.Org != "" {
		return c.OAuth.Org
	}
	return c.OAuth.Email
}

// ---- token usage ----------------------------------------------------------

// Day is one session's token totals for one UTC day.
type Day struct {
	SessionID  string `json:"session_id"`
	Day        string `json:"day"`
	Cwd        string `json:"cwd,omitempty"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheWrite int64  `json:"cache_write"`
	CacheRead  int64  `json:"cache_read"`
}

type fileState struct {
	Offset int64           `json:"offset"`
	Size   int64           `json:"size"`
	Days   map[string]*Day `json:"days"`
}

type cache struct {
	Files map[string]*fileState `json:"files"`
}

// Scan returns per-session, per-day token totals for transcripts modified
// since `since`. Subagent transcripts count toward their parent session. It
// remembers how far it read each file, so repeated calls only read what is new.
func Scan(since time.Time) ([]Day, error) {
	root := filepath.Join(claudeDir(), "projects")
	cachePath := filepath.Join(config.Home(), "usage-cache.json")
	var c cache
	if b, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Files == nil {
		c.Files = map[string]*fileState{}
	}
	seen := map[string]bool{}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		seen[path] = true
		st := c.Files[path]
		if st == nil || info.Size() < st.Offset {
			st = &fileState{Days: map[string]*Day{}} // new or rewritten
			c.Files[path] = st
		}
		if info.Size() > st.Offset {
			readFrom(path, st, parentSession(root, path))
		}
		st.Size = info.Size()
		return nil
	})
	for p := range c.Files {
		if !seen[p] {
			delete(c.Files, p)
		}
	}
	if b, err := json.Marshal(c); err == nil {
		os.MkdirAll(config.Home(), 0o700)
		tmp := cachePath + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, cachePath)
		}
	}
	cutoff := since.UTC().Format("2006-01-02")
	merged := map[string]*Day{}
	for _, st := range c.Files {
		for _, d := range st.Days {
			if d.Day < cutoff {
				continue
			}
			k := d.SessionID + "|" + d.Day
			m := merged[k]
			if m == nil {
				m = &Day{SessionID: d.SessionID, Day: d.Day, Cwd: d.Cwd}
				merged[k] = m
			}
			if m.Cwd == "" {
				m.Cwd = d.Cwd
			}
			m.Input += d.Input
			m.Output += d.Output
			m.CacheWrite += d.CacheWrite
			m.CacheRead += d.CacheRead
		}
	}
	out := make([]Day, 0, len(merged))
	for _, d := range merged {
		out = append(out, *d)
	}
	return out, nil
}

// parentSession: .../<project>/<session>/subagents/<agent>.jsonl belongs to <session>.
func parentSession(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 4 && parts[2] == "subagents" {
		return parts[1]
	}
	return ""
}

func readFrom(path string, st *fileState, parent string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 1<<20)
	off := st.Offset
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is re-read next time
		}
		off += int64(len(line))
		if !strings.Contains(string(line[:min(len(line), 400)]), `"assistant"`) {
			continue
		}
		var l struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Usage struct {
					Input      int64 `json:"input_tokens"`
					Output     int64 `json:"output_tokens"`
					CacheWrite int64 `json:"cache_creation_input_tokens"`
					CacheRead  int64 `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &l) != nil || l.Type != "assistant" {
			continue
		}
		u := l.Message.Usage
		if u.Input+u.Output+u.CacheWrite+u.CacheRead == 0 {
			continue
		}
		sid := l.SessionID
		if parent != "" {
			sid = parent
		}
		day := "unknown"
		if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			day = t.UTC().Format("2006-01-02")
		}
		k := sid + "|" + day
		d := st.Days[k]
		if d == nil {
			d = &Day{SessionID: sid, Day: day, Cwd: l.Cwd}
			st.Days[k] = d
		}
		d.Input += u.Input
		d.Output += u.Output
		d.CacheWrite += u.CacheWrite
		d.CacheRead += u.CacheRead
	}
	st.Offset = off
}
