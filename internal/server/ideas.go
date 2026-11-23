package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// Ideas: route each sent dictation, start a session for every idea that
// needs research (or building), and keep their state true. docket decides
// nothing for the human: the router and the sessions are agents, and an idea
// is only built after the human taps Build it (or said "just build it").

const (
	ideaStartTimeout = 10 * time.Minute // a start no session has answered
	laptopSilence    = 6 * time.Hour    // a laptop took a start and never said how it went
	routeAttempts    = 2
	stopAfterDone    = 2 * time.Hour  // an idea's session is stopped this long after the idea ended
	stopIdleProposal = 24 * time.Hour // a session idle this long on a proposal is stopped (an answer resumes it)
)

// Proposal options, as the item shows them.
const (
	ProposeBuild = "Build it"
	ProposeSave  = "Save for later"
)

func (s *Server) ideasTick(ctx context.Context) {
	s.finishStale()
	s.unstickRouting()
	s.watchIdeas(ctx)
	s.placeIdeas(ctx)
	s.routeDictations(ctx)
	s.placeIdeas(ctx) // what the router just made
	if last := s.st.Get("audio:pruned"); last != time.Now().Format("2006-01-02") {
		s.st.Set("audio:pruned", time.Now().Format("2006-01-02"))
		s.pruneAudio()
	}
}

// ---- routing ----------------------------------------------------------------

// unstickRouting puts back dictations a restart left half-routed.
func (s *Server) unstickRouting() {
	ds, _ := s.st.Dictations(store.DictRouting)
	for _, d := range ds {
		if time.Since(time.Unix(d.UpdatedAt, 0)) > 10*time.Minute {
			s.st.EditDictation(d.ID, func(x *store.Dictation) error {
				if x.Status == store.DictRouting {
					x.Status = store.DictSent
				}
				return nil
			})
		}
	}
}

func (s *Server) routeDictations(ctx context.Context) {
	ds, err := s.st.Dictations(store.DictSent)
	if err != nil {
		return
	}
	for _, d := range ds {
		if ctx.Err() != nil {
			return
		}
		s.routeOne(ctx, d)
	}
}

func (s *Server) routeOne(ctx context.Context, d store.Dictation) {
	d, err := s.st.EditDictation(d.ID, func(x *store.Dictation) error {
		if x.Status != store.DictSent {
			return errors.New("not waiting to be routed")
		}
		x.Status = store.DictRouting
		return nil
	})
	if err != nil {
		return
	}
	out, err := s.route(ctx, d)
	if err == nil && len(out.Ideas) == 0 {
		err = errors.New("the router found no idea in it")
	}
	if err != nil {
		s.log.Printf("dictation %s: route: %v", d.ID, err)
		s.st.EditDictation(d.ID, func(x *store.Dictation) error {
			x.Attempts++
			x.Error = "routing failed: " + err.Error()
			x.Status = store.DictSent
			if x.Attempts >= routeAttempts {
				x.Status = store.DictFailed
			}
			return nil
		})
		return
	}
	for _, r := range out.Ideas {
		s.addRoutedIdea(d, r.Project, r.NewProject, r.Title, r.Body, r.Route, r.SessionID, r.Reason)
	}
	s.st.EditDictation(d.ID, func(x *store.Dictation) error {
		x.Status, x.Error = store.DictRouted, ""
		return nil
	})
}

// route asks the model where the dictation's ideas go. Tests replace the
// claude CLI with a script, as for the brain.
func (s *Server) route(ctx context.Context, d store.Dictation) (routed, error) {
	var out routed
	raw, _, err := s.runClaude(ctx, s.chiefDefaults().Claude, s.cfg.Ideas.RouterModel, s.routerPrompt(d), routeSchema, s.cfg.Ideas.BudgetUSD, 5*time.Minute)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("unreadable answer: %w", err)
	}
	return out, nil
}

