// Package cli implements the commands agents run to reach the human:
// docket ask | review | task | issue | done | add | list | projects | job.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blobspire/docket/internal/agentctx"
	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/spool"
	"github.com/blobspire/docket/internal/ulid"
)

// Kinds an agent can file, by command name. fyi is kept for older callers
// and shows as a review.
var kinds = map[string]string{
	"ask":    "ask",
	"review": "review",
	"task":   "human_task",
	"issue":  "issue",
	"fyi":    "fyi",
}

const (
	maxEvidence = 200 << 20
	maxTitle    = 90
)

var (
	boolFlags  = map[string]bool{"blocking": true, "physical": true, "all": true}
	valueFlags = map[string]bool{"options": true, "recommend": true, "why": true, "details": true, "blocks": true, "due": true, "key": true, "evidence": true,
		"project": true, "new-project": true, "component": true, "place": true, "urgency": true, "expires": true}
	// The flags each command takes; anything else is an error, not ignored.
	fileFlags = set("options", "recommend", "why", "details", "blocks", "due", "key", "evidence", "project", "new-project", "component", "place", "urgency", "expires", "blocking", "physical")
	doneFlags = set("why", "details", "key", "project")
	addFlags  = set("why", "details", "evidence")
	listFlags = set("project", "all")
	dashWord  = regexp.MustCompile(`^-[A-Za-z]`)
	dateOK    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	slugBad   = regexp.MustCompile(`[^a-z0-9]+`)
	relative  = regexp.MustCompile(`^(\d+)([mhd])$`)
)

// IsFileCommand reports whether cmd is one of the filing commands.
func IsFileCommand(cmd string) bool { _, ok := kinds[cmd]; return ok }

