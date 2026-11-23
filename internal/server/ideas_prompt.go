package server

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// The router: one headless model call per dictation that splits it into
// ideas and decides where each goes. It reads only what docket tells it.

const routeSchema = `{"type":"object","properties":{"ideas":{"type":"array","items":{"type":"object","properties":{` +
	`"project":{"type":"string"},` +
	`"new_project_description":{"type":"string"},` +
	`"title":{"type":"string"},` +
	`"body":{"type":"string"},` +
	`"route":{"type":"string","enum":["research","build","save","send"]},` +
	`"session_id":{"type":"string"},` +
	`"reason":{"type":"string"}},` +
	`"required":["project","title","body","route","reason"]}}},"required":["ideas"]}`

type routed struct {
	Ideas []struct {
		Project    string `json:"project"`
		NewProject string `json:"new_project_description"`
		Title      string `json:"title"`
		Body       string `json:"body"`
		Route      string `json:"route"`
		SessionID  string `json:"session_id"`
		Reason     string `json:"reason"`
	} `json:"ideas"`
}

func (s *Server) routerPrompt(d store.Dictation) string {
	p := s.cfg.Person
	var b strings.Builder
	fmt.Fprintf(&b, "You route ideas that %s dictated (or typed) into docket, the dashboard where they run their coding agents. Split what they said into separate ideas and decide where each one goes. You can't run tools: everything you know is below.\n\n", p)
	if d.Source == "voice" {
		fmt.Fprintf(&b, "It came from speech-to-text, so expect misheard words, above all names: the projects, components and sessions below are the real names.")
		if said := s.cfg.Transcribe.SaidAs; len(said) > 0 {
			b.WriteString(" Some names come out as they sound:")
			for i, name := range sortedKeys(said) {
				if i > 0 {
					b.WriteString(";")
				}
				fmt.Fprintf(&b, " %q usually means %s", said[name], name)
			}
			b.WriteString(" (unless the context says otherwise).")
		}
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "<dictation>\n%s\n</dictation>\n\n", strings.TrimSpace(d.Text))
	if d.ProjectHint != "" {
		fmt.Fprintf(&b, "%s recorded it on the %q project's page: a strong hint, not a rule (an idea plainly about another project goes there).\n\n", p, d.ProjectHint)
	}

	b.WriteString("## Projects\n")
	awake := s.awakeMachines()
	projects, _ := s.st.Projects(false)
	for _, pr := range projects {
		fmt.Fprintf(&b, "- %s: %s\n", pr.Name, firstNonEmptyStr(pr.Description, "(no description)"))
		homes, _ := s.st.ProjectHomes(pr.Name)
		var hs []string
		for _, h := range homes {
			st := "always on"
			if m, ok := s.cfg.Machine(h.Machine); ok && m.Mode == "push" {
				st = "awake"
				if !awake[h.Machine] {
					st = "asleep"
				}
			}
			kind := ""
			if h.Clone {
				kind = ", docket's clone"
			}
			hs = append(hs, fmt.Sprintf("%s:%s (%s%s)", h.Machine, h.Dir, st, kind))
			if len(hs) == 4 {
				break
			}
		}
		if len(hs) > 0 {
			fmt.Fprintf(&b, "  code: %s\n", strings.Join(hs, "; "))
		}
		for _, l := range s.cfg.Leads {
			if l.Project == pr.Name {
				fmt.Fprintf(&b, "  lead: %s (a long-running agent for this area, on %s)\n", l.Name, l.Machine)
			}
		}
	}

	b.WriteString("\n## Sessions running now (last 48 hours)\n")
	n := 0
	cutoff := time.Now().Add(-48 * time.Hour).Unix()
	sessions, _ := s.st.Sessions(false, 300)
	dirs, _ := s.st.ProjectDirs("")
	for _, x := range sessions {
		if !x.HasPlugin || x.LastEventAt < cutoff {
			continue
		}
		project := ""
		if x.Lead != "" {
			project = s.leadProject(x.Lead)
		}
		if x.Idea != "" {
			if i, ok, _ := s.st.GetIdea(x.Idea); ok {
				project = i.Project
			}
		}
		if project == "" {
			var mine []store.ProjectDir
			for _, d := range dirs {
				if d.Machine == x.Machine {
					mine = append(mine, d)
				}
			}
			project = store.ProjectForDir(mine, x.Cwd)
		}
		what := ""
		switch {
		case x.Lead != "":
			what = "lead " + x.Lead
		case x.Idea != "":
			if i, ok, _ := s.st.GetIdea(x.Idea); ok {
				what = fmt.Sprintf("working on the idea %q", i.Title)
			}
		}
		name := firstNonEmptyStr(x.Name, filepath.Base(x.Cwd))
		fmt.Fprintf(&b, "- session_id %s · project %s · %s · %s · %s", x.ID, firstNonEmptyStr(project, "?"), x.Machine, firstNonEmptyStr(x.State, "?"), name)
		if what != "" {
			fmt.Fprintf(&b, " · %s", what)
		}
		if m := strings.TrimSpace(x.LastMessage); m != "" {
			fmt.Fprintf(&b, "\n  last said: %q", clipStr(strings.Join(strings.Fields(m), " "), 300))
		}
		b.WriteString("\n")
		if n++; n >= 40 {
			break
		}
	}
	if n == 0 {
		b.WriteString("(none)\n")
	}

	b.WriteString("\n## Ideas already in docket\n")
	ideas, _ := s.st.RecentIdeas(time.Now().Add(-30 * 24 * time.Hour).Unix())
	sort.Slice(ideas, func(i, j int) bool { return ideas[i].CreatedAt > ideas[j].CreatedAt })
	n = 0
	for _, i := range ideas {
		if i.Status == store.IdeaDismissed {
			continue
		}
		fmt.Fprintf(&b, "- %s · %s · %s · %s\n", i.ID, i.Project, i.Status, i.Title)
		if n++; n >= 60 {
			break
		}
	}
	if n == 0 {
		b.WriteString("(none)\n")
	}

	fmt.Fprintf(&b, `
## Routes
- research: the default for a concrete feature, change or bug. An agent researches it in the project's code, critiques it, and proposes a plan that %[1]s approves with one tap.
- build: only when %[1]s plainly says to just do it or build it without a proposal.
- save: an idea for later: vague, long-term, or they said to just note or save it.
- send: a correction or addition for work one of the running sessions above is doing right now (give its session_id). Only when the dictation is clearly about that session's current work; otherwise research.

## For each idea
- project: one of the projects above. Only for a clearly new project, a new short name (lowercase, dashes) and new_project_description (one line).
- title: at most 80 characters, scannable: what the idea is.
- body: the idea restated cleanly in markdown, keeping every detail, example, constraint and preference they gave. Fix misheard words where the context makes the meaning clear; otherwise keep their wording. Add nothing of your own.
- reason: one line: why this project and route.
One idea per separate change, not per sentence: keep together what belongs together. A dictation with one idea gives one. If something they said adds to an idea already in docket, say so in the body ("Adds to %[2]s: ...").
`, p, "idea-…")
	return b.String()
}