// addRoutedIdea checks one routed idea and stores it. What the router got
// wrong falls back to something safe: an unknown project to the hint, an
// unknown session to research.
func (s *Server) addRoutedIdea(d store.Dictation, project, newDesc, title, body, route, sessionID, reason string) {
	project = store.Slug(project)
	if !s.projectExists(project) {
		if project != "" && strings.TrimSpace(newDesc) != "" {
			s.st.EnsureProject(project, clipStr(strings.TrimSpace(newDesc), 200))
		} else if d.ProjectHint != "" && s.projectExists(d.ProjectHint) {
			project = d.ProjectHint
		} else {
			project = store.Unsorted
		}
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		title = clipStr(strings.Join(strings.Fields(body), " "), 80)
	}
	i := store.Idea{ID: ulid.ShortID("idea"), DictationID: d.ID, Project: project, Title: clipStr(title, 120),
		Body: clipStr(strings.TrimSpace(firstNonEmptyStr(body, title)), 20000), Route: route, Reason: clipStr(strings.TrimSpace(reason), 400)}
	var sess store.Session
	if route == "send" {
		x, ok, _ := s.st.GetSession(sessionID)
		if ok && x.HasPlugin && x.State != "ended" {
			sess = x
		} else {
			i.Route = "research"
			i.Reason = strings.TrimSpace(i.Reason + " (the session to send it to isn't running, so it's researched instead)")
		}
	}
	switch i.Route {
	case "save":
		i.Status = store.IdeaSaved
	case "send":
		i.Status, i.SessionID, i.Machine = store.IdeaSent, sess.ID, sess.Machine
	case "build":
		i.Status = store.IdeaQueued
	default:
		i.Route, i.Status = "research", store.IdeaQueued
	}
	if err := s.st.AddIdea(i); err != nil {
		s.log.Printf("idea: %v", err)
		return
	}
	if i.Status == store.IdeaSent {
		nonce := randomNonce()
		text := fmt.Sprintf("[docket #%s] %s dictated this for you in docket (%s):\n\n%s", nonce, s.cfg.Person, i.ID, i.Body)
		if d.Source == "voice" {
			text += "\n\n(It came from speech-to-text, so a word may be wrong.)"
		}
		s.st.AddDelivery(store.Delivery{ID: ulid.New(time.Now()), Machine: sess.Machine, SessionID: sess.ID, Nonce: nonce, Text: text})
		s.kickDeliver()
	}
}