// File parses `docket <cmd> "<title>" [flags]` and writes a cli.file record.
func File(cmd string, args []string, stdout io.Writer, getenv func(string) string) error {
	kind, ok := kinds[cmd]
	if !ok {
		return fmt.Errorf("unknown command %q", cmd)
	}
	pos, flags, err := parse(args, fileFlags)
	if err != nil {
		return err
	}
	ctx, hasCtx := agentctx.Load()
	ctx = ctx.Merged(agentctx.LoadOverlay())
	person := Person()
	title := strings.TrimSpace(strings.Join(pos, " "))
	if title == "" {
		return fmt.Errorf("usage: docket %s \"<what you need from %s>\" --project <name> [flags]  (see `docket help`)", cmd, person)
	}
	if strings.ContainsAny(title, "\r\n") {
		return errors.New("the title must be one line. Put details in --why (markdown is fine)")
	}
	if n := utf8.RuneCountInString(title); n > maxTitle {
		return fmt.Errorf("the title is %d characters; keep it to %d at most so %s can scan it. Say the decision or action in plain words, no paths, hashes or ids, and put the details in --why", n, maxTitle, person)
	}
	if k := flags["key"]; utf8.RuneCountInString(k) > 120 || len(k) > 200 || (k != "" && strings.TrimSpace(k) == "") {
		return errors.New("--key is a short stable id (at most 120 characters), like arc-auth or nightly-report")
	}
	if flags["new-project"] != "" && flags["project"] == "" {
		return errors.New("--new-project describes a project you're creating: give its name with --project <short-name>")
	}
	urgency := strings.ToLower(flags["urgency"])
	switch urgency {
	case "", "now", "soon", "normal":
	default:
		return fmt.Errorf("--urgency is now, soon or normal (got %q). now = %s must act immediately; it pushes their phone", flags["urgency"], person)
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
		Urgency:   urgency,
		Component: strings.TrimSpace(flags["component"]),
		Place:     Slug(flags["place"]),
	}
	if utf8.RuneCountInString(it.Component) > 40 {
		return errors.New("--component is a short name for a part of the project (at most 40 characters)")
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
	now := time.Now().In(ctx.Location())
	if d := flags["due"]; d != "" {
		if it.Due, err = parseDue(d, now); err != nil {
			return err
		}
	}
	if e := flags["expires"]; e != "" {
		if it.Expires, err = parseExpires(e, now); err != nil {
			return err
		}
	}
	cwd, _ := os.Getwd()
	it.Root = RepoRoot(cwd)
	project, warn, err := resolveProject(flags["project"], flags["new-project"], ctx, hasCtx, it.Root, realPath(cwd), getenv("DOCKET_LEAD"), true)
	if err != nil {
		return err
	}
	it.Project = project
	it.Explicit = flags["project"] != ""
	if flags["new-project"] != "" {
		it.NewProject = strings.TrimSpace(flags["new-project"])
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
	r.Cwd = cwd
	r.Item = it
	if err := sp.Append(r); err != nil {
		return fmt.Errorf("could not record the item: %w", err)
	}
	rememberErr := agentctx.Remember(func(o *agentctx.Overlay) {
		at := now.Unix()
		o.Filed = append(o.Filed, agentctx.Item{ID: it.ID, Title: it.Title, Kind: kind, Project: project, Urgency: firstNonEmpty(urgency, "normal"), Key: it.Key, SessionID: r.SessionID, CreatedAt: at})
		if it.NewProject != "" && !ctx.HasProject(project) {
			o.Projects = append(o.Projects, agentctx.NewThing{Name: project, Description: it.NewProject, ID: it.ID, At: at})
		}
		if it.Explicit && it.Root != "" && !ctx.HasDir(it.Root) {
			o.Dirs = append(o.Dirs, agentctx.NewThing{Name: it.Root, Project: project, ID: it.ID, At: at})
		}
	})
	if warn != "" {
		fmt.Fprintln(stdout, warn)
	}
	if rememberErr != nil {
		fmt.Fprintf(stdout, "docket: note: this machine's local record wasn't updated (%v); the item is filed, and shows in docket list after the next sync.\n", rememberErr)
	}
	where := ""
	if project != "" {
		where = " in " + project
	}
	fmt.Fprintf(stdout, "docket: filed %s%s (%s). %s will see it in docket; keep working. An answer arrives as a message tagged [docket %s]. If it's settled in chat instead, run `docket done %s`.\n",
		it.ID, where, firstNonEmpty(urgency, "normal"), capitalize(person), it.ID, it.ID)
	return nil
}

// resolveProject decides which project an item belongs to. strict makes an
// unknown or missing project an error that lists the existing ones, so the
// agent can choose; without a project list on this machine yet, it files and
// lets the server decide.
func resolveProject(flag, newDesc string, ctx agentctx.Context, hasCtx bool, root, cwd, lead string, strict bool) (string, string, error) {
	if flag != "" {
		p := Slug(flag)
		if p == "" {
			return "", "", fmt.Errorf("--project %q isn't a usable name: use letters, digits and dashes", flag)
		}
		if !hasCtx || ctx.HasProject(p) || strings.TrimSpace(newDesc) != "" || !strict {
			return p, "", nil
		}
		return "", "", fmt.Errorf("there is no project %q yet.\n%s", p, projectHelp(ctx))
	}
	if !hasCtx {
		return "", "docket: this machine has no project list yet (it arrives with the next sync), so the server will pick the project. Next time pass --project.", nil
	}
	if p := ctx.ProjectForDir(root, cwd); p != "" {
		return p, "", nil
	}
	if p := ctx.Leads[lead]; lead != "" && p != "" {
		return p, "", nil
	}
	if !strict {
		return "", "", nil
	}
	return "", "", fmt.Errorf("which project is this for? Pass --project <name>.\n%s", projectHelp(ctx))
}

func projectHelp(ctx agentctx.Context) string {
	var b strings.Builder
	if len(ctx.Projects) > 0 {
		b.WriteString("Existing projects:\n")
		w := 0
		for _, p := range ctx.Projects {
			if len(p.Name) > w {
				w = len(p.Name)
			}
		}
		for _, p := range ctx.Projects {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, p.Name, p.Description)
		}
	}
	b.WriteString(`If none fits, start one: --project <short-name> --new-project "<one-line description>". Reuse an existing project when you can.`)
	return b.String()
}

// Done records that an item the agent filed is resolved, by id or by key.
func Done(args []string, stdout io.Writer, getenv func(string) string) error {
	pos, flags, err := parse(args, doneFlags)
	if err != nil {
		return err
	}
	key, hasKey := flags["key"]
	if hasKey && (strings.TrimSpace(key) == "" || len(pos) > 0) {
		return errors.New(`give either an id or --key, not both: docket done <dk-id> | --key <key> [--project <name>]`)
	}
	if (len(pos) != 1 || !strings.HasPrefix(pos[0], "dk-")) && !(len(pos) == 0 && key != "") {
		return errors.New(`usage: docket done <dk-id> | --key <key> [--project <name>] [--why "<how it was resolved>"]`)
	}
	r := record.New("cli.done", getenv)
	r.SessionID = sessionID(getenv)
	r.Cwd, _ = os.Getwd()
	d := &record.Done{Note: firstNonEmpty(flags["why"], flags["details"])}
	if len(pos) == 1 {
		d.ItemID = pos[0]
	}
	ctx, hasCtx := agentctx.Load()
	ctx = ctx.Merged(agentctx.LoadOverlay())
	closing := agentctx.Closed{ID: d.ItemID, RecordID: r.ID, At: time.Now().Unix()}
	if d.ItemID == "" {
		d.Key = key
		d.Explicit = flags["project"] != ""
		d.Project, _, _ = resolveProject(flags["project"], "", ctx, hasCtx, RepoRoot(r.Cwd), realPath(r.Cwd), getenv("DOCKET_LEAD"), false)
		// Hide exactly the item the server will close, when this machine can
		// tell which; otherwise only that key's items filed before now.
		if t, ok := ctx.KeyTarget(key, d.Project, d.Explicit, r.SessionID); ok {
			closing.ID = t.ID
		} else {
			closing.Key = key
			if d.Explicit {
				closing.Project = d.Project
			}
		}
	}
	r.Done = d
	if err := (spool.Spool{Dir: config.SpoolDir()}).Append(r); err != nil {
		return err
	}
	rememberErr := agentctx.Remember(func(o *agentctx.Overlay) { o.Closed = append(o.Closed, closing) })
	fmt.Fprintf(stdout, "docket: marked %s done.\n", firstNonEmpty(d.ItemID, "--key "+d.Key))
	if rememberErr != nil {
		fmt.Fprintf(stdout, "docket: note: this machine's local record wasn't updated (%v); docket list shows it closed after the next sync.\n", rememberErr)
	}
	return nil
}

// List prints this machine's open items, so an agent can find and close what
// it filed (after a context compaction, say).
func List(args []string, stdout io.Writer, getenv func(string) string) error {
	_, flags, err := parse(args, listFlags)
	if err != nil {
		return err
	}
	ctx, hasCtx := agentctx.Load()
	ctx = ctx.Merged(agentctx.LoadOverlay())
	if !hasCtx && len(ctx.Open) == 0 {
		fmt.Fprintln(stdout, "docket: this machine has no copy of the queue yet; it arrives with the next sync.")
		return nil
	}
	cwd, _ := os.Getwd()
	project := ""
	if flags["all"] != "true" {
		project, _, _ = resolveProject(flags["project"], "", ctx, true, RepoRoot(cwd), realPath(cwd), getenv("DOCKET_LEAD"), false)
	}
	me := sessionID(getenv)
	items := ctx.Open
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt > items[j].CreatedAt })
	n := 0
	for _, it := range items {
		if project != "" && it.Project != project {
			continue
		}
		mark := " "
		if me != "" && it.SessionID == me {
			mark = "*"
		}
		key := ""
		if it.Key != "" {
			key = "  key:" + it.Key
		}
		fmt.Fprintf(stdout, "%s %s  %-6s  %-12s  %s%s  (%s ago)\n", mark, it.ID, firstNonEmpty(it.Urgency, "normal"), it.Project, it.Title, key, ago(time.Unix(it.CreatedAt, 0)))
		n++
	}
	scope := "on this machine"
	if project != "" {
		scope = "in " + project + " on this machine (--all for every project)"
	}
	if n == 0 {
		fmt.Fprintf(stdout, "docket: nothing open %s.\n", scope)
	} else {
		asOf := time.Unix(ctx.AsOf, 0).In(ctx.Location())
		when := asOf.Format("15:04")
		if asOf.Format("2006-01-02") != time.Now().In(ctx.Location()).Format("2006-01-02") {
			when = asOf.Format("Jan 2 15:04")
		}
		last := "last change on the server " + when
		if !hasCtx {
			last = "only what this machine filed; the server's copy arrives with the next sync"
		}
		fmt.Fprintf(stdout, "docket: %d open %s; * = filed by this session (%s). Close one with `docket done <id>`.\n", n, scope, last)
	}
	return nil
}

