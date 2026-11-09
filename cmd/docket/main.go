// Command docket: one queue for everything your agents need from you.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/cli"
	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/spool"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `docket: one queue for everything your agents need from you.

Filing (for agents; never blocks, keep working afterwards):
  docket ask "<question>" [--options "a|b"] [--recommend a] [--why "..."] [--blocking] [--due YYYY-MM-DD|today|tomorrow] [--physical]
      You are holding an action for Cole's approval, or need his decision.
  docket task "<what only Cole can do>" [--blocks "<work it blocks>"] [--due ...]
      Logins, credentials, hardware, money, anything physical.
  docket review "<what to review>" --evidence <file>[,<file>...] [--why "..."]
      Work ready for his review: screenshots, videos, reports, diffs.
  docket issue "<problem you noticed>" [--why "..."]
      A problem you are not fixing now.
  docket fyi "<something he should know>"
  docket done <dk-id> [--why "<how it was resolved>"]
  Common flags: --key <stable-id> (re-filing the same key updates one item instead of adding another).

Machine setup:
  docket init --agent [--machine <name>]   create ~/.docket and name this machine
  docket spool tail [-n N]                 show recent records
  docket spool export [--cursor C] [--max BYTES]   (used by the server over SSH)
  docket spool ack <cursor>                        (used by the server over SSH)
  docket hook <Event>                      (used by the Claude Code plugin)
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
		// error and panic and exit 0. (Exit 2 would block some events.)
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(stderr, "docket hook: internal error: %v\n", r)
			}
			code = 0
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
			return 2 // wakes the idle session with the answer on stderr
		}
		return 0
	}

	var err error
	switch {
	case cli.IsFileCommand(cmd):
		err = cli.File(cmd, rest, stdout, os.Getenv)
	case cmd == "done":
		err = cli.Done(rest, stdout, os.Getenv)
	case cmd == "spool":
		err = spoolCmd(rest, stdout)
	case cmd == "init":
		err = initCmd(rest, stdout)
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

func spoolCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: docket spool tail|export|ack|path")
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
	}
	return fmt.Errorf("unknown spool command %q", args[0])
}

func initCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	agent := fs.Bool("agent", false, "set up this machine to capture agent events")
	machine := fs.String("machine", "", "this machine's name in docket (default: short hostname)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*agent {
		return errors.New("usage: docket init --agent [--machine <name>]   (server setup comes in a later version)")
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
	fmt.Fprintf(stdout, "docket: %s is ready as machine %q.\nNext: install the Claude Code plugin:\n  claude plugin marketplace add blobspire/docket\n  claude plugin install docket@docket\n", config.Home(), config.Machine())
	return nil
}