func (s *Server) projectExists(name string) bool {
	if name == "" {
		return false
	}
	ps, _ := s.st.Projects(false)
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

// ---- placement and starts ---------------------------------------------------

// placeIdeas starts sessions for queued ideas while there are free slots.
func (s *Server) placeIdeas(ctx context.Context) {
	all, err := s.st.Ideas(store.IdeaQueued, store.IdeaWaiting, store.IdeaStarting, store.IdeaResearching, store.IdeaBuilding)
	if err != nil {
		return
	}
	running := 0
	for _, i := range all {
		if store.IdeaRunning(i.Status) {
			running++
		}
	}
	for _, i := range all {
		if i.Status != store.IdeaQueued && i.Status != store.IdeaWaiting {
			continue
		}
		if running >= s.cfg.Ideas.MaxRunning || ctx.Err() != nil {
			return
		}
		if s.placeOne(ctx, i) {
			running++
		}
	}
}

// placeOne finds where an idea runs and starts its session there. It
// reports whether a start went out.
func (s *Server) placeOne(ctx context.Context, i store.Idea) bool {
	sp, wait, err := s.place(ctx, i)
	if err != nil || wait {
		s.st.EditIdea(i.ID, func(x *store.Idea) error {
			if x.Status != store.IdeaQueued && x.Status != store.IdeaWaiting {
				return nil
			}
			if wait {
				x.Status = store.IdeaWaiting // keeps the last error, if any
				return nil
			}
			x.Status, x.Error = store.IdeaFailed, err.Error()
			return nil
		})
		return false
	}
	// Claim it before starting anything: a dismiss or save that landed
	// meanwhile wins, and once it's starting the page won't take one.
	old := i
	run := ""
	claimed, err := s.st.EditIdea(i.ID, func(x *store.Idea) error {
		if x.Status != store.IdeaQueued && x.Status != store.IdeaWaiting {
			return errors.New("no longer queued")
		}
		x.Attempts++
		run = fmt.Sprintf("%s-%d", x.ID, x.Attempts) // never the name of an earlier try's session
		x.Status, x.Error = store.IdeaStarting, ""
		x.Machine, x.Dir, x.Place, x.SessionName, x.StartedAt = sp.m.Name, sp.dir, sp.describe(), run, time.Now().Unix()
		x.SessionID, x.StoppedAt = "", 0
		return nil
	})
	if err != nil {
		return false
	}
	if old.SessionName != "" && old.SessionName != run {
		s.stopRun(ctx, old.Machine, old.SessionName, old.ID) // an earlier try's session, if it's still there
	}
	d, _, _ := s.st.GetDictation(claimed.DictationID)
	prompt := s.researchPrompt(claimed, d, sp)
	args := s.ideaClaudeArgs()
	if sp.m.Mode == "push" {
		err = s.st.AddStart(store.Start{ID: ulid.New(time.Now()), Machine: sp.m.Name, Op: "start", Name: run, Cwd: sp.dir, IdeaID: i.ID, Prompt: prompt, Args: args})
	} else {
		err = s.startSession(ctx, sp.m, prompt, append([]string{"session", "start", "--name", run, "--cwd", sp.dir, "--idea", i.ID, "--"}, args...))
	}
	if err != nil {
		s.st.EditIdea(i.ID, func(x *store.Idea) error {
			if x.Status == store.IdeaStarting && x.SessionName == run {
				x.Status, x.Error = store.IdeaFailed, "couldn't start a session on "+sp.m.Name+": "+err.Error()
			}
			return nil
		})
		return false
	}
	return true
}

func (s *Server) ideaClaudeArgs() []string {
	args := append([]string{}, s.cfg.Ideas.ClaudeArgs...)
	for _, a := range args {
		if a == "--model" || strings.HasPrefix(a, "--model=") {
			return args
		}
	}
	return append(args, "--model", s.cfg.Ideas.Model)
}

func (s *Server) startSession(ctx context.Context, m Machine, prompt string, args []string) error {
	if s.startFn != nil {
		return s.startFn(ctx, m, prompt, args)
	}
	_, err := s.remote(ctx, m, strings.NewReader(prompt), args...)
	return err
}

// localMachine is the server's own machine, where clones and scratch folders go.
func (s *Server) localMachine() (Machine, bool) {
	for _, m := range s.cfg.Machines {
		if m.Mode == "local" {
			return m, true
		}
	}
	return Machine{}, false
}

// place picks where an idea's session runs, first match wins:
//  1. the project's lead, on a machine docket can always reach
//  2. another folder of the project on such a machine
//  3. its lead or folder on a laptop that is awake
//  4. a clone of its repository on the server (the laptop is asleep)
//  5. nothing reachable and no repository to clone: wait for the laptop
//  6. no folder anywhere (a new project): a scratch folder on the server
func (s *Server) place(ctx context.Context, i store.Idea) (sp spot, wait bool, err error) {
	avoid := map[string]string{} // machines a start for this idea failed on since the last fresh try, and why
	if starts, _ := s.st.StartsForIdea(i.ID); len(starts) > 0 {
		for _, st := range starts {
			if st.Op == "start" && st.Status == "failed" && st.CreatedAt > i.ResetAt {
				if _, seen := avoid[st.Machine]; !seen {
					avoid[st.Machine] = firstNonEmptyStr(st.Error, "it failed")
				}
			}
		}
	}
	awake := s.awakeMachines()
	reachable := func(name string, laptopOK bool) (Machine, bool) {
		m, ok := s.cfg.Machine(name)
		if _, bad := avoid[name]; !ok || bad {
			return m, false
		}
		if m.Mode == "push" {
			return m, laptopOK && awake[name]
		}
		return m, !laptopOK
	}
	homes, _ := s.st.ProjectHomes(i.Project)
	pushHome := ""
	for _, laptop := range []bool{false, true} {
		for _, l := range s.cfg.Leads {
			if l.Project != i.Project || !l.On() {
				continue
			}
			if m, ok := reachable(l.Machine, laptop); ok {
				return spot{m: m, dir: l.Dir, kind: "home"}, false, nil
			}
			if m, ok := s.cfg.Machine(l.Machine); ok && m.Mode == "push" && pushHome == "" {
				pushHome = l.Machine + ":" + l.Dir
			}
		}
		for _, h := range homes {
			if h.Clone {
				continue
			}
			if m, ok := reachable(h.Machine, laptop); ok {
				return spot{m: m, dir: h.Dir, kind: "home"}, false, nil
			}
			if m, ok := s.cfg.Machine(h.Machine); ok && m.Mode == "push" && pushHome == "" {
				pushHome = h.Machine + ":" + h.Dir
			}
		}
	}
	local, hasLocal := s.localMachine()
	remote := ""
	for _, h := range homes {
		if h.Remote != "" {
			remote = h.Remote
			break
		}
	}
	if remote != "" && hasLocal {
		dir, err := s.ensureClone(ctx, local, i.Project, remote)
		if err != nil {
			return sp, false, err
		}
		return spot{m: local, dir: dir, kind: "clone", remote: remote, away: firstNonEmptyStr(pushHome, "on another machine")}, false, nil
	}
	if len(homes) > 0 || pushHome != "" {
		// Waiting only helps if the machine with its code can take it.
		if why, bad := avoid[strings.SplitN(pushHome, ":", 2)[0]]; pushHome != "" && bad {
			return sp, false, fmt.Errorf("couldn't start on %s (%s), and there's no repository to clone; fix that, then Try again", strings.SplitN(pushHome, ":", 2)[0], why)
		}
		return sp, true, nil
	}
	if !hasLocal {
		return sp, false, errors.New("no folder for this project, and no local machine to make one on")
	}
	dir := filepath.Join(s.cfg.DataDir, "workspaces", firstNonEmptyStr(store.Slug(i.Project), store.Unsorted))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return sp, false, err
	}
	return spot{m: local, dir: dir, kind: "workspace"}, false, nil
}