// Projects prints the projects agents can file to.
func Projects(args []string, stdout io.Writer, getenv func(string) string) error {
	if len(args) > 0 && args[0] == "add" {
		return addProject(args[1:], stdout, getenv)
	}
	if len(args) > 0 {
		return errors.New(`usage: docket projects | docket projects add <name> --why "<one-line description>"`)
	}
	ctx, hasCtx := agentctx.Load()
	if !hasCtx {
		fmt.Fprintln(stdout, "docket: this machine has no project list yet; it arrives with the next sync. File with --project <name> meanwhile.")
		return nil
	}
	ctx = ctx.Merged(agentctx.LoadOverlay())
	cwd, _ := os.Getwd()
	for _, p := range ctx.Projects {
		fmt.Fprintf(stdout, "%s (%d open)  %s\n", p.Name, p.Open, p.Description)
		if len(p.Components) > 0 {
			fmt.Fprintf(stdout, "    components: %s\n", strings.Join(p.Components, ", "))
		}
		if len(p.Places) > 0 {
			fmt.Fprintf(stdout, "    places: %s\n", strings.Join(p.Places, ", "))
		}
	}
	if len(ctx.Places) > 0 {
		fmt.Fprintf(stdout, "\nPlaces in use: %s\n", strings.Join(ctx.Places, ", "))
	}
	root := RepoRoot(cwd)
	here, _, _ := resolveProject("", "", ctx, true, root, realPath(cwd), getenv("DOCKET_LEAD"), false)
	switch {
	case here != "":
		fmt.Fprintf(stdout, "\nThis folder files to %s by default.\n", here)
	case root == "":
		fmt.Fprintln(stdout, "\nThis folder isn't in a git repository, so it has no default project: pass --project each time.")
	default:
		fmt.Fprintln(stdout, "\nThis repository has no default project yet: pass --project, and the project you name here becomes its default.")
	}
	return nil
}

