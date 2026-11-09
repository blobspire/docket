// Package server is docket's always-on side: it collects spool records from
// every machine, turns them into queue items, serves the web queue, sends
// phone pushes and delivers answers back to sessions.
package server

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is ~/.config/docket/docket.toml on the server. It is personal (host
// names, push topic), so it never lives in the repository.
type Config struct {
	DataDir     string    `toml:"data_dir"`
	Listen      string    `toml:"listen"`
	PublicURL   string    `toml:"public_url"`
	Person      string    `toml:"person"`
	Timezone    string    `toml:"timezone"`
	AllowLogins []string  `toml:"allow_logins"`
	BriefAt     string    `toml:"brief_at"`
	BriefPush   *bool     `toml:"brief_push"`
	Ntfy        Ntfy      `toml:"ntfy"`
	Machines    []Machine `toml:"machine"`
	Leads       []Lead    `toml:"lead"`
	Chief       Chief     `toml:"chief"`

	loc *time.Location
}

type Ntfy struct {
	Server     string `toml:"server"`
	Topic      string `toml:"topic"`
	Token      string `toml:"token"`
	EnvFile    string `toml:"env_file"`
	QuietStart string `toml:"quiet_start"`
	QuietEnd   string `toml:"quiet_end"`
}

// Machine is one computer docket collects from.
//
//	mode = "local": the server's own machine; read its spool directly
//	mode = "pull":  the server SSHes in (machines that can't reach the server)
//	mode = "push":  the machine runs `docket sync` against the server (laptops
//	                that sleep; no inbound SSH to them needed)
type Machine struct {
	Name          string   `toml:"name"`
	Mode          string   `toml:"mode"`
	SSH           string   `toml:"ssh"`
	Docket        string   `toml:"docket"`
	Interval      Duration `toml:"interval"`
	Sleepy        bool     `toml:"sleepy"`
	RemoteApprove *bool    `toml:"remote_approve"`
	Physical      []string `toml:"physical_keywords"`
}

// Lead is one long-running agent responsible for an area of work.
type Lead struct {
	Name       string   `toml:"name"`
	Machine    string   `toml:"machine"`
	Dir        string   `toml:"dir"`
	Session    string   `toml:"session"`     // tmux session name (default: lead-<name>)
	Priority   int      `toml:"priority"`    // higher ranks its items higher (0-5)
	ClaudeArgs []string `toml:"claude_args"` // extra flags when docket starts it
	Schedule   []string `toml:"schedule"`    // "hourly", "hourly@47", "daily@09:00", "every 30m"
	WakePrompt string   `toml:"wake_prompt"` // what a scheduled wake says
	WakeOn     []string `toml:"wake_on"`     // job_done, job_stale
	Enabled    *bool    `toml:"enabled"`     // false: listed, but docket never starts or wakes it
	MatchDir   bool     `toml:"match_dir"`   // also treat any session working in dir as this lead
	AlwaysOn   bool     `toml:"always_on"`   // alert when its session isn't running
}

func (l Lead) On() bool { return l.Enabled == nil || *l.Enabled }

func (l Lead) WakesOn(event string) bool {
	for _, e := range l.WakeOn {
		if e == event {
			return true
		}
	}
	return false
}

// Lead looks up a lead by name.
func (c *Config) Lead(name string) (Lead, bool) {
	for _, l := range c.Leads {
		if l.Name == name {
			return l, true
		}
	}
	return Lead{}, false
}

// Duration decodes TOML strings like "90s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

// ConfigPath is where the server reads its config.
func ConfigPath() string {
	if p := os.Getenv("DOCKET_CONFIG"); p != "" {
		return p
	}
	dir, _ := os.UserConfigDir()
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dir = xdg
	} else if home, err := os.UserHomeDir(); err == nil {
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "docket", "docket.toml")
}