// ensureClone clones a project's repository into [ideas] clone_dir on the
// server (or updates the clone made last time) and maps it to the project.
func (s *Server) ensureClone(ctx context.Context, m Machine, project, remote string) (string, error) {
	if !cloneable(remote) {
		return "", fmt.Errorf("docket only clones https, ssh or git@ remotes, not %q", remote)
	}
	name := strings.TrimSuffix(filepath.Base(strings.TrimRight(remote, "/")), ".git")
	if name == "" || name == "." || name == "/" || strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("can't tell a folder name from %s", remote)
	}
	dir := filepath.Join(expand(s.cfg.Ideas.CloneDir), name)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := os.Stat(dir); err == nil {
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output()
		if err != nil || !sameRemote(strings.TrimSpace(string(out)), remote) {
			return "", fmt.Errorf("%s already exists and isn't a clone of %s", dir, remote)
		}
		// Best effort: a dirty tree or a diverged branch keeps what it has.
		exec.CommandContext(ctx, "git", "-C", dir, "pull", "--ff-only", "-q").Run()
	} else {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, "git", "clone", "-q", "--", remote, dir)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git clone %s: %s", remote, clipErr(firstNonEmptyStr(string(out), err.Error())))
		}
	}
	if err := s.st.AddClone(m.Name, dir, project, remote); err != nil {
		return "", err
	}
	return dir, nil
}

// cloneable accepts the remotes docket will clone: https, ssh, scp-like
// git@host:path, or a folder on the server itself.
func cloneable(remote string) bool {
	switch {
	case strings.ContainsAny(remote, " \t\r\n") || strings.HasPrefix(remote, "-"):
		return false
	case strings.HasPrefix(remote, "https://"), strings.HasPrefix(remote, "ssh://"):
		return true
	case strings.HasPrefix(remote, "git@") && strings.Contains(remote, ":") && !strings.Contains(remote, "::"):
		return true
	case filepath.IsAbs(remote) && !strings.Contains(remote, "::"):
		return true
	}
	return false
}

