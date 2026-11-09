// Command docket: one queue for everything your agents need from you.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/blobspire/docket/internal/backfill"
	"github.com/blobspire/docket/internal/claudemd"
	"github.com/blobspire/docket/internal/cli"
	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/server"
	"github.com/blobspire/docket/internal/sessions"
	"github.com/blobspire/docket/internal/spool"
	"github.com/blobspire/docket/internal/ulid"
	tokens "github.com/blobspire/docket/internal/usage"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `docket: one queue for everything your agents need from you.

Filing (for agents; never blocks, keep working afterwards):
  docket ask "<question>" [--options "a|b"] [--recommend a] [--why "..."] [--blocking] [--due YYYY-MM-DD|today|tomorrow] [--physical]
      You are holding an action for the human's approval, or need their decision.
  docket task "<what only they can do>" [--blocks "<work it blocks>"] [--due ...]
      Logins, credentials, hardware, money, anything physical.
  docket review "<what to review>" --evidence <file>[,<file>...] [--why "..."]
      Work ready for review: screenshots, videos, reports, diffs.
  docket issue "<problem you noticed>" [--why "..."]
      A problem you are not fixing now.
  docket fyi "<something they should know>"
  docket done <dk-id> [--why "<how it was resolved>"]
  --key <stable-id> on any of these updates one item instead of adding another.

Long-running jobs (so a stall gets noticed):
  docket job register <name> --every 10m [--why "..."]
  docket job beat <name> [--why "progress"]
  docket job done <name> [--failed] [--why "..."]

Setting up a machine:
  docket init --agent [--machine <name>] [--person <name>] [--no-claude-md]
  docket uninstall                          remove docket's block from ~/.claude/CLAUDE.md
  docket sync --server <ssh-host>           for laptops: push events to the server, fetch answers
  docket backfill grep [--since 30d]        file "maybe" items for asks already in transcripts
  docket import md <file> [--section "<heading>"] [--lead <name>]   import open tasks from markdown

Server:
  docket serve                              run the queue (config: ~/.config/docket/docket.toml)
  docket server receive --machine <name>    (used by docket sync over SSH)
  docket server put-blob <sha256>           (used by docket sync over SSH)
  docket server backfill-prompt [--dry-run] ask live sessions to file what they're waiting on
  docket server test-push                   send a test notification to your phone

Used by the server and the plugin:
  docket spool tail|export|ack|blob|path
  docket mailbox put
  docket sessions --json | quota | usage [--json] [--since 7d]
  docket statusline [-- <your statusline command>]   record quota as Claude Code renders its status line
  docket session send --pane <%id> | resume --id <id> --cwd <dir> --name <name>
  docket session start --name <n> --cwd <dir> [--lead <l>] [--resume <id>] [-- <claude flags>]
  docket hook <Event>
  docket version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]

	if cmd == "hook" {
		// Hooks must never break or block the agent's session: swallow every
		// error and panic. Only the background waiter may exit 2, to wake an
		// idle session with an answer.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "docket hook: internal error: %v\n", r)
				code = 0
			}
		}()
		event := ""
		if len(rest) > 0 {
			event = rest[0]
		}
		hc, err := hook.Run(event, stdin, stdout, stderr, os.Getenv)
		if err != nil {
			fmt.Fprintf(stderr, "docket hook: %v\n", err)
		}
		if event == "Wait" && hc == 2 {
			return 2
		}
		return 0
	}

	var err error
	switch {
	case cli.IsFileCommand(cmd):
		err = cli.File(cmd, rest, stdout, os.Getenv)
	case cmd == "done":
		err = cli.Done(rest, stdout, os.Getenv)
	case cmd == "job":
		err = cli.Job(rest, stdout, os.Getenv)
	case cmd == "spool":
		err = spoolCmd(rest, stdout)
	case cmd == "mailbox":
		err = mailboxCmd(rest, stdin)
	case cmd == "sessions":
		err = sessionsCmd(stdout)
	case cmd == "statusline":
		err = statuslineCmd(rest, stdin, stdout)
	case cmd == "quota":
		q, ok := tokens.Read()
		if !ok {
			fmt.Fprintln(stdout, "null")
		} else {
			err = json.NewEncoder(stdout).Encode(q)
		}
	case cmd == "usage":
		err = usageCmd(rest, stdout)
	case cmd == "session":
		err = sessionCmd(rest, stdin)
	case cmd == "init":
		err = initCmd(rest, stdout)
	case cmd == "uninstall":
		err = uninstallCmd(stdout)
	case cmd == "sync":
		err = syncCmd(rest, stdout)
	case cmd == "backfill":
		err = backfillCmd(rest, stdout)
	case cmd == "import":
		err = importCmd(rest, stdout)
	case cmd == "serve":
		err = serveCmd()
	case cmd == "server":
		err = serverCmd(rest, stdin, stdout)
	case cmd == "version", cmd == "--version", cmd == "-V":
		fmt.Fprintf(stdout, "docket %s\n", version)
	case cmd == "help", cmd == "--help", cmd == "-h":
		fmt.Fprint(stdout, usage)
	default:
		err = fmt.Errorf("unknown command %q (try `docket help`)", cmd)
	}
	if err != nil {
		fmt.Fprintf(stderr, "docket: %v\n", err)
		return 1
	}
	return 0
}

// ---- spool / mailbox / sessions (agent machines) ---------------------------

func spoolCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: docket spool tail|export|ack|blob|path")
	}
	sp := spool.Spool{Dir: config.SpoolDir()}
	fs := flag.NewFlagSet("spool "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "path":
		fmt.Fprintln(stdout, sp.Dir)
		return nil
	case "tail":
		n := fs.Int("n", 20, "number of records")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		lines, err := sp.Tail(*n)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(stdout)
		for _, l := range lines {
			w.Write(l)
			w.WriteByte('\n')
		}
		return w.Flush()
	case "export":
		cur := fs.String("cursor", "", "resume after this cursor")
		max := fs.Int64("max", 4<<20, "approximate byte budget")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		c, err := spool.ParseCursor(*cur)
		if err != nil {
			return err
		}
		_, err = sp.Export(stdout, c, *max)
		return err
	case "ack":
		retention := fs.Duration("retention", 72*time.Hour, "keep acknowledged segments this long")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: docket spool ack <cursor>")
		}
		c, err := spool.ParseCursor(fs.Arg(0))
		if err != nil {
			return err
		}
		return sp.Ack(c, *retention)
	case "blob":
		if len(args) != 2 || len(args[1]) != 64 || strings.Trim(args[1], "0123456789abcdef") != "" {
			return errors.New("usage: docket spool blob <sha256>")
		}
		f, err := os.Open(filepath.Join(sp.Dir, "blobs", args[1]))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(stdout, f)
		return err
	}
	return fmt.Errorf("unknown spool command %q", args[0])
}

func mailboxCmd(args []string, stdin io.Reader) error {
	if len(args) != 1 || args[0] != "put" {
		return errors.New("usage: docket mailbox put  (reads one delivery as JSON on stdin)")
	}
	var d mailbox.Delivery
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&d); err != nil {
		return fmt.Errorf("mailbox put: %w", err)
	}
	return mailbox.Box{Dir: config.MailboxDir()}.Put(d)
}

func sessionsCmd(stdout io.Writer) error {
	live, err := sessions.List()
	if err != nil {
		return err
	}
	if live == nil {
		live = []sessions.Live{}
	}
	return json.NewEncoder(stdout).Encode(live)
}

func sessionCmd(args []string, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: docket session send|resume ...")
	}
	text, err := io.ReadAll(io.LimitReader(stdin, 256<<10))
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("session "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "send":
		pane := fs.String("pane", "", "tmux pane id, e.g. %3")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return sessions.SendTmux(*pane, string(text))
	case "start":
		name := fs.String("name", "", "tmux session name")
		cwd := fs.String("cwd", "", "folder to start in")
		lead := fs.String("lead", "", "lead name (exported as DOCKET_LEAD)")
		resume := fs.String("resume", "", "resume this Claude Code session id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		home, _ := os.UserHomeDir()
		return sessions.Start(sessions.StartOpts{Name: *name, Cwd: *cwd, Lead: *lead, ResumeID: *resume, Args: fs.Args(), Prompt: string(text), Home: home})
	case "resume":
		id := fs.String("id", "", "Claude Code session id")
		cwd := fs.String("cwd", "", "folder the session ran in")
		name := fs.String("name", "", "name for the resumed session")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		home, _ := os.UserHomeDir()
		return sessions.Resume(*id, *cwd, *name, string(text), home)
	}
	return fmt.Errorf("unknown session command %q", args[0])
}

// statuslineCmd records Claude Code's rate limits as it renders the status
// line, then prints the output of the chained status line command, if any:
//
//	"statusLine": {"type": "command", "command": "docket statusline -- python3 ~/my_statusline.py"}
func statuslineCmd(args []string, stdin io.Reader, stdout io.Writer) error {
	in, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	tokens.Record(in)
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdin = bytes.NewReader(in)
	c.Stdout = stdout
	c.Stderr = io.Discard
	c.Run()
	return nil
}

func usageCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	since := fs.String("since", "7d", "how far back")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := parseDays(*since)
	if err != nil {
		return err
	}
	days, err := tokens.Scan(time.Now().Add(-d))
	if err != nil {
		return err
	}
	if *asJSON {
		if days == nil {
			days = []tokens.Day{}
		}
		return json.NewEncoder(stdout).Encode(days)
	}
	per := map[string]*tokens.Day{}
	for _, x := range days {
		k := filepath.Base(x.Cwd)
		if per[k] == nil {
			per[k] = &tokens.Day{Cwd: k}
		}
		per[k].Input += x.Input
		per[k].Output += x.Output
		per[k].CacheWrite += x.CacheWrite
		per[k].CacheRead += x.CacheRead
	}
	fmt.Fprintf(stdout, "%-36s %12s %12s %14s\n", "project", "output", "input+write", "cache read")
	for k, v := range per {
		fmt.Fprintf(stdout, "%-36s %12d %12d %14d\n", k, v.Output, v.Input+v.CacheWrite, v.CacheRead)
	}
	return nil
}

// ---- machine setup ---------------------------------------------------------

func initCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	agent := fs.Bool("agent", false, "set up this machine to capture agent events")
	machine := fs.String("machine", "", "this machine's name in docket (default: short hostname)")
	person := fs.String("person", "", "how agents should refer to you (default: the user)")
	noBlock := fs.Bool("no-claude-md", false, "don't add docket's block to ~/.claude/CLAUDE.md")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*agent {
		return errors.New("usage: docket init --agent [--machine <name>] [--person <name>]   (server: write ~/.config/docket/docket.toml, then `docket serve`)")
	}
	for _, d := range []string{config.SpoolDir(), config.MailboxDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if name := strings.TrimSpace(*machine); name != "" {
		if strings.ContainsAny(name, " /\t\n") {
			return fmt.Errorf("machine name %q must not contain spaces or slashes", name)
		}
		if err := config.SetMachine(name); err != nil {
			return err
		}
	}
	if p := strings.TrimSpace(*person); p != "" {
		if err := os.WriteFile(filepath.Join(config.Home(), "person"), []byte(p+"\n"), 0o600); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "docket: %s is ready as machine %q.\n", config.Home(), config.Machine())
	if !*noBlock {
		p := strings.TrimSpace(*person)
		if p == "" {
			if b, err := os.ReadFile(filepath.Join(config.Home(), "person")); err == nil {
				p = strings.TrimSpace(string(b))
			}
		}
		changed, err := claudemd.Install(p)
		if err != nil {
			return fmt.Errorf("CLAUDE.md: %w", err)
		}
		if changed {
			fmt.Fprintf(stdout, "docket: added the docket block to %s (a backup is next to it).\n", claudemd.Path())
		}
	}
	fmt.Fprint(stdout, "If you haven't yet, install the Claude Code plugin:\n  claude plugin marketplace add blobspire/docket\n  claude plugin install docket@docket\n")
	return nil
}

func uninstallCmd(stdout io.Writer) error {
	changed, err := claudemd.Uninstall()
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(stdout, "docket: removed the docket block from %s.\n", claudemd.Path())
	}
	fmt.Fprint(stdout, "To remove the plugin too: claude plugin uninstall docket@docket\n")
	return nil
}

// syncCmd is for machines the server can't reach (laptops): push the spool
// to the server over SSH and bring back answers for local sessions.
func syncCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	host := fs.String("server", "", "ssh host of the docket server")
	remoteBin := fs.String("docket", "~/.local/bin/docket", "docket path on the server")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host == "" {
		return errors.New("usage: docket sync --server <ssh-host>")
	}
	sp := spool.Spool{Dir: config.SpoolDir()}
	box := mailbox.Box{Dir: config.MailboxDir()}
	machine := config.Machine()
	live, _ := sessions.List()
	env := map[string]any{"_sessions": live}
	if q, ok := tokens.Read(); ok {
		env["_quota"] = q
	}
	stamp := filepath.Join(config.Home(), "usage-sent")
	if info, err := os.Stat(stamp); err != nil || time.Since(info.ModTime()) > 15*time.Minute {
		if days, err := tokens.Scan(time.Now().Add(-8 * 24 * time.Hour)); err == nil {
			env["_usage"] = days
			defer os.WriteFile(stamp, []byte(time.Now().Format(time.RFC3339)), 0o600)
		}
	}
	sessJSON, _ := json.Marshal(env)
	sent, delivered := 0, 0
	for page := 0; page < 50; page++ {
		var buf bytes.Buffer
		buf.Write(sessJSON)
		buf.WriteByte('\n')
		tr, err := sp.Export(&buf, sp.Acked(), 4<<20)
		if err != nil {
			return err
		}
		out, err := sshRun(*host, *remoteBin+" server receive --machine "+shellQuote(machine), &buf)
		if err != nil {
			return err
		}
		var reply server.SyncReply
		if err := json.Unmarshal(out, &reply); err != nil {
			return fmt.Errorf("server reply: %w", err)
		}
		c, err := spool.ParseCursor(reply.Cursor)
		if err == nil && reply.Cursor != "" {
			sp.Ack(c, 72*time.Hour)
		}
		for _, d := range reply.Deliveries {
			if err := box.Put(d); err == nil {
				delivered++
			}
		}
		for _, sha := range reply.WantBlobs {
			if f, err := os.Open(filepath.Join(sp.Dir, "blobs", sha)); err == nil {
				sshRun(*host, *remoteBin+" server put-blob "+sha, f)
				f.Close()
			}
		}
		sent++
		if !tr.More {
			break
		}
		sessJSON = []byte(`{"_sessions":null}`)
	}
	box.GC(7 * 24 * time.Hour)
	if !*quiet {
		fmt.Fprintf(stdout, "docket: synced %s with %s (%d answer(s) received).\n", machine, *host, delivered)
	}
	return nil
}

func sshRun(host, command string, stdin io.Reader) ([]byte, error) {
	c := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, command)
	c.Stdin = stdin
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("ssh %s: %v: %s", host, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func backfillCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "grep" {
		return errors.New("usage: docket backfill grep [--since 30d] [--last 30]")
	}
	fs := flag.NewFlagSet("backfill grep", flag.ContinueOnError)
	since := fs.String("since", "30d", "look at transcripts modified within this long")
	last := fs.Int("last", 30, "assistant messages to read from the end of each session")
	dry := fs.Bool("dry-run", false, "print candidates without filing them")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	d, err := parseDays(*since)
	if err != nil {
		return err
	}
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, ".claude")
	}
	cands, err := backfill.Scan(filepath.Join(root, "projects"), time.Now().Add(-d), *last)
	if err != nil {
		return err
	}
	sp := spool.Spool{Dir: config.SpoolDir()}
	self := os.Getenv("CLAUDE_CODE_SESSION_ID") // don't backfill the session running this
	n := 0
	for _, c := range cands {
		if c.SessionID != "" && c.SessionID == self {
			continue
		}
		n++
		if *dry {
			fmt.Fprintf(stdout, "%s  %-40s  %s\n", c.At, filepath.Base(c.Cwd), c.Sentence)
			continue
		}
		r := record.New("backfill.candidate", os.Getenv)
		r.SessionID, r.Cwd = c.SessionID, c.Cwd
		r.Payload, _ = json.Marshal(map[string]string{"sentence": c.Sentence, "pattern": c.Pattern, "at": c.At})
		if err := sp.Append(r); err != nil {
			return err
		}
	}
	if !*dry {
		fmt.Fprintf(stdout, "docket: filed %d possible ask(s) from transcripts as \"maybe\" items.\n", n)
	}
	return nil
}

func parseDays(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

func importCmd(args []string, stdout io.Writer) error {
	if len(args) < 2 || args[0] != "md" {
		return errors.New(`usage: docket import md <file> [--section "<heading>"] [--lead <name>] [--kind human_task]`)
	}
	file := args[1]
	fs := flag.NewFlagSet("import md", flag.ContinueOnError)
	section := fs.String("section", "", "also import plain bullets under headings containing this text")
	lead := fs.String("lead", "", "lead these items belong to")
	kind := fs.String("kind", "human_task", "item kind")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	tasks, err := backfill.ImportMarkdown(file, *section)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(file)
	sp := spool.Spool{Dir: config.SpoolDir()}
	for _, t := range tasks {
		r := record.New("cli.file", func(k string) string {
			if k == "DOCKET_LEAD" && *lead != "" {
				return *lead
			}
			return os.Getenv(k)
		})
		r.Cwd = filepath.Dir(abs)
		r.Item = &record.Item{ID: ulid.ItemID(), Kind: *kind, Title: t.Text, Body: fmt.Sprintf("Imported from %s line %d.", filepath.Base(file), t.Line),
			Key: "import:" + filepath.Base(file) + ":" + strings.ToLower(t.Text)}
		if err := sp.Append(r); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "docket: imported %d open task(s) from %s.\n", len(tasks), file)
	return nil
}

// ---- server ---------------------------------------------------------------

func loadServer() (*server.Server, error) {
	cfg, err := server.LoadConfig(server.ConfigPath())
	if err != nil {
		return nil, err
	}
	return server.Open(cfg)
}

func serveCmd() error {
	s, err := loadServer()
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Run(ctx)
}

func serverCmd(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: docket server receive|put-blob")
	}
	s, err := loadServer()
	if err != nil {
		return err
	}
	defer s.Close()
	switch args[0] {
	case "receive":
		fs := flag.NewFlagSet("server receive", flag.ContinueOnError)
		machine := fs.String("machine", "", "the pushing machine's name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		all, err := io.ReadAll(io.LimitReader(stdin, 64<<20))
		if err != nil {
			return err
		}
		var sessJSON, quotaJSON, usageJSON []byte
		if i := bytes.IndexByte(all, '\n'); i > 0 && bytes.HasPrefix(all, []byte(`{"_`)) {
			var env struct {
				Sessions json.RawMessage `json:"_sessions"`
				Quota    json.RawMessage `json:"_quota"`
				Usage    json.RawMessage `json:"_usage"`
			}
			if json.Unmarshal(all[:i], &env) == nil {
				if string(env.Sessions) != "null" {
					sessJSON = env.Sessions
				}
				quotaJSON, usageJSON = env.Quota, env.Usage
			}
			all = all[i+1:]
		}
		s.ApplyQuota(*machine, quotaJSON)
		s.ApplyUsage(*machine, usageJSON)
		reply, err := s.Receive(*machine, bytes.NewReader(all), sessJSON)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(reply)
	case "backfill-prompt":
		fs := flag.NewFlagSet("server backfill-prompt", flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "list the sessions without prompting them")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		for _, line := range s.BackfillPrompt(context.Background(), *dry) {
			fmt.Fprintln(stdout, line)
		}
		return nil
	case "test-push":
		if err := s.TestPush(context.Background()); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "docket: test push sent.")
		return nil
	case "put-blob":
		if len(args) != 2 {
			return errors.New("usage: docket server put-blob <sha256>")
		}
		return s.PutBlob(args[1], stdin)
	}
	return fmt.Errorf("unknown server command %q", args[0])
}
