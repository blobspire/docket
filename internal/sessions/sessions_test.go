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
}

func TestSendTmuxRejectsBadPane(t *testing.T) {
	if err := SendTmux("%3; rm -rf ~", "hi"); err == nil {
		t.Fatal("accepted an unsafe pane id")
	}
}
