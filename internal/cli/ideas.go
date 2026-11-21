package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/agentctx"
	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/spool"
)

// Ideas the human dictated: `docket ideas` lists the saved ones (the
// backlog), and `docket idea done|save|take` reports on one.

var ideaFlags = set("why", "details")

// Idea is `docket idea done|save|take <idea-id> [--why "..."]`.
func Idea(args []string, stdout io.Writer, getenv func(string) string) error {
	const usage = `usage: docket idea done|save|take <idea-id> [--why "<what shipped / what you found>"]`
	if len(args) == 0 {
		return errors.New(usage)
	}
	op := args[0]
	pos, flags, err := parse(args[1:], ideaFlags)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !ideaIDOK.MatchString(pos[0]) {
		return errors.New(usage)
	}
	switch op {
	case "done", "save", "take":
	default:
		return errors.New(usage)
	}
	r := record.New("cli.idea", getenv)
	r.SessionID = sessionID(getenv)
	if op == "take" && r.SessionID == "" {
		return errors.New("docket idea take is for an agent session (it marks the idea as yours); run it from inside Claude Code")
	}
	r.Cwd, _ = os.Getwd()
	r.Payload, _ = json.Marshal(map[string]string{"op": op, "id": pos[0], "why": firstNonEmpty(flags["why"], flags["details"])})
	if err := (spool.Spool{Dir: config.SpoolDir()}).Append(r); err != nil {
		return fmt.Errorf("could not record it: %w", err)
	}
	switch op {
	case "done":
		fmt.Fprintf(stdout, "docket: %s marked shipped; its proposal closes. Thanks.\n", pos[0])
	case "save":
		fmt.Fprintf(stdout, "docket: %s saved for later, with your notes; its proposal closes.\n", pos[0])
	default:
		fmt.Fprintf(stdout, "docket: %s is yours: it shows as being worked on. Run docket propose --idea %s to bring %s a plan, or docket idea done %s once it has shipped.\n", pos[0], pos[0], Person(), pos[0])
	}
	return nil
}

// Ideas is `docket ideas [--project <p>] [--all]`: the saved ideas, for
// this folder's project unless --all.
func Ideas(args []string, stdout io.Writer, getenv func(string) string) error {
	_, flags, err := parse(args, listFlags)
	if err != nil {
		return err
	}
	ctx, hasCtx := agentctx.Load()
	if !hasCtx {
		return errors.New("this machine has no docket context yet (it arrives with the next sync)")
	}
	project := flags["project"]
	if project == "" && flags["all"] != "true" {
		cwd, _ := os.Getwd()
		project, _, _ = resolveProject("", "", ctx, hasCtx, RepoRoot(cwd), realPath(cwd), getenv("DOCKET_LEAD"), false)
	}
	project = Slug(project)
	n := 0
	for _, i := range ctx.Ideas {
		if project != "" && i.Project != project {
			continue
		}
		n++
		fmt.Fprintf(stdout, "%s  %s  %s  (%s)\n", i.ID, i.Project, i.Title, time.Unix(i.CreatedAt, 0).Format("Jan 2"))
		if b := strings.TrimSpace(i.Body); b != "" {
			for _, line := range strings.Split(b, "\n") {
				fmt.Fprintf(stdout, "    %s\n", line)
			}
		}
		if note := strings.TrimSpace(i.Note); note != "" {
			fmt.Fprintf(stdout, "    Found earlier: %s\n", strings.Join(strings.Fields(note), " "))
		}
	}
	switch {
	case n == 0 && project != "":
		fmt.Fprintf(stdout, "No saved ideas for %s (docket ideas --all lists every project's).\n", project)
	case n == 0:
		fmt.Fprintln(stdout, "No saved ideas.")
	default:
		fmt.Fprintf(stdout, "\nTo work on one: docket idea take <idea-id>, then docket propose --idea <idea-id> with your plan, or docket idea done <idea-id> once it has shipped.\n")
	}
	return nil
}
