package sessions

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var nameOK = regexp.MustCompile(`^[A-Za-z0-9_-]{1,60}$`)
var idOK = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// Resume restarts a finished Claude Code session in a new detached tmux
// session (named like rc's, so `rc <computer>` lists it) with prompt as its
// first message, and with Remote Control on so it can be watched.
func Resume(id, cwd, name, prompt, home string) error {
	if !idOK.MatchString(id) {
		return fmt.Errorf("bad session id %q", id)
	}
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, name)
	if !nameOK.MatchString(name) {
		return fmt.Errorf("bad session name %q", name)
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return fmt.Errorf("folder %s doesn't exist", cwd)
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("empty prompt")
	}
	if exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil {
		return fmt.Errorf("a tmux session named %s is already running", name)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		claude = filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(claude); err != nil {
			return errors.New("claude isn't installed on this machine")
		}
	}
	dir := filepath.Join(home, ".docket", "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	promptFile := filepath.Join(dir, name+".prompt")
	if err := os.WriteFile(promptFile, []byte(prompt), 0o600); err != nil {
		return err
	}
	script := filepath.Join(dir, name+".sh")
	body := fmt.Sprintf("#!/bin/sh\n# Started by docket to deliver an answer to a finished session.\ncd %s || exit 1\n%s --resume %s --remote-control %s \"$(cat %s)\"\necho $? > %s\n",
		shq(cwd), shq(claude), shq(id), shq(name), shq(promptFile), shq(filepath.Join(dir, name+".exited")))
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		return err
	}
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", "200", "-y", "50", "sh", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