// sameRemote compares two git URLs, ignoring the forms that name the same
// repository (https vs ssh, a trailing .git).
func sameRemote(a, b string) bool {
	norm := func(u string) string {
		u = strings.ToLower(strings.TrimSpace(u))
		u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
		for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
			u = strings.TrimPrefix(u, p)
		}
		u = strings.TrimPrefix(u, "git@")
		if at := strings.LastIndex(u, "@"); at >= 0 {
			u = u[at+1:]
		}
		return strings.Replace(u, ":", "/", 1)
	}
	return norm(a) == norm(b)
}

// ---- keeping ideas true -------------------------------------------------------

func (s *Server) watchIdeas(ctx context.Context) {
	ideas, err := s.st.Ideas(store.IdeaStarting, store.IdeaResearching, store.IdeaBuilding, store.IdeaProposed,
		store.IdeaBuilt, store.IdeaSaved, store.IdeaDismissed)
	if err != nil {
		return
	}
	now := time.Now()
	for _, i := range ideas {
		var sess store.Session
		has := false
		if i.SessionID != "" {
			sess, has, _ = s.st.GetSession(i.SessionID)
		}
		switch i.Status {
		case store.IdeaStarting:
			s.watchStart(i, now)
		case store.IdeaResearching, store.IdeaBuilding:
			if has && sess.State == "ended" && sess.EndedAt > 0 && now.Sub(time.Unix(sess.EndedAt, 0)) > 2*time.Minute {
				what := "before it proposed anything"
				if i.Status == store.IdeaBuilding {
					what = "before it finished building"
				}
				s.st.EditIdea(i.ID, func(x *store.Idea) error {
					if x.Status == i.Status {
						x.Status, x.Error = store.IdeaFailed, "the session ended "+what
					}
					return nil
				})
			}
		case store.IdeaProposed:
			if has && sess.State == "idle" && now.Sub(time.Unix(sess.LastEventAt, 0)) > stopIdleProposal {
				if m, ok := s.cfg.Machine(sess.Machine); ok && m.Mode != "push" {
					s.stopIdeaSession(ctx, i, sess)
				}
			}
		default: // ended: stop its session after a while
			if i.SessionName != "" && i.Route != "send" && i.EndedAt > 0 && now.Sub(time.Unix(i.EndedAt, 0)) > stopAfterDone &&
				(!has || (sess.State != "ended" && sess.State != "busy")) {
				s.stopIdeaSession(ctx, i, sess)
			}
		}
	}
}

