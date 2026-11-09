package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/sessions"
	"github.com/blobspire/docket/internal/spool"
	"github.com/blobspire/docket/internal/usage"
)

// localRun answers the same commands a remote machine's `docket` would, in
// process, for the server's own machine.
func localRun(stdin io.Reader, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("no command")
	}
	sp := spool.Spool{Dir: config.SpoolDir()}
	var out bytes.Buffer
	switch args[0] {
	case "spool":
		if len(args) < 2 {
			break
		}
		switch args[1] {
		case "export":
			cur, max := "", int64(4<<20)
			for i := 2; i+1 < len(args); i += 2 {
				switch args[i] {
				case "--cursor":
					cur = args[i+1]
				case "--max":
					fmt.Sscan(args[i+1], &max)
				}
			}
			c, err := spool.ParseCursor(cur)
			if err != nil {
				return nil, err
			}
			_, err = sp.Export(&out, c, max)
			return out.Bytes(), err
		case "ack":
			if len(args) != 3 {
				break
			}
			c, err := spool.ParseCursor(args[2])
			if err != nil {
				return nil, err
			}
			return nil, sp.Ack(c, 72*time.Hour)
		case "blob":
			if len(args) != 3 {
				break
			}
			return os.ReadFile(filepath.Join(sp.Dir, "blobs", filepath.Base(args[2])))
		}
	case "sessions":
		live, err := sessions.List()
		if err != nil {
			return nil, err
		}
		if live == nil {
			live = []sessions.Live{}
		}
		return json.Marshal(live)
	case "rules":
		body, _ := io.ReadAll(io.LimitReader(stdin, 256<<10))
		return nil, writeRules(body)
	case "quota":
		q, ok := usage.Read()
		if !ok {
			return []byte("null"), nil
		}
		return json.Marshal(q)
	case "usage":
		days, err := usage.Scan(time.Now().Add(-8 * 24 * time.Hour))
		if err != nil {
			return nil, err
		}
		if days == nil {
			days = []usage.Day{}
		}
		return json.Marshal(days)
	case "mailbox":
		var d mailbox.Delivery
		if err := json.NewDecoder(stdin).Decode(&d); err != nil {
			return nil, err
		}
		return nil, mailbox.Box{Dir: config.MailboxDir()}.Put(d)
	case "session":
		text, _ := io.ReadAll(io.LimitReader(stdin, 256<<10))
		flags := map[string]string{}
		var extra []string
		for i := 2; i < len(args); i++ {
			if args[i] == "--" {
				extra = args[i+1:]
				break
			}
			if i+1 < len(args) {
				flags[args[i]] = args[i+1]
				i++
			}
		}
		if len(args) > 1 && args[1] == "start" {
			home, _ := os.UserHomeDir()
			return nil, sessions.Start(sessions.StartOpts{Name: flags["--name"], Cwd: flags["--cwd"], Lead: flags["--lead"], ResumeID: flags["--resume"], Args: extra, Prompt: string(text), Home: home})
		}
		if len(args) > 1 && args[1] == "send" {
			return nil, sessions.SendTmux(flags["--pane"], string(text))
		}
		if len(args) > 1 && args[1] == "resume" {
			home, _ := os.UserHomeDir()
			return nil, sessions.Resume(flags["--id"], flags["--cwd"], flags["--name"], string(text), home)
		}
	}
	return nil, fmt.Errorf("unsupported local command %v", args)
}

// writeRules stores the approved preferences docket's CLAUDE.md block imports.
func writeRules(body []byte) error {
	if err := os.MkdirAll(config.Home(), 0o700); err != nil {
		return err
	}
	p := filepath.Join(config.Home(), "rules.md")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// WriteRules is writeRules for the CLI.
func WriteRules(body []byte) error { return writeRules(body) }