// LoadConfig reads and validates the config, filling in defaults.
func LoadConfig(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	home, _ := os.UserHomeDir()
	if c.DataDir == "" {
		c.DataDir = filepath.Join(home, ".local", "share", "docket")
	}
	c.DataDir = expand(c.DataDir)
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7788"
	}
	if c.Person == "" {
		c.Person = "the user"
	}
	if c.BriefAt == "" {
		c.BriefAt = "09:00"
	}
	if c.Ntfy.Server == "" {
		c.Ntfy.Server = "https://ntfy.sh"
	}
	if c.Ntfy.QuietStart == "" && c.Ntfy.QuietEnd == "" {
		c.Ntfy.QuietStart, c.Ntfy.QuietEnd = "23:00", "07:00"
	}
	if c.Ntfy.EnvFile != "" {
		if err := c.Ntfy.loadEnv(expand(c.Ntfy.EnvFile)); err != nil {
			return nil, err
		}
	}
	c.loc = time.Local
	if c.Timezone != "" {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return nil, fmt.Errorf("config: timezone: %w", err)
		}
		c.loc = loc
	}
	seen := map[string]bool{}
	for i := range c.Machines {
		m := &c.Machines[i]
		if m.Name == "" || seen[m.Name] {
			return nil, fmt.Errorf("config: every [[machine]] needs a unique name")
		}
		seen[m.Name] = true
		switch m.Mode {
		case "local", "push":
		case "pull":
			if m.SSH == "" {
				m.SSH = m.Name
			}
		default:
			return nil, fmt.Errorf("config: machine %s: mode must be local, pull or push", m.Name)
		}
		if m.Docket == "" {
			m.Docket = "~/.local/bin/docket"
		}
		if m.Interval.Duration == 0 {
			m.Interval.Duration = time.Minute
		}
	}
	seenLead := map[string]bool{}
	for i := range c.Leads {
		l := &c.Leads[i]
		if l.Name == "" || seenLead[l.Name] {
			return nil, fmt.Errorf("config: every [[lead]] needs a unique name")
		}
		seenLead[l.Name] = true
		if _, ok := c.Machine(l.Machine); !ok {
			return nil, fmt.Errorf("config: lead %s: unknown machine %q", l.Name, l.Machine)
		}
		if l.Dir == "" {
			return nil, fmt.Errorf("config: lead %s needs dir", l.Name)
		}
		if l.Session == "" {
			l.Session = "lead-" + l.Name
		}
		for _, sch := range l.Schedule {
			if _, err := parseSchedule(sch); err != nil {
				return nil, fmt.Errorf("config: lead %s: %w", l.Name, err)
			}
		}
		if l.WakePrompt == "" {
			l.WakePrompt = "Scheduled check-in from docket: review your area, continue the next step of your plan, and file anything you need from " + c.Person + " with docket."
		}
	}
	return &c, nil
}

// loadEnv reads NTFY_SERVER / NTFY_TOPIC / NTFY_TOKEN from a KEY=VALUE file.
func (n *Ntfy) loadEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("config: ntfy env_file: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "NTFY_SERVER":
			n.Server = v
		case "NTFY_TOPIC":
			n.Topic = v
		case "NTFY_TOKEN":
			n.Token = v
		}
	}
	return sc.Err()
}

// Machine looks up a machine by name.
func (c *Config) Machine(name string) (Machine, bool) {
	for _, m := range c.Machines {
		if m.Name == name {
			return m, true
		}
	}
	return Machine{}, false
}

// Approvable reports whether answers from the queue may approve actions on
// this machine. Physical actions are never approvable remotely regardless.
func (m Machine) Approvable() bool { return m.RemoteApprove == nil || *m.RemoteApprove }

// Now is the current time in the configured zone.
func (c *Config) Now() time.Time { return time.Now().In(c.loc) }

// BriefPushOn reports whether the morning brief also goes to the phone.
func (c *Config) BriefPushOn() bool { return c.BriefPush == nil || *c.BriefPush }

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// inWindow reports whether hh:mm t falls in [start, end), wrapping midnight.
func inWindow(t time.Time, start, end string) bool {
	s, e := minutes(start), minutes(end)
	if s < 0 || e < 0 || s == e {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e
}

func minutes(hhmm string) int {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return -1
	}
	return h*60 + m
}