// watchStart notices a start that never became a session.
func (s *Server) watchStart(i store.Idea, now time.Time) {
	if i.SessionID != "" || now.Sub(time.Unix(i.StartedAt, 0)) < ideaStartTimeout {
		return
	}
	m, _ := s.cfg.Machine(i.Machine)
	if m.Mode == "push" {
		starts, _ := s.st.StartsForIdea(i.ID)
		found := false
		fail := func(why string) {
			s.st.EditIdea(i.ID, func(x *store.Idea) error {
				if x.Status == store.IdeaStarting && x.SessionID == "" && x.SessionName == i.SessionName {
					x.Status, x.Error = store.IdeaFailed, why
				}
				return nil
			})
		}
		for _, st := range starts {
			if st.Op != "start" || st.Name != i.SessionName {
				continue
			}
			found = true
			switch {
			case st.Status == "pending" && s.st.ExpireStart(st.ID):
				// Never handed out (the laptop went to sleep): place it again.
				s.st.EditIdea(i.ID, func(x *store.Idea) error {
					if x.Status == store.IdeaStarting && x.SessionID == "" && x.SessionName == i.SessionName {
						x.Status, x.Error = store.IdeaQueued, i.Machine+" went to sleep before it picked this up"
					}
					return nil
				})
			case st.Status == "sent" && now.Sub(time.Unix(st.SentAt, 0)) > laptopSilence:
				// Handed out: it may yet run there, so never start a second one.
				fail(i.Machine + " took the session but never reported it started; check tmux there (" + i.SessionName + "), then Try again")
			case st.Status == "started" && now.Sub(time.Unix(i.StartedAt, 0)) > 3*ideaStartTimeout:
				fail("the session started on " + i.Machine + " but never reported in (Claude may have failed to launch); check `tmux attach -t " + i.SessionName + "` there, then Try again")
			case st.Status == "failed":
				fail("couldn't start on " + i.Machine + ": " + st.Error)
			}
		}
		if !found {
			// Claimed, but its start never got queued (a crash between the
			// two): nothing went out, so place it again.
			s.st.EditIdea(i.ID, func(x *store.Idea) error {
				if x.Status == store.IdeaStarting && x.SessionID == "" && x.SessionName == i.SessionName {
					x.Status = store.IdeaQueued
				}
				return nil
			})
		} else if now.Sub(time.Unix(i.StartedAt, 0)) > laptopSilence {
			fail("no session reported in on " + i.Machine + " for " + i.SessionName + "; check tmux there, then Try again")
		}
		return
	}
	s.st.EditIdea(i.ID, func(x *store.Idea) error {
		if x.Status == store.IdeaStarting && x.SessionID == "" {
			x.Status = store.IdeaFailed
			x.Error = fmt.Sprintf("the session on %s didn't report in within %d minutes (check `tmux attach -t %s` there)", i.Machine, int(ideaStartTimeout.Minutes()), i.SessionName)
		}
		return nil
	})
}

// stopIdeaSession ends the tmux session docket started for an idea. A later
// answer resumes it.
func (s *Server) stopIdeaSession(ctx context.Context, i store.Idea, sess store.Session) {
	if i.SessionName == "" || (i.StoppedAt > 0 && time.Since(time.Unix(i.StoppedAt, 0)) < 10*time.Minute) {
		return
	}
	s.stopRun(ctx, firstNonEmptyStr(sess.Machine, i.Machine), i.SessionName, i.ID)
	s.st.EditIdea(i.ID, func(x *store.Idea) error { x.StoppedAt = time.Now().Unix(); return nil })
}

// stopRun ends one tmux session docket started for an idea, on its machine.
func (s *Server) stopRun(ctx context.Context, machine, name, ideaID string) {
	m, ok := s.cfg.Machine(machine)
	if !ok || name == "" {
		return
	}
	var err error
	if m.Mode == "push" {
		err = s.st.AddStart(store.Start{ID: ulid.New(time.Now()), Machine: m.Name, Op: "stop", Name: name, IdeaID: ideaID})
	} else if s.startFn == nil { // tests replace starts; they never stop real sessions
		_, err = s.remote(ctx, m, nil, "session", "stop", "--name", name)
	}
	if err != nil {
		s.log.Printf("idea %s: stop %s: %v", ideaID, name, err)
	}
}

// ---- the human's decisions ------------------------------------------------------

// ideaDecided keeps an idea in step with what the human did to its proposal.
func (s *Server) ideaDecided(before store.Item, a Action) {
	i, ok := s.st.IdeaByItem(before.ID)
	if !ok {
		return
	}
	after, _, _ := s.st.GetItem(before.ID)
	next := ""
	switch a.Kind {
	case "choose":
		if a.Option >= 0 && a.Option < len(before.Options) {
			switch before.Options[a.Option] {
			case ProposeBuild:
				next = store.IdeaBuilding
			case ProposeSave:
				next = store.IdeaSaved
			}
		}
	case "approve":
		next = store.IdeaBuilding
	case "done", "dismiss", "noise":
		if closedStatus(after.Status) {
			next = store.IdeaDismissed
		}
	case "stash":
		next = store.IdeaSaved
	case "undo", "reopen":
		if after.Status == "open" {
			next = store.IdeaProposed
		}
	}
	if next == "" || next == i.Status || i.Status == store.IdeaBuilt {
		return
	}
	// No session wrote it (docket filed it from an email): building means
	// starting a session, given the approved plan and nothing else.
	fromEmail := i.SessionID == "" && strings.HasPrefix(i.Reason, emailIdeaMark)
	if next == store.IdeaBuilding && fromEmail {
		next = store.IdeaQueued
	}
	s.st.EditIdea(i.ID, func(x *store.Idea) error {
		x.Status = next
		if next == store.IdeaBuilding || next == store.IdeaQueued {
			x.Error = ""
		}
		if fromEmail && next == store.IdeaQueued {
			x.Route = "build"
		}
		return nil
	})
	if next == store.IdeaQueued {
		s.kickIdeas()
	}
}

