package sessions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListReadsRegistryAndSkipsDeadProcesses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	reg := filepath.Join(dir, "sessions")
	os.MkdirAll(reg, 0o700)
	os.WriteFile(filepath.Join(reg, "1.json"), []byte(`{"pid":`+itoa(os.Getpid())+`,"sessionId":"abc","cwd":"/w","status":"idle","tmux":"a:@0.%3","bridgeSessionId":"session_01X"}`), 0o600)
	os.WriteFile(filepath.Join(reg, "2.json"), []byte(`{"pid":999999,"sessionId":"dead","status":"idle"}`), 0o600)
	os.WriteFile(filepath.Join(reg, "3.json"), []byte(`garbage`), 0o600)
	got, err := List()
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v %v", got, err)
	}
	if got[0].ID != "abc" || got[0].BridgeID != "session_01X" || PaneOf(got[0].Tmux) != "%3" {
		t.Fatalf("got %+v", got[0])
	}
}

func itoa(i int) string {
	b := []byte{}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestInputEmpty(t *testing.T) {
	empty := "● done\n────────\n❯ \n────────\n  ⏵⏵ auto mode on\n"
	typed := "● done\n────────\n❯ half-typed message\n────────\n"
	dialog := "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"
	if !inputEmpty(empty) {
		t.Error("empty prompt not detected")
	}
	if inputEmpty(typed) {
		t.Error("typed text treated as empty")
	}
	if inputEmpty(dialog) {
		t.Error("a permission dialog treated as an empty prompt")
	}
	if inputEmpty("still working…\n") {
		t.Error("no prompt treated as empty")
	}
	// Captured with -e from a real pane: a dim prompt suggestion after ❯ is
	// not typed text; the same text drawn normally is.
	ghost := "────\n\x1b[39m❯\u00a0\x1b[2mi reviewed the audit, approve the proposals\x1b[0m\n────\n  ⏵⏵ auto mode on\n"
	if !inputEmpty(ghost) {
		t.Error("a dim prompt suggestion treated as typed text")
	}
	typedStyled := "────\n\x1b[39m❯\u00a0i reviewed the audit\x1b[0m\n────\n"
	if inputEmpty(typedStyled) {
		t.Error("typed text with escapes treated as empty")
	}
	mixed := "\x1b[39m❯ \x1b[2mghost\x1b[22m but typed\n"
	if inputEmpty(mixed) {
		t.Error("typed text after a dim run treated as empty")
	}
	if !inputEmpty("\x1b]0;title\x07\x1b[39m❯ \x1b[0m\n") {
		t.Error("an OSC title sequence broke prompt detection")
	}
}

func TestSendTmuxRejectsBadPane(t *testing.T) {
	if err := SendTmux("%3; rm -rf ~", "hi"); err == nil {
		t.Fatal("accepted an unsafe pane id")
	}
}

func TestClaudeInForeground(t *testing.T) {
	// Real trees: rc's launcher script (tmux reports "bash") and a zsh start
	// script, each running claude as a child in the foreground group.
	rc := `  81716       1   81716       -1 tmux: server
  81717   81716   81717   81717 audit-vlm-fork.
  81791   81717   81717   81717 claude
  81888   81791   81717   81717 python
3337245   81716 3337245 3337245 bash
`
	if !claudeInForeground(rc, 81717) {
		t.Error("claude under rc's launcher script not found")
	}
	if claudeInForeground(rc, 3337245) {
		t.Error("a plain shell pane treated as Claude Code")
	}
	if claudeInForeground(rc, 99999) {
		t.Error("an unknown pane pid treated as Claude Code")
	}
	zsh := `   1957    1937    1957    1957 zsh
   1965    1957    1957    1957 /home/cole/.local/bin/claude
`
	if !claudeInForeground(zsh, 1957) {
		t.Error("claude under a zsh start script not found")
	}
	direct := "  500 400 500 500 claude\n"
	if !claudeInForeground(direct, 500) {
		t.Error("claude as the pane's own process not found")
	}
	// claude suspended with ctrl-z: the shell is in the foreground again.
	suspended := `  600 400 600 600 bash
  601 600 601 600 claude
`
	if claudeInForeground(suspended, 600) {
		t.Error("a suspended claude treated as foreground")
	}
	mac := "  700 400 700 700 /Users/sam/Library/Application Support/x/claude\n"
	if !claudeInForeground(mac, 700) {
		t.Error("macOS comm path with spaces not handled")
	}
}
