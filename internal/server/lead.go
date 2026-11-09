package server

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// Leads are event-driven: they sleep until an answer arrives (delivery wakes
// or resumes them), a job they registered ends or stalls, or one of their
// schedules fires. Schedules live in this always-on service and remember
// their last run, so they survive reboots.

// leadSession is the lead's most recent session, preferring a live one.
func (s *Server) leadSession(l Lead) (store.Session, bool) {
	all, _ := s.st.Sessions(true, 1000)
	var best store.Session
	found := false
	for _, x := range all {
		if x.Machine != l.Machine || !l.owns(x) {
			continue
		}
		live := x.State != "ended"
		if !found || (live && best.State == "ended") || (live == (best.State != "ended") && x.LastEventAt > best.LastEventAt) {
			best, found = x, true
		}
	}
	return best, found
}

// Wake gets text to a lead now: into its live session, by resuming its last
// session, or by starting it fresh.
func (s *Server) Wake(ctx context.Context, l Lead, reason string) error {
	if !l.On() {
		return fmt.Errorf("lead %s is disabled", l.Name)
	}
	m, ok := s.cfg.Machine(l.Machine)
	if !ok {
		return fmt.Errorf("lead %s: unknown machine %s", l.Name, l.Machine)
	}
	if sess, ok := s.leadSession(l); ok {
		nonce := randomNonce()
		d := store.Delivery{ID: ulid.New(time.Now()), Machine: l.Machine, SessionID: sess.ID, Nonce: nonce,
			Text: fmt.Sprintf("[docket #%s] %s", nonce, reason)}
		if err := s.st.AddDelivery(d); err != nil {
			return err
		}
		s.kickDeliver()
		return nil
	}
	if m.Mode == "push" {
		return fmt.Errorf("lead %s has no session yet and %s can't be reached; start it there once", l.Name, l.Machine)
	}
	args := []string{"session", "start", "--name", l.Session, "--cwd", l.Dir, "--lead", l.Name}
	claudeArgs := l.ClaudeArgs
	if len(claudeArgs) == 0 {
		claudeArgs = []string{"--permission-mode", "auto"}
	}
	args = append(args, "--")
	args = append(args, claudeArgs...)
	_, err := s.remote(ctx, m, strings.NewReader(reason), args...)
	return err
}

func (s *Server) leadTick(ctx context.Context) {
	now := s.cfg.Now()
	for _, l := range s.cfg.Leads {
		if !l.On() || len(l.Schedule) == 0 {
			continue
		}
		key := "lead:" + l.Name + ":last_wake"
		var last time.Time
		if v, err := strconv.ParseInt(s.st.Get(key), 10, 64); err == nil && v > 0 {
			last = time.Unix(v, 0).In(now.Location())
		}
		for _, spec := range l.Schedule {
			sc, err := parseSchedule(spec)
			if err != nil || !sc.due(last, now) {
				continue
			}
			s.st.Set(key, strconv.FormatInt(now.Unix(), 10)) // record first: never double-wake
			if err := s.Wake(ctx, l, l.WakePrompt); err != nil {
				s.log.Printf("lead %s: scheduled wake: %v", l.Name, err)
			}
			break
		}
	}
	s.watchJobs(ctx)
}

// watchJobs flags jobs that stopped beating and tells leads about jobs that
// ended or stalled, when they asked to be woken for that.
func (s *Server) watchJobs(ctx context.Context) {
	jobs, err := s.st.Jobs(500)
	if err != nil {
		return
	}
	now := time.Now()
	for _, j := range jobs {
		l, hasLead := s.cfg.Lead(j.Lead)
		key := "job:" + j.Scope + ":" + j.Name
		switch {
		case j.Status == "running" && j.Every > 0 && now.Sub(time.Unix(j.LastBeatAt, 0)) > 2*time.Duration(j.Every)*time.Second:
			s.st.SetJobState(j.Scope, j.Name, "stale", "stale")
			s.st.UpsertItem(store.Item{ID: ulid.ItemID(), Kind: "health", Blocking: true, Source: ingest.SrcWatchdog,
				Machine: j.Machine, SessionID: j.SessionID, Lead: j.Lead, DedupeKey: key,
				Title: fmt.Sprintf("Job %q looks stalled (no heartbeat for %s)", j.Name, age(now, j.LastBeatAt)),
				Body:  "It was expected to beat every " + (time.Duration(j.Every) * time.Second).String() + ". Last note: " + j.Note})
			s.st.Bump()
			if hasLead && l.WakesOn("job_stale") {
				s.Wake(ctx, l, fmt.Sprintf("Job %q has not sent a heartbeat for %s (expected every %s). Check whether it is still running and fix or report it.", j.Name, age(now, j.LastBeatAt), time.Duration(j.Every)*time.Second))
			}
		case (j.Status == "done" || j.Status == "failed") && j.Notified == "":
			s.st.SetJobState(j.Scope, j.Name, j.Status, "ended")
			s.resolveKey(key, "the job ended")
			if hasLead && l.WakesOn("job_done") {
				s.Wake(ctx, l, fmt.Sprintf("Job %q finished: %s.%s", j.Name, j.Status, comment(j.Note)))
			}
		case j.Status == "running" && j.Notified == "stale":
			s.st.SetJobState(j.Scope, j.Name, "running", "")
			s.resolveKey(key, "the job is beating again")
		}
	}
}

// leadPriority is used when ranking a lead's items.
func (s *Server) leadPriority(name string) int {
	if l, ok := s.cfg.Lead(name); ok {
		return l.Priority
	}
	return 0
}

// owns reports whether a session belongs to this lead: it was started as the
// lead, its name is (or starts with) the lead's session name, or, with
// match_dir, it works in the lead's folder.
func (l Lead) owns(x store.Session) bool {
	if x.Lead != "" {
		return x.Lead == l.Name
	}
	if x.Name != "" && (x.Name == l.Session || strings.HasPrefix(x.Name, l.Session+"-")) {
		return true
	}
	if l.MatchDir && x.Cwd != "" {
		dir := strings.TrimRight(strings.TrimPrefix(l.Dir, "~"), "/")
		return dir != "" && (strings.HasSuffix(strings.TrimRight(x.Cwd, "/"), dir) || strings.Contains(x.Cwd, dir+"/"))
	}
	return false
}