// ---- the Ideas page's buttons ---------------------------------------------------

func (s *Server) ideaAct(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	i, ok, err := s.st.GetIdea(id)
	if err == nil && !ok {
		err = errors.New("no such idea")
	}
	if err == nil {
		err = s.doIdeaAct(r.Context(), i, action)
	}
	s.actDone(w, r, "/ideas", err)
}

func (s *Server) doIdeaAct(ctx context.Context, i store.Idea, action string) error {
	switch action {
	case "research", "build":
		_, err := s.st.EditIdea(i.ID, func(x *store.Idea) error {
			if store.IdeaRunning(x.Status) || x.Status == store.IdeaProposed {
				return errors.New("an agent is already on it")
			}
			x.Route, x.Status, x.Error = action, store.IdeaQueued, ""
			x.SessionID, x.ItemID, x.ResetAt = "", "", time.Now().Unix()
			return nil
		})
		s.kickIdeas()
		return err
	case "save":
		_, err := s.st.EditIdea(i.ID, func(x *store.Idea) error {
			if store.IdeaRunning(x.Status) {
				return errors.New("an agent is working on it")
			}
			x.Status, x.Error = store.IdeaSaved, ""
			return nil
		})
		return err
	case "dismiss":
		if _, err := s.st.EditIdea(i.ID, func(x *store.Idea) error {
			if store.IdeaRunning(x.Status) {
				return errors.New("an agent is working on it; dismiss its proposal when it comes")
			}
			x.Status = store.IdeaDismissed
			return nil
		}); err != nil {
			return err
		}
		if i.ItemID != "" {
			if it, ok, _ := s.st.GetItem(i.ItemID); ok && !closedStatus(it.Status) {
				return s.st.SetStatus(it.ID, "dismissed", "dismissed from the Ideas page", "dismiss")
			}
		}
		return nil
	case "retry":
		if i.Status != store.IdeaFailed {
			return errors.New("only a failed idea can be retried")
		}
		// A session that exists picks up where it left off (an ended one is
		// resumed); otherwise start afresh.
		if sess, ok, _ := s.st.GetSession(i.SessionID); ok && i.SessionID != "" {
			nonce := randomNonce()
			what := "research it and file your proposal with docket propose"
			if i.Route == "build" || i.ItemID != "" {
				what = "finish the work, then run docket idea done " + i.ID
			}
			if err := s.st.AddDelivery(store.Delivery{ID: ulid.New(time.Now()), Machine: sess.Machine, SessionID: sess.ID, Nonce: nonce,
				Text: fmt.Sprintf("[docket #%s] Please carry on with %s (%q): %s.", nonce, i.ID, i.Title, what)}); err != nil {
				return err
			}
			s.kickDeliver()
			next := store.IdeaResearching
			if i.ItemID != "" || i.Route == "build" {
				next = store.IdeaBuilding
			}
			_, err := s.st.EditIdea(i.ID, func(x *store.Idea) error { x.Status, x.Error = next, ""; return nil })
			return err
		}
		_, err := s.st.EditIdea(i.ID, func(x *store.Idea) error {
			if x.Status != store.IdeaFailed {
				return errors.New("only a failed idea can be retried")
			}
			x.Status, x.Error, x.SessionID, x.ResetAt = store.IdeaQueued, "", "", time.Now().Unix()
			return nil
		})
		s.kickIdeas()
		return err
	}
	return errors.New("unknown action")
}
