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
	"strconv"
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
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to send")
	}
	if err := CheckTmux(pane); err != nil {
		return err
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
	_, err := tmux("send-keys", "-t", pane, "Enter")
	return err
}

// CheckTmux reports whether typing into the pane now is safe: the pane runs
// Claude Code in the foreground, nobody is attached to its tmux session, and
// the input line is empty. A nil error means a paste would go ahead.
func CheckTmux(pane string) error {
	if !paneRe.MatchString(pane) {
		return fmt.Errorf("bad pane id %q", pane)
	}
	info, err := tmux("display-message", "-p", "-t", pane, "#{session_attached}\t#{pane_pid}\t#{pane_dead}")
	if err != nil {
		return fmt.Errorf("pane %s: %w", pane, err)
	}
	f := strings.Split(strings.TrimRight(info, "\r\n"), "\t")
	if len(f) != 3 {
		return fmt.Errorf("pane %s: unexpected tmux output %q", pane, info)
	}
	if f[1] == "" {
		return fmt.Errorf("%w: pane %s doesn't exist (the session may have exited)", ErrUnsafe, pane)
	}
	if f[2] == "1" {
		return fmt.Errorf("%w: pane %s is dead", ErrUnsafe, pane)
	}
	if f[0] != "0" {
		return fmt.Errorf("%w: someone is attached to that tmux session", ErrUnsafe)
	}
	panePID, err := strconv.Atoi(f[1])
	if err != nil {
		return fmt.Errorf("pane %s: bad pane pid %q", pane, f[1])
	}
	ps, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,tpgid=,comm=").Output()
	if err != nil {
		return fmt.Errorf("ps: %w", err)
	}
	if !claudeInForeground(string(ps), panePID) {
		return fmt.Errorf("%w: pane %s isn't running Claude Code in the foreground", ErrUnsafe, pane)
	}
	screen, err := tmux("capture-pane", "-p", "-e", "-t", pane)
	if err != nil {
		return err
	}
	if !inputEmpty(screen) {
		return fmt.Errorf("%w: the input line isn't empty", ErrUnsafe)
	}
	return nil
}

// claudeInForeground reports whether Claude Code runs in the pane, as the
// pane's own process or a descendant of it, in the terminal's foreground
// process group. tmux's pane_current_command isn't enough: launcher scripts
// (rc's ~/.rc-sessions/<name>.sh, start scripts) run claude as a child in the
// script's process group, so tmux reports "bash" or "zsh". ps is the output
// of `ps -A -o pid=,ppid=,pgid=,tpgid=,comm=`.
func claudeInForeground(ps string, panePID int) bool {
	type proc struct {
		ppid, pgid, tpgid int
		comm              string
	}
	procs := map[int]proc{}
	children := map[int][]int{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		var n [4]int
		ok := true
		for i := range n {
			v, err := strconv.Atoi(f[i])
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if !ok {
			continue
		}
		// comm is the last field; on macOS it can be a path with spaces.
		comm := filepath.Base(strings.Join(f[4:], " "))
		procs[n[0]] = proc{ppid: n[1], pgid: n[2], tpgid: n[3], comm: strings.ToLower(comm)}
		children[n[1]] = append(children[n[1]], n[0])
	}
	if _, ok := procs[panePID]; !ok {
		return false
	}
	queue, seen := []int{panePID}, map[int]bool{panePID: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		p := procs[pid]
		if (strings.Contains(p.comm, "claude") || p.comm == "node") && p.tpgid > 0 && p.pgid == p.tpgid {
			return true
		}
		for _, c := range children[pid] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	return false
}

// inputEmpty finds Claude Code's input prompt line ("❯") nearest the bottom
// of the screen and reports whether nothing is typed after it. The screen is
// captured with escapes (capture-pane -e) so that dim text, Claude Code's
// greyed-out prompt suggestion, can be told apart from text a person typed.
func inputEmpty(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-12; i-- {
		t := strings.TrimSpace(visibleText(lines[i], true))
		if strings.HasPrefix(t, "❯") {
			typed := strings.TrimSpace(visibleText(lines[i], false))
			rest := strings.TrimSpace(strings.TrimPrefix(typed, "❯"))
			return rest == ""
		}
	}
	return false // no prompt visible (a dialog, or still busy): don't type
}

// visibleText strips terminal escapes from a captured line. With keepDim
// false it also drops text drawn dim (SGR 2), which is how Claude Code shows
// a suggestion in an empty input box.
func visibleText(line string, keepDim bool) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c != 0x1b {
			if keepDim || !dim {
				b.WriteByte(c)
			}
			continue
		}
		if i+1 >= len(line) {
			break
		}
		switch line[i+1] {
		case '[': // CSI: parameters, then a final byte in 0x40-0x7e
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				params := line[i+2 : j]
				if params == "" {
					dim = false
				}
				for _, p := range strings.Split(params, ";") {
					switch p {
					case "0", "22":
						dim = false
					case "2":
						dim = true
					}
				}
			}
			i = j
		case ']': // OSC: up to BEL or ST
			j := i + 2
			for j < len(line) && line[j] != 0x07 && !(line[j] == 0x1b && j+1 < len(line) && line[j+1] == '\\') {
				j++
			}
			if j < len(line) && line[j] == 0x1b {
				j++
			}
			i = j
		default:
			i++
		}
	}
	return b.String()
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
