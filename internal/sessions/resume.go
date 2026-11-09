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
var argOK = regexp.MustCompile(`^[A-Za-z0-9_./:=@,+-]{1,200}$`)

// StartOpts describes a Claude Code session docket starts in tmux.
type StartOpts struct {
	Name     string   // tmux session name (rc-compatible, so `rc <computer>` lists it)
	Cwd      string   // folder to start in
	ResumeID string   // optional: resume this conversation
	Lead     string   // optional: exported as DOCKET_LEAD so its items are attributed
	Args     []string // extra claude flags
	Prompt   string   // first message
	Home     string
}

// Start launches a detached tmux session running Claude Code with Remote
// Control on, so the human can watch or take over from claude.ai.
func Start(o StartOpts) error {
	o.Name = cleanName(o.Name)
	if !nameOK.MatchString(o.Name) {
		return fmt.Errorf("bad session name %q", o.Name)
	}
	if o.ResumeID != "" && !idOK.MatchString(o.ResumeID) {
		return fmt.Errorf("bad session id %q", o.ResumeID)
	}
	if o.Lead != "" && !nameOK.MatchString(o.Lead) {
		return fmt.Errorf("bad lead name %q", o.Lead)
	}
	for _, a := range o.Args {
		if !argOK.MatchString(a) {
			return fmt.Errorf("unsupported claude argument %q", a)
		}
	}
	if strings.HasPrefix(o.Cwd, "~/") {
		o.Cwd = filepath.Join(o.Home, o.Cwd[2:])
	}
	if info, err := os.Stat(o.Cwd); err != nil || !info.IsDir() {
		return fmt.Errorf("folder %s doesn't exist", o.Cwd)
	}
	if strings.TrimSpace(o.Prompt) == "" {
		return errors.New("empty prompt")
	}
	if exec.Command("tmux", "has-session", "-t", "="+o.Name).Run() == nil {
		return fmt.Errorf("a tmux session named %s is already running", o.Name)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		claude = filepath.Join(o.Home, ".local", "bin", "claude")
		if _, err := os.Stat(claude); err != nil {
			return errors.New("claude isn't installed on this machine")
		}
	}
	dir := filepath.Join(o.Home, ".docket", "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	promptFile := filepath.Join(dir, o.Name+".prompt")
	if err := os.WriteFile(promptFile, []byte(o.Prompt), 0o600); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Started by docket.\n")
	fmt.Fprintf(&b, "cd %s || exit 1\n", shq(o.Cwd))
	if o.Lead != "" {
		fmt.Fprintf(&b, "export DOCKET_LEAD=%s\n", shq(o.Lead))
	}
	fmt.Fprintf(&b, "%s", shq(claude))
	if o.ResumeID != "" {
		fmt.Fprintf(&b, " --resume %s", shq(o.ResumeID))
	}
	fmt.Fprintf(&b, " --remote-control %s", shq(o.Name))
	for _, a := range o.Args {
		fmt.Fprintf(&b, " %s", shq(a))
	}
	fmt.Fprintf(&b, " \"$(cat %s)\"\necho $? > %s\n", shq(promptFile), shq(filepath.Join(dir, o.Name+".exited")))
	script := filepath.Join(dir, o.Name+".sh")
	if err := os.WriteFile(script, []byte(b.String()), 0o700); err != nil {
		return err
	}
	out, err := exec.Command("tmux", "new-session", "-d", "-s", o.Name, "-x", "200", "-y", "50", "sh", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Resume restarts a finished session with prompt as its first message.
func Resume(id, cwd, name, prompt, home string) error {
	if !idOK.MatchString(id) {
		return fmt.Errorf("bad session id %q", id)
	}
	return Start(StartOpts{Name: name, Cwd: cwd, ResumeID: id, Prompt: prompt, Home: home, Args: []string{"--permission-mode", "auto"}})
}

func cleanName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, name)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