// awakeMachines lists machines heard from in the last 90 seconds (for push
// machines, which sync every 30 s, that means awake).
func (s *Server) awakeMachines() map[string]bool {
	out := map[string]bool{}
	ms, _ := s.st.Machines()
	for _, m := range ms {
		if time.Since(time.Unix(m.LastSeenAt, 0)) < 90*time.Second {
			out[m.Name] = true
		}
	}
	return out
}

// spot is where an idea's session runs.
type spot struct {
	m      Machine
	dir    string // as that machine sees it (may start with ~/)
	kind   string // home | clone | workspace
	remote string
	away   string // for a clone: where the usual checkout is
}

func (sp spot) describe() string {
	switch sp.kind {
	case "clone":
		return sp.m.Name + ":" + sp.dir + " (docket's clone)"
	case "workspace":
		return sp.m.Name + ":" + sp.dir + " (scratch folder)"
	}
	return sp.m.Name + ":" + sp.dir
}

// researchPrompt is the first message of the session docket starts for an idea.
func (s *Server) researchPrompt(i store.Idea, d store.Dictation, sp spot) string {
	p := s.cfg.Person
	var b strings.Builder
	if isEmailIdea(i) {
		fmt.Fprintf(&b, "[docket %s] Someone emailed %s asking for this change to the %s project. docket's drafting agent read the email and the code and wrote the plan below. The plan is all you get.", i.ID, p, firstNonEmptyStr(i.Project, "unsorted"))
	} else {
		fmt.Fprintf(&b, "[docket %s] %s dictated an idea for the %s project, and docket started you to work on it.", i.ID, p, firstNonEmptyStr(i.Project, "unsorted"))
	}
	if d.Source == "voice" {
		b.WriteString(" It came from speech-to-text, so a few words may be wrong; the full dictation is at the end.")
		for _, name := range sortedKeys(s.cfg.Transcribe.SaidAs) {
			fmt.Fprintf(&b, " In it, %q usually means %s.", s.cfg.Transcribe.SaidAs[name], name)
		}
	}
	fmt.Fprintf(&b, "\n\n## The idea: %s\n\n%s\n\n", i.Title, strings.TrimSpace(i.Body))

	switch sp.kind {
	case "clone":
		fmt.Fprintf(&b, "## Where you are\nThis folder is docket's clone of %s on %s; the usual checkout is %s, which is asleep. docket ran `git pull` here just now. Push your branches to origin as usual.\n\n", sp.remote, sp.m.Name, sp.away)
	case "workspace":
		fmt.Fprintf(&b, "## Where you are\nThere's no code folder for this project yet, so this is a scratch folder. If building it needs a new repository, create it under %s.\n\n", s.cfg.Ideas.CloneDir)
	}

	build := fmt.Sprintf(`build it completely: on a new branch in a new git worktree (never on this checkout's current branch), with tests; then open a pull request, wait for its CI, merge it, and release or deploy it the way this repository does. Don't ask for approval again: agents here handle git, pull requests, merges and releases themselves. Bring %[1]s only what docket is for (credentials, money, irreversible data loss, hardware, anything physical), filed with docket as usual. When it has shipped, run `+"`docket idea done %[2]s --why \"<what shipped, with the pull request link>\"`"+`.`, p, i.ID)

	if i.Route == "build" && isEmailIdea(i) {
		fmt.Fprintf(&b, "## Your job: build it\n%s approved this plan. Read what you need first (this repository's CLAUDE.md, docs and the code involved), then %s Your summary in --why goes into the reply, so say plainly what changed and where to find it.\n", p, build)
	} else if i.Route == "build" {
		fmt.Fprintf(&b, "## Your job: build it\n%s asked for this to be built straight away, without a proposal. Read what you need first (this repository's CLAUDE.md, docs and the code involved), then %s\n", p, build)
	} else {
		fmt.Fprintf(&b, `## Your job: research it and propose how to do it
1. Understand it. Read this repository (its CLAUDE.md, docs and the code involved), and the web if it helps. Think critically: is it worth doing? What is the best way, and is there a simpler or better version? What could go wrong? Don't change any files yet.
2. File your proposal (one item; running it again updates it):
   docket propose "<what you'd build, at most 80 characters>" --idea %[1]s --project %[2]s --why "<markdown>"
   In --why, in this order: **What I heard** (the idea in a line or two, so a mishearing gets caught), **What I found**, **Proposal** (concrete: the plan and the files involved), **Risks and open questions**, **Size** (rough effort). If your research says it isn't worth doing, say so and add --recommend "Save for later".
3. End your turn and wait. %[3]s's answer arrives as a message tagged [docket dk-…]:
   - "Build it": approved. Now %[4]s
   - "Save for later": run `+"`docket idea save %[1]s --why \"<what you found, briefly>\"`"+` and stop.
   - Anything else is a reply: answer it, revise the plan, and run docket propose again if the plan changed.
`, i.ID, firstNonEmptyStr(i.Project, store.Unsorted), p, build)
	}
	b.WriteString("\nThis repository's own instructions (CLAUDE.md and the like) take precedence over these where they differ.\n")
	if strings.TrimSpace(d.Text) != "" && strings.TrimSpace(d.Text) != strings.TrimSpace(i.Body) {
		fmt.Fprintf(&b, "\n## The full dictation (it may cover other ideas, which other sessions handle)\n\n%s\n", strings.TrimSpace(d.Text))
	}
	return b.String()
}
