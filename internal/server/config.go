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

	"github.com/blobspire/docket/internal/mail"
	"github.com/blobspire/docket/internal/store"
)

// Config is ~/.config/docket/docket.toml on the server. It is personal (host
// names, push topic), so it never lives in the repository.
type Config struct {
	Ignored     []string `toml:"-"` // keys in the file docket doesn't use (logged at start)
	DataDir     string   `toml:"data_dir"`
	Listen      string   `toml:"listen"`
	PublicURL   string   `toml:"public_url"`
	Person      string   `toml:"person"`
	Timezone    string   `toml:"timezone"`
	AllowLogins []string `toml:"allow_logins"`
	// SystemProject is where docket's own health items go (machines,
	// logins, quota). Default "system".
	SystemProject string      `toml:"system_project"`
	Ntfy          Ntfy        `toml:"ntfy"`
	Machines      []Machine   `toml:"machine"`
	Leads         []Lead      `toml:"lead"`
	Chief         Chief       `toml:"chief"`
	Transcribe    Transcribe  `toml:"transcribe"`
	Ideas         IdeasConfig `toml:"ideas"`
	Email         EmailConfig `toml:"email"`
	// BackupDays is how many nightly database backups to keep, gzipped in
	// <data_dir>/backups. Unset means 7; 0 turns backups off.
	BackupDays *int `toml:"backup_days"`

	loc *time.Location
}

type Ntfy struct {
	Server     string `toml:"server"`
	Topic      string `toml:"topic"`
	Token      string `toml:"token"`
	EnvFile    string `toml:"env_file"`
	QuietStart string `toml:"quiet_start"` // no longer used: act-now pushes go out at any hour
	QuietEnd   string `toml:"quiet_end"`
}

