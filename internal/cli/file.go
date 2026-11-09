// Package cli implements the commands agents run to file items for the
// human: docket ask | review | task | issue | fyi | done.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/spool"
	"github.com/blobspire/docket/internal/ulid"
)

// Kinds an agent can file, by command name.
var kinds = map[string]string{
	"ask":    "ask",
	"review": "review",
	"task":   "human_task",
	"issue":  "issue",
	"fyi":    "fyi",
}

const maxEvidence = 200 << 20

var (
	boolFlags  = map[string]bool{"blocking": true, "physical": true}
	valueFlags = map[string]bool{"options": true, "recommend": true, "why": true, "details": true, "blocks": true, "due": true, "key": true, "evidence": true}
	dateOK     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// IsFileCommand reports whether cmd is one of the filing commands.
func IsFileCommand(cmd string) bool { _, ok := kinds[cmd]; return ok }

// File parses `docket <cmd> "<title>" [flags]` and writes a cli.file record.
func File(cmd string, args []string, stdout io.Writer, getenv func(string) string) error {
	kind, ok := kinds[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q", cmd)
	}
	pos, flags, err := parse(args)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(strings.Join(pos, " "))
	if title == "" {
		return fmt.Errorf("usage: docket %s \"<what you need from Cole>\" [flags]  (see `docket help`)", cmd)
	}
	it := &record.Item{
		ID:        ulid.ItemID(),
		Kind:      kind,
		Title:     title,
		Body:      firstNonEmpty(flags["why"], flags["details"]),
		Recommend: flags["recommend"],
		Blocking:  flags["blocking"] == "true",
		Blocks:    flags["blocks"],
		Physical:  flags["physical"] == "true",
		Key:       flags["key"],
	}
	if it.Blocks != "" {
		it.Blocking = true
	}
	if o := flags["options"]; o != "" {
		for _, opt := range strings.Split(o, "|") {
			if opt = strings.TrimSpace(opt); opt != "" {
				it.Options = append(it.Options, opt)
			}
		}
	}
	if d := flags["due"]; d != "" {
		if it.Due, err = parseDue(d, time.Now()); err != nil {
			return err
		}
	}
	sp := spool.Spool{Dir: config.SpoolDir()}
	if ev := flags["evidence"]; ev != "" {
		for _, p := range splitList(ev) {
			e, err := storeEvidence(sp.Dir, p)
			if err != nil {
				return err
			}
			it.Evidence = append(it.Evidence, e)
		}
	}
	r := record.New("cli.file", getenv)
	r.SessionID = sessionID(getenv)
	r.Cwd, _ = os.Getwd()
	r.Item = it
	if err := sp.Append(r); err != nil {
		return fmt.Errorf("could not record the item: %w", err)
	}
	fmt.Fprintf(stdout, "docket: filed %s (%s). Cole will see it in his queue; keep working. His answer arrives as a message tagged [docket %s].\n", it.ID, kind, it.ID)
	return nil
}

// Done records that an item the agent filed is resolved.
func Done(args []string, stdout io.Writer, getenv func(string) string) error {
	pos, flags, err := parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !strings.HasPrefix(pos[0], "dk-") {
		return errors.New(`usage: docket done <dk-id> [--why "<how it was resolved>"]`)
	}
	r := record.New("cli.done", getenv)
	r.SessionID = sessionID(getenv)
	r.Cwd, _ = os.Getwd()
	r.Done = &record.Done{ItemID: pos[0], Note: firstNonEmpty(flags["why"], flags["details"])}
	if err := (spool.Spool{Dir: config.SpoolDir()}).Append(r); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "docket: marked %s done.\n", pos[0])
	return nil
}

// parse accepts flags before or after the positional words, as
// --name value, --name=value, or bare --blocking / --physical.
func parse(args []string) ([]string, map[string]string, error) {
	var pos []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		switch {
		case boolFlags[name]:
			if hasVal && val != "true" && val != "false" {
				return nil, nil, fmt.Errorf("--%s takes no value", name)
			}
			if !hasVal {
				val = "true"
			}
		case valueFlags[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
		default:
			return nil, nil, fmt.Errorf("unknown flag --%s", name)
		}
		if name == "evidence" && flags[name] != "" {
			val = flags[name] + "," + val // --evidence may repeat
		}
		flags[name] = val
	}
	return pos, flags, nil
}

func parseDue(s string, now time.Time) (string, error) {
	switch strings.ToLower(s) {
	case "today":
		return now.Format("2006-01-02"), nil
	case "tomorrow":
		return now.AddDate(0, 0, 1).Format("2006-01-02"), nil
	}
	if dateOK.MatchString(s) {
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return s, nil
		}
	}
	return "", fmt.Errorf("--due takes YYYY-MM-DD, today or tomorrow (got %q)", s)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// storeEvidence copies a file into the spool's blob store by content hash.
func storeEvidence(spoolDir, path string) (record.Evidence, error) {
	f, err := os.Open(path)
	if err != nil {
		return record.Evidence{}, fmt.Errorf("evidence: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return record.Evidence{}, err
	}
	if info.IsDir() {
		return record.Evidence{}, fmt.Errorf("evidence %s is a directory; attach files", path)
	}
	if info.Size() > maxEvidence {
		return record.Evidence{}, fmt.Errorf("evidence %s is %d MB; the limit is %d MB", path, info.Size()>>20, maxEvidence>>20)
	}
	dir := filepath.Join(spoolDir, "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return record.Evidence{}, err
	}
	tmp, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return record.Evidence{}, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), f)
	tmp.Close()
	if err != nil {
		return record.Evidence{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), filepath.Join(dir, sum)); err != nil {
		return record.Evidence{}, err
	}
	return record.Evidence{SHA256: sum, Name: filepath.Base(path), Mime: mime.TypeByExtension(strings.ToLower(filepath.Ext(path))), Size: n}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// sessionID is the filing session: DOCKET_SESSION (set by docket's
// SessionStart hook) or Claude Code's own CLAUDE_CODE_SESSION_ID.
func sessionID(getenv func(string) string) string {
	return firstNonEmpty(getenv("DOCKET_SESSION"), getenv("CLAUDE_CODE_SESSION_ID"))
}
