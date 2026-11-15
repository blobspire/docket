package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/agentctx"
)

func writeContext(t *testing.T, c agentctx.Context) {
	t.Helper()
	b, _ := json.Marshal(c)
	if err := agentctx.Write(b); err != nil {
		t.Fatal(err)
	}
}

func TestTitlesAndUrgencyAreChecked(t *testing.T) {
	getenv := env(t)
	long := strings.Repeat("word ", 20)
	for _, args := range [][]string{
		{long},
		{"two\nlines"},
		{"Fine title", "--urgency", "asap"},
		{"Fine title", "--expires", "2001-01-01"},
	} {
		if err := File("ask", args, &bytes.Buffer{}, getenv); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	err := File("ask", []string{long}, &bytes.Buffer{}, getenv)
	if err == nil || !strings.Contains(err.Error(), "--why") {
		t.Errorf("the long-title error should say where details go: %v", err)
	}
}

func TestProjectResolution(t *testing.T) {
	getenv := env(t)
	// No project list yet: file anyway and say so.
	var out bytes.Buffer
	if err := File("ask", []string{"Ship it?"}, &out, getenv); err != nil || !strings.Contains(out.String(), "no project list") {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
	writeContext(t, agentctx.Context{AsOf: time.Now().Unix(), Projects: []agentctx.Project{{Name: "cwant", Description: "Trading"}, {Name: "vlm-teacher", Description: "Robots"}},
		Dirs: []agentctx.Dir{{Dir: "/w/robot", Project: "vlm-teacher"}}, Leads: map[string]string{"website": "windward"}})
	// The lead's project applies when the folder isn't mapped.
	if err := File("ask", []string{"Ship it?"}, &bytes.Buffer{}, getenv); err != nil || last(t).Item.Project != "windward" {
		t.Fatalf("lead project: err=%v item=%+v", err, last(t).Item)
	}
	noLead := func(k string) string {
		if k == "DOCKET_LEAD" {
			return ""
		}
		return getenv(k)
	}
	// Nothing to go on: an error that lists the projects.
	err := File("ask", []string{"Ship it?"}, &bytes.Buffer{}, noLead)
	if err == nil || !strings.Contains(err.Error(), "vlm-teacher") || !strings.Contains(err.Error(), "--new-project") {
		t.Fatalf("err = %v", err)
	}
	// An unknown name needs a description to become a project.
	if err := File("ask", []string{"Ship it?", "--project", "Finance"}, &bytes.Buffer{}, noLead); err == nil {
		t.Fatal("accepted an unknown project without --new-project")
	}
	if err := File("ask", []string{"Ship it?", "--project", "Finance", "--new-project", "Money"}, &bytes.Buffer{}, noLead); err != nil {
		t.Fatal(err)
	}
	if it := last(t).Item; it.Project != "finance" || it.NewProject != "Money" {
		t.Fatalf("item = %+v", it)
	}
	// It's known locally right away, before the next sync.
	if err := File("ask", []string{"Another?", "--project", "finance"}, &bytes.Buffer{}, noLead); err != nil {
		t.Fatalf("the just-created project wasn't remembered: %v", err)
	}
}

func TestRepoRootIsTheMainRepoForWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		return cmd.Run()
	}
	if err := git("init", "-q"); err != nil {
		t.Skip("git init failed")
	}
	// A worktree needs a commit; skip where git has no identity configured.
	if err := git("commit", "-q", "--allow-empty", "-m", "x"); err != nil {
		t.Skip("git can't commit here (no identity configured)")
	}
	if err := git("worktree", "add", "-q", filepath.Join(dir, "wt")); err != nil {
		t.Skip("git worktree failed")
	}
	want, _ := filepath.EvalSymlinks(repo)
	for _, d := range []string{filepath.Join(repo, "sub"), filepath.Join(dir, "wt")} {
		got, _ := filepath.EvalSymlinks(RepoRoot(d))
		if got != want {
			t.Errorf("RepoRoot(%s) = %q, want %q", d, got, want)
		}
	}
	if RepoRoot(dir) != "" {
		t.Errorf("outside a repo: %q", RepoRoot(dir))
	}
}

func TestDoneByKeyAndList(t *testing.T) {
	getenv := env(t)
	writeContext(t, agentctx.Context{AsOf: time.Now().Add(-time.Hour).Unix(), Projects: []agentctx.Project{{Name: "cwant"}},
		Open: []agentctx.Item{{ID: "dk-11111111", Title: "Old one", Project: "cwant", Key: "old", CreatedAt: time.Now().Add(-2 * time.Hour).Unix()}}})
	if err := File("issue", []string{"cw-live killed", "--project", "cwant", "--key", "cw-live", "--urgency", "now"}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	List([]string{"--all"}, &out, getenv)
	if !strings.Contains(out.String(), "cw-live killed") || !strings.Contains(out.String(), "Old one") {
		t.Fatalf("list = %q", out.String())
	}
	if err := Done([]string{"--key", "cw-live", "--why", "recovered"}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	if d := last(t).Done; d == nil || d.Key != "cw-live" || d.Note != "recovered" {
		t.Fatalf("done = %+v", d)
	}
	out.Reset()
	List([]string{"--all"}, &out, getenv)
	if strings.Contains(out.String(), "cw-live killed") {
		t.Fatalf("a closed item is still listed: %q", out.String())
	}
}
