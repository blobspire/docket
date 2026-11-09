// Package backfill finds asks that agents made in chat before docket was
// running, so the queue starts with what is already waiting. It greps local
// transcripts with the self-check patterns: no model reads anything.
package backfill

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/selfcheck"
)

// Candidate is one ask-like sentence from a transcript.
type Candidate struct {
	SessionID string
	Cwd       string
	At        string
	Sentence  string
	Pattern   string
}

// Scan looks at main-session transcripts (not subagents) modified since
// `since`, keeps the assistant's prose from the last `last` assistant
// messages of each, and returns the ask-like sentences, de-duplicated.
func Scan(projectsDir string, since time.Time, last int) ([]Candidate, error) {
	var files []string
	dirs, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		ents, _ := os.ReadDir(filepath.Join(projectsDir, d.Name()))
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			if info, err := e.Info(); err == nil && info.ModTime().After(since) {
				files = append(files, filepath.Join(projectsDir, d.Name(), e.Name()))
			}
		}
	}
	var out []Candidate
	for _, f := range files {
		out = append(out, scanFile(f, last)...)
	}
	return out, nil
}

type line struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Sidechain bool   `json:"isSidechain"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type msg struct {
	sid, cwd, at, text string
}

func scanFile(path string, last int) []Candidate {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var msgs []msg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		b := sc.Bytes()
		if !strings.Contains(string(b[:min(len(b), 200)]), `"assistant"`) {
			continue // cheap skip of user/tool lines
		}
		var l line
		if json.Unmarshal(b, &l) != nil || l.Type != "assistant" || l.Sidechain {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			continue
		}
		var parts []string
		for _, bl := range blocks {
			if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
				parts = append(parts, bl.Text)
			}
		}
		if len(parts) > 0 {
			msgs = append(msgs, msg{sid: l.SessionID, cwd: l.Cwd, at: l.Timestamp, text: strings.Join(parts, "\n\n")})
		}
	}
	if len(msgs) > last {
		msgs = msgs[len(msgs)-last:]
	}
	seen := map[string]bool{}
	var out []Candidate
	for _, m := range msgs {
		for _, s := range selfcheck.Sentences(m.text) {
			for _, p := range selfcheck.Patterns {
				if p.Re.MatchString(s) {
					k := strings.ToLower(s)
					if !seen[k] {
						seen[k] = true
						out = append(out, Candidate{SessionID: m.sid, Cwd: m.cwd, At: m.at, Sentence: s, Pattern: p.ID})
					}
					break
				}
			}
		}
	}
	return out
}