// Machine is one computer docket collects from.
//
//	mode = "local": the server's own machine; read its spool directly
//	mode = "pull":  the server SSHes in (machines that can't reach the server)
//	mode = "push":  the machine runs `docket sync` against the server (laptops
//	                that sleep; no inbound SSH to them needed)
type Machine struct {
	Name     string   `toml:"name"`
	Mode     string   `toml:"mode"`
	SSH      string   `toml:"ssh"`
	Docket   string   `toml:"docket"`
	Interval Duration `toml:"interval"`
	Sleepy   bool     `toml:"sleepy"`
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
	Project    string   `toml:"project"`     // the project its items belong to (default: its name)
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

// Transcribe is the [transcribe] section: the speech-to-text server that
// turns dictations into text (whisper.cpp's whisper-server, self-hosted).
type Transcribe struct {
	URL        string   `toml:"url"`        // e.g. http://127.0.0.1:8178/inference; empty: dictation is typed only
	Vocabulary []string `toml:"vocabulary"` // words to spell right; project, component and place names are added
	// SaidAs maps a name to how it sounds, when speech-to-text can't spell
	// it (cwant = "quant"): the glossary says so, and the router maps it back.
	SaidAs    map[string]string `toml:"said_as"`
	KeepAudio string            `toml:"keep_audio"` // how long recordings are kept, e.g. "90d" (default); transcripts stay
	Language  string            `toml:"language"`   // default "en"
}

// IdeasConfig is the [ideas] section: how dictated ideas are routed and
// researched.
type IdeasConfig struct {
	Model       string   `toml:"model"`        // for research sessions and the router (default claude-opus-5-5)
	RouterModel string   `toml:"router_model"` // default: model
	MaxRunning  int      `toml:"max_running"`  // sessions researching or building at once (default 4)
	CloneDir    string   `toml:"clone_dir"`    // where docket clones repositories on the server (default ~/code/git)
	ClaudeArgs  []string `toml:"claude_args"`  // flags for the sessions it starts (default --permission-mode auto)
	BudgetUSD   float64  `toml:"router_budget_usd"`
}

// EmailConfig is the [email] section: the mailboxes docket triages. Mail is
// read and sent only by this server, with the accounts' passwords in files
// only it can read; agents never get mail access. See docs/email.md.
type EmailConfig struct {
	Accounts    []mail.Account `toml:"account"`
	Schedule    []string       `toml:"schedule"`     // triage passes (default 08:00, 12:30, 17:30)
	TriageModel string         `toml:"triage_model"` // sorts mail (default claude-sonnet-5-5)
	DraftModel  string         `toml:"draft_model"`  // researches and drafts replies (default claude-opus-5-5)
	MaxRunning  int            `toml:"max_running"`  // drafts written at once; the rest wait (default 4)
	DigestDays  int            `toml:"digest_days"`  // how long a digest line stays (default 3)
	KeepDays    int            `toml:"keep_days"`    // how long message bodies are kept (default 30)
	Backfill    string         `toml:"backfill"`     // how far back the first read looks (default 3d)
	Voice       string         `toml:"voice"`        // the drafting guide (default: the draft-email skill)
	SendHold    Duration       `toml:"send_hold"`    // Undo window before a reply goes out (default 30s)
	MaxPerPass  int            `toml:"max_per_pass"` // messages read per folder per pass (default 300)
}

// On reports whether any mailbox is configured.
func (e EmailConfig) On() bool { return len(e.Accounts) > 0 }

// Account looks up a mailbox by name.
func (e EmailConfig) Account(name string) (mail.Account, bool) {
	for _, a := range e.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return mail.Account{}, false
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
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	ignored := map[string]bool{}
	for _, k := range md.Undecoded() {
		if key := k.String(); !ignored[key] {
			ignored[key] = true
			c.Ignored = append(c.Ignored, key)
		}
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
	c.SystemProject = store.Slug(c.SystemProject)
	if c.SystemProject == "" {
		c.SystemProject = "system"
	}
	if c.Ntfy.Server == "" {
		c.Ntfy.Server = "https://ntfy.sh"
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
	if c.Transcribe.Language == "" {
		c.Transcribe.Language = "en"
	}
	if c.Transcribe.KeepAudio == "" {
		c.Transcribe.KeepAudio = "90d"
	}
	if _, err := parseEvery(c.Transcribe.KeepAudio); err != nil {
		return nil, fmt.Errorf("config: transcribe.keep_audio: %w", err)
	}
	if c.Ideas.Model == "" {
		c.Ideas.Model = "claude-opus-5-5"
	}
	if c.Ideas.RouterModel == "" {
		c.Ideas.RouterModel = c.Ideas.Model
	}
	if c.Ideas.MaxRunning <= 0 {
		c.Ideas.MaxRunning = 4
	}
	if c.Ideas.CloneDir == "" {
		c.Ideas.CloneDir = "~/code/git"
	}
	if len(c.Ideas.ClaudeArgs) == 0 {
		c.Ideas.ClaudeArgs = []string{"--permission-mode", "auto"}
	}
	if c.Ideas.BudgetUSD <= 0 {
		c.Ideas.BudgetUSD = 1
	}
	if err := c.Email.check(); err != nil {
		return nil, err
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
		l.Project = store.Slug(l.Project)
		if l.Project == "" {
			l.Project = store.Slug(l.Name)
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

func (e *EmailConfig) check() error {
	seen := map[string]bool{}
	for i := range e.Accounts {
		if err := e.Accounts[i].Check(); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if seen[e.Accounts[i].Name] {
			return fmt.Errorf("config: two email accounts are named %s", e.Accounts[i].Name)
		}
		seen[e.Accounts[i].Name] = true
	}
	if len(e.Schedule) == 0 {
		e.Schedule = []string{"daily@08:00", "daily@12:30", "daily@17:30"}
	}
	for _, sch := range e.Schedule {
		if _, err := parseSchedule(sch); err != nil {
			return fmt.Errorf("config: email.schedule: %w", err)
		}
	}
	if e.TriageModel == "" {
		e.TriageModel = "claude-sonnet-5-5"
	}
	if e.DraftModel == "" {
		e.DraftModel = "claude-opus-5-5"
	}
	if e.MaxRunning <= 0 {
		e.MaxRunning = 4
	}
	if e.DigestDays <= 0 {
		e.DigestDays = 3
	}
	if e.KeepDays <= 0 {
		e.KeepDays = 30
	}
	if e.Backfill == "" {
		e.Backfill = "3d"
	}
	if _, err := parseEvery(e.Backfill); err != nil {
		return fmt.Errorf("config: email.backfill: %w", err)
	}
	if e.Voice == "" {
		e.Voice = "~/.claude/skills/draft-email/SKILL.md"
	}
	e.Voice = expand(e.Voice)
	if e.SendHold.Duration <= 0 {
		e.SendHold.Duration = 30 * time.Second
	}
	if e.MaxPerPass <= 0 {
		e.MaxPerPass = 300
	}
	return nil
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

// Now is the current time in the configured zone.
func (c *Config) Now() time.Time { return time.Now().In(c.loc) }

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