// addProject creates a project without filing anything, for setting up.
func addProject(args []string, stdout io.Writer, getenv func(string) string) error {
	pos, flags, err := parse(args, set("why", "details"))
	if err != nil {
		return err
	}
	desc := strings.TrimSpace(firstNonEmpty(flags["why"], flags["details"]))
	if len(pos) != 1 || Slug(pos[0]) == "" || desc == "" {
		return errors.New(`usage: docket projects add <name> --why "<one-line description>"`)
	}
	name := Slug(pos[0])
	r := record.New("cli.project", getenv)
	r.SessionID = sessionID(getenv)
	r.Cwd, _ = os.Getwd()
	r.Payload, _ = json.Marshal(map[string]string{"name": name, "description": desc})
	if err := (spool.Spool{Dir: config.SpoolDir()}).Append(r); err != nil {
		return err
	}
	agentctx.Remember(func(o *agentctx.Overlay) {
		o.Projects = append(o.Projects, agentctx.NewThing{Name: name, Description: desc, ID: r.ID, At: time.Now().Unix()})
	})
	fmt.Fprintf(stdout, "docket: project %s added (docket projects shows it now; the dashboard after the next sync). If it already exists, its description is only filled in when it had none.\n", name)
	return nil
}

// RepoRoot is the main repository folder containing dir: for a git worktree,
// the repository it belongs to, so every worktree maps to one project. ""
// outside a repository.
func RepoRoot(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-common-dir")
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	common := strings.TrimSpace(out.String())
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return "" // a bare repository or something unusual: don't guess
	}
	root := realPath(filepath.Dir(common))
	if home, _ := os.UserHomeDir(); root == realPath(home) || root == "/" {
		return ""
	}
	return root
}

