// Package sessions reads Claude Code's live-session registry on this machine
// (~/.claude/sessions/<pid>.json) and can type into a session's tmux pane
// when no hook can deliver to it (sessions started before docket's plugin).
package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Live is one running Claude Code session.
type Live struct {
	PID      int    `json:"pid"`
	ID       string `json:"session_id"`
	Cwd      string `json:"cwd"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Waiting  string `json:"waiting_for,omitempty"`
	Name     string `json:"name"`
	Tmux     string `json:"tmux,omitempty"` // session:@window.%pane
	BridgeID string `json:"bridge_id,omitempty"`
	Version  string `json:"version,omitempty"`
	Updated  int64  `json:"updated_at,omitempty"`
}

type registryFile struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	WaitingFor      string `json:"waitingFor"`
	Name            string `json:"name"`
	Tmux            string `json:"tmux"`
	BridgeSessionID string `json:"bridgeSessionId"`
	Version         string `json:"version"`
	UpdatedAt       int64  `json:"updatedAt"`
}

func registryDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

// List returns the sessions whose process is still alive.
func List() ([]Live, error) {
	ents, err := os.ReadDir(registryDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Live
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(registryDir(), e.Name()))
		if err != nil {
			continue
		}
		var r registryFile
		if json.Unmarshal(b, &r) != nil || r.SessionID == "" || r.PID <= 0 || !alive(r.PID) {
			continue
		}
		out = append(out, Live{PID: r.PID, ID: r.SessionID, Cwd: r.Cwd, Kind: r.Kind, Status: r.Status, Waiting: r.WaitingFor,
			Name: r.Name, Tmux: r.Tmux, BridgeID: r.BridgeSessionID, Version: r.Version, Updated: r.UpdatedAt})
	}
	return out, nil
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// PaneOf extracts the pane id ("%3") from a registry tmux value.
func PaneOf(tmux string) string {
	if i := strings.LastIndexByte(tmux, '%'); i >= 0 {
		return tmux[i:]
	}
	return ""
}

var paneRe = regexp.MustCompile(`^%[0-9]+$`)

// ErrUnsafe means typing into the pane now could interfere with a person.
var ErrUnsafe = errors.New("unsafe to type into the pane")

// SendTmux pastes text into a Claude Code session's tmux pane and presses
// Enter, but only when it is safe: the pane runs Claude Code, nobody is
// attached to its tmux session, and the input line is empty.
func SendTmux(pane, text string) error {
	if !paneRe.MatchString(pane) {
		return fmt.Errorf("bad pane id %q", pane)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to send")
	}
	info, err := tmux("display-message", "-p", "-t", pane, "#{session_attached}\t#{pane_current_command}\t#{pane_dead}")
	if err != nil {
		return fmt.Errorf("pane %s: %w", pane, err)
	}
	f := strings.Split(strings.TrimSpace(info), "\t")
	if len(f) != 3 {
		return fmt.Errorf("pane %s: unexpected tmux output %q", pane, info)
	}
	if f[2] == "1" {
		return fmt.Errorf("%w: pane %s is dead", ErrUnsafe, pane)
	}
	if f[0] != "0" {
		return fmt.Errorf("%w: someone is attached to that tmux session", ErrUnsafe)
	}
	cmd := strings.ToLower(f[1])
	if !strings.Contains(cmd, "claude") && cmd != "node" {
		return fmt.Errorf("%w: pane %s runs %q, not Claude Code", ErrUnsafe, pane, f[1])
	}
	screen, err := tmux("capture-pane", "-p", "-t", pane)
	if err != nil {
		return err
	}
	if !inputEmpty(screen) {
		return fmt.Errorf("%w: the input line isn't empty", ErrUnsafe)
	}
	buf := "docket-" + fmt.Sprint(time.Now().UnixNano())
	load := exec.Command("tmux", "load-buffer", "-b", buf, "-")
	load.Stdin = strings.NewReader(text)
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %v %s", err, out)
	}
	if _, err := tmux("paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	_, err = tmux("send-keys", "-t", pane, "Enter")
	return err
}

// inputEmpty finds Claude Code's input prompt line ("❯") nearest the bottom
// of the screen and reports whether nothing is typed after it.
func inputEmpty(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-12; i-- {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "❯") {
			rest := strings.TrimSpace(strings.TrimPrefix(t, "❯"))
			return rest == ""
		}
	}
	return false // no prompt visible (a dialog, or still busy): don't type
}

func tmux(args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := exec.Command("tmux", args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %v %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
