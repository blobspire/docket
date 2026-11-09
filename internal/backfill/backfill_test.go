package backfill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanFindsRecentAsksOnly(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-home-me-site")
	os.MkdirAll(filepath.Join(proj, "subagents"), 0o700)
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"deploy it"}}`,
		`{"type":"assistant","sessionId":"s1","cwd":"/home/me/site","timestamp":"2026-11-01T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Staged. It needs your approval before I push to production."}]}}`,
		`{"type":"assistant","sessionId":"s1","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"Could you approve this? (subagent)"}]}}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"assistant","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"All tests pass."}]}}`,
	}
	os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	old := filepath.Join(proj, "old.jsonl")
	os.WriteFile(old, []byte(lines[1]+"\n"), 0o600)
	past := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(old, past, past)

	got, err := Scan(root, time.Now().Add(-30*24*time.Hour), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "s1" || !strings.Contains(got[0].Sentence, "needs your approval") || got[0].Cwd != "/home/me/site" {
		t.Fatalf("got %+v", got)
	}
}

func TestImportMarkdown(t *testing.T) {
	md := `# Status
## Human tasks (checklist)
- [ ] Approve SIM_VERSION 6
- [x] Install timers
- [ ] Insert the COLE drive
## Waiting on the human
- Review the R&D audit
- ~~old item~~
## Running now
- lane AB
`
	p := filepath.Join(t.TempDir(), "STATUS.md")
	os.WriteFile(p, []byte(md), 0o600)
	texts := func(section string) string {
		got, err := ImportMarkdown(p, section)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range got {
			out = append(out, g.Text)
		}
		return strings.Join(out, "|")
	}
	if got := texts("waiting on the human"); got != "Review the R&D audit" {
		t.Fatalf("section only: %q", got)
	}
	if got := texts("human tasks"); got != "Approve SIM_VERSION 6|Insert the COLE drive" {
		t.Fatalf("checklist section: %q", got)
	}
	if got := texts(""); got != "Approve SIM_VERSION 6|Insert the COLE drive" {
		t.Fatalf("all unchecked: %q", got)
	}
}