// realPath resolves symlinks (on macOS, /tmp and /var are links into
// /private), so one folder always maps to one project however it was reached.
func realPath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// Slug normalises a project or place name.
func Slug(s string) string {
	s = strings.Trim(slugBad.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

// Person is how docket refers to the human: `docket init --person`, else
// the server's setting as it arrived in context.json.
func Person() string {
	if b, err := os.ReadFile(filepath.Join(config.Home(), "person")); err == nil {
		if p := strings.TrimSpace(string(b)); p != "" {
			return p
		}
	}
	if c, ok := agentctx.Load(); ok && c.Person != "" {
		return c.Person
	}
	return "the user"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// parseExpires accepts YYYY-MM-DD (end of that day), YYYY-MM-DDTHH:MM, today,
// tomorrow, or a relative 30m / 6h / 3d.
func parseExpires(s string, now time.Time) (int64, error) {
	endOfDay := func(t time.Time) int64 {
		return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location()).Unix()
	}
	var at int64
	switch low := strings.ToLower(strings.TrimSpace(s)); {
	case low == "today":
		at = endOfDay(now)
	case low == "tomorrow":
		at = endOfDay(now.AddDate(0, 0, 1))
	case relative.MatchString(low):
		m := relative.FindStringSubmatch(low)
		var n int
		fmt.Sscanf(m[1], "%d", &n)
		unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
		if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil || n < 0 || time.Duration(n) > 3650*24*time.Hour/unit {
			return 0, fmt.Errorf("--expires %q is too far off; use a date", s)
		}
		at = now.Add(time.Duration(n) * unit).Unix()
	default:
		if t, err := time.ParseInLocation("2006-01-02T15:04", s, now.Location()); err == nil {
			at = t.Unix()
		} else if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
			at = endOfDay(t)
		} else {
			return 0, fmt.Errorf("--expires takes YYYY-MM-DD, YYYY-MM-DDTHH:MM, today, tomorrow or 6h / 3d (got %q)", s)
		}
	}
	if at <= now.Unix() {
		return 0, fmt.Errorf("--expires %q is already past", s)
	}
	return at, nil
}

// parse accepts flags before or after the positional words, as
// --name value, --name=value, or bare --blocking / --physical.
func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func parse(args []string, allowed map[string]bool) ([]string, map[string]string, error) {
	var pos []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			// "-p x" or "-urgency now" is a mistyped flag, not title words (a
			// quoted title with spaces can't be a flag).
			if dashWord.MatchString(a) && !strings.ContainsAny(a, " \t") {
				return nil, nil, fmt.Errorf("unknown flag %s (flags take two dashes, like --project; to start a title with a dash, put -- before it, after the flags)", a)
			}
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
		if !allowed[name] {
			return nil, nil, fmt.Errorf("--%s doesn't apply here", name)
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

// Job records a long-running job's lifecycle so docket can tell whether it is
// really still running:
//
//	docket job register <name> --every 10m [--why "..."]
//	docket job beat <name> [--why "progress note"]
//	docket job done <name> [--failed] [--why "..."]
func Job(args []string, stdout io.Writer, getenv func(string) string) error {
	if len(args) < 2 {
		return errors.New(`usage: docket job register|beat|done <name> [--every 10m] [--failed] [--why "..."]`)
	}
	verb, rest := args[0], args[1:]
	var name, every, note string
	failed := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--failed":
			failed = true
		case a == "--every" || a == "--why" || a == "--details":
			if i+1 >= len(rest) {
				return fmt.Errorf("%s needs a value", a)
			}
			i++
			if a == "--every" {
				every = rest[i]
			} else {
				note = rest[i]
			}
		case strings.HasPrefix(a, "--every="):
			every = strings.TrimPrefix(a, "--every=")
		case strings.HasPrefix(a, "--why="):
			note = strings.TrimPrefix(a, "--why=")
		case strings.HasPrefix(a, "--"):
			return fmt.Errorf("unknown flag %s", a)
		default:
			if name != "" {
				return errors.New("one job name please (quote names with spaces)")
			}
			name = a
		}
	}
	if name == "" {
		return errors.New("missing job name")
	}
	payload := map[string]any{"name": name}
	if note != "" {
		payload["note"] = note
	}
	switch verb {
	case "register":
		d, err := time.ParseDuration(every)
		if err != nil || d < time.Minute {
			return errors.New("register needs --every <how often you'll beat>, at least 1m (e.g. --every 10m)")
		}
		payload["every"] = int64(d.Seconds())
	case "beat":
	case "done":
		payload["failed"] = failed
	default:
		return fmt.Errorf("unknown job command %q", verb)
	}
	r := record.New("job."+verb, getenv)
	r.SessionID = sessionID(getenv)
	r.Cwd, _ = os.Getwd()
	r.Payload, _ = json.Marshal(payload)
	if err := (spool.Spool{Dir: config.SpoolDir()}).Append(r); err != nil {
		return err
	}
	switch verb {
	case "register":
		fmt.Fprintf(stdout, "docket: tracking job %q; beat at least every %s (docket job beat %s) and finish with docket job done %s.\n", name, every, name, name)
	case "done":
		fmt.Fprintf(stdout, "docket: job %q finished.\n", name)
	}
	return nil
}

// Add appends details or evidence to an item this agent filed earlier:
//
//	docket add dk-xxxxxxxx --why "the benchmark numbers" --evidence chart.png
func Add(args []string, stdout io.Writer, getenv func(string) string) error {
	pos, flags, err := parse(args, addFlags)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !strings.HasPrefix(pos[0], "dk-") {
		return errors.New(`usage: docket add <dk-id> [--why "<more details>"] [--evidence <files>]`)
	}
	text := firstNonEmpty(flags["why"], flags["details"])
	sp := spool.Spool{Dir: config.SpoolDir()}
	var evs []record.Evidence
	for _, p := range splitList(flags["evidence"]) {
		e, err := storeEvidence(sp.Dir, p)
		if err != nil {
			return err
		}
		evs = append(evs, e)
	}
	if text == "" && len(evs) == 0 {
		return errors.New("nothing to add: give --why and/or --evidence")
	}
	r := record.New("cli.add", getenv)
	r.SessionID = sessionID(getenv)
	r.Cwd, _ = os.Getwd()
	r.Payload, _ = json.Marshal(map[string]any{"item_id": pos[0], "text": text, "evidence": evs})
	if err := sp.Append(r); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "docket: added to %s.\n", pos[0])
	return nil
}
