package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// The chief of staff is deterministic: it ranks the queue, decides what is
// worth a phone push, watches machines, and writes the morning brief. It
// never answers anything on the human's behalf.

// Give someone at the terminal a moment before pushing to their phone.
const pushGrace = 2 * time.Minute

// Machines that stay unreachable this long become a health item.
const unreachableAfter = 10 * time.Minute

var kindBase = map[string]float64{
	"health": 60, "blocker": 70, "approval": 60, "ask": 50, "human_task": 45, "review": 40, "issue": 25, "fyi": 10,
}

// Score orders the queue: what blocks work first, then what has waited longest.
func Score(it store.Item, sess store.Session, today string, leadPriority int) float64 {
	sc := kindBase[it.Kind] + 10*float64(leadPriority)
	if it.Kind == "health" && it.Blocking {
		sc += 30
	}
	if it.Blocking {
		sc += 20
	}
	if sess.State == "waiting" || (sess.State == "idle" && it.Kind != "fyi") {
		sc += 15
	}
	if it.Due != "" {
		switch {
		case it.Due < today:
			sc += 40
		case it.Due == today:
			sc += 30
		}
	}
	age := time.Since(time.Unix(it.CreatedAt, 0)).Hours()
	if age > 72 {
		age = 72
	}
	return sc + age/2
}

// Ranked is an item with its session and score, for the queue view.
type Ranked struct {
	store.Item
	Session store.Session
	Score   float64
}

// Queue returns open items ranked, plus the "maybe" pile.
func (s *Server) Queue() (now, maybe, snoozed []Ranked, err error) {
	items, err := s.st.Items("open", "answered", "maybe", "snoozed")
	if err != nil {
		return
	}
	today := s.cfg.Now().Format("2006-01-02")
	for _, it := range items {
		sess, _, gerr := s.st.GetSession(it.SessionID)
		if gerr != nil {
			s.log.Printf("queue: session %s: %v", short(it.SessionID), gerr)
		}
		it.Lead = s.leadOf(it, sess)
		r := Ranked{Item: it, Session: sess, Score: Score(it, sess, today, s.leadPriority(it.Lead))}
		switch it.Status {
		case "maybe":
			maybe = append(maybe, r)
		case "snoozed":
			snoozed = append(snoozed, r)
		default:
			now = append(now, r)
		}
	}
	for _, l := range [][]Ranked{now, maybe, snoozed} {
		sort.SliceStable(l, func(i, j int) bool { return l[i].Score > l[j].Score })
	}
	return
}

// pushWorthy decides whether an open item deserves a phone push now.
// Pushes go out only when a lead is blocked, a system is down, or something
// is due today; everything else waits quietly in the queue.
func (s *Server) pushWorthy(it store.Item, sess store.Session, now time.Time) (bool, bool) {
	critical := it.Kind == "health" && it.Blocking
	if it.Status != "open" || it.PushedAt != 0 {
		return false, critical
	}
	if critical {
		return true, true
	}
	old := now.Sub(time.Unix(it.CreatedAt, 0)) >= pushGrace
	today := now.Format("2006-01-02")
	if it.Due != "" && it.Due <= today && now.Hour()*60+now.Minute() >= minutes(s.cfg.BriefAt) {
		return true, false
	}
	stalled := sess.State == "idle" || sess.State == "waiting" || sess.State == "ended" || it.SessionID == ""
	switch it.Kind {
	case "blocker":
		return old, false
	case "approval":
		return old && stalled, false
	case "ask", "human_task":
		return old && stalled && it.Blocking, false
	}
	return false, false
}

func (s *Server) chiefTick(ctx context.Context) {
	if n, _ := s.st.Wake(); n > 0 {
		s.st.Bump()
	}
	s.watchMachines()
	s.watchHealth(ctx)
	s.pushes(ctx)
	s.brief(ctx)
	s.syncRules(ctx)
	s.preReviewTick(ctx)
	s.rulesTick(ctx)
}

func (s *Server) pushes(ctx context.Context) {
	items, err := s.st.Items("open")
	if err != nil {
		return
	}
	now := s.cfg.Now()
	quiet := inWindow(now, s.cfg.Ntfy.QuietStart, s.cfg.Ntfy.QuietEnd)
	var due []store.Item
	urgent := false
	for _, it := range items {
		sess, _, _ := s.st.GetSession(it.SessionID)
		ok, critical := s.pushWorthy(it, sess, now)
		if !ok || (quiet && !critical) {
			continue
		}
		due = append(due, it)
		urgent = urgent || critical
	}
	if len(due) == 0 {
		return
	}
	prio := prioHigh
	if urgent {
		prio = prioUrgent
	}
	var title, body, click string
	if len(due) == 1 {
		it := due[0]
		title = pushTitle(it)
		body = it.Title + where(it)
		click = s.itemURL(it.ID)
	} else {
		title = fmt.Sprintf("%d things need you", len(due))
		var lines []string
		for i, it := range due {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("…and %d more", len(due)-5))
				break
			}
			lines = append(lines, "• "+it.Title+where(it))
		}
		body = strings.Join(lines, "\n")
		click = strings.TrimRight(s.cfg.PublicURL, "/") + "/"
	}
	if err := s.push(ctx, title, body, click, prio, "docket"); err != nil {
		s.log.Printf("push: %v", err)
		return
	}
	ids := make([]string, len(due))
	for i, it := range due {
		ids[i] = it.ID
	}
	s.st.MarkPushed(ids)
}

func pushTitle(it store.Item) string {
	switch it.Kind {
	case "health":
		return "System problem"
	case "blocker":
		return "An agent is waiting on you"
	case "approval":
		return "An agent needs your approval"
	case "human_task":
		return "Only you can do this"
	}
	if it.Due != "" {
		return "Due today"
	}
	return "An agent needs your decision"
}

func where(it store.Item) string {
	p := project(it.Cwd)
	switch {
	case it.Lead != "":
		return " (" + it.Lead + ")"
	case p != "" && it.Machine != "":
		return " (" + p + " on " + it.Machine + ")"
	case it.Machine != "":
		return " (" + it.Machine + ")"
	}
	return ""
}

func project(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if i := strings.LastIndexByte(cwd, '/'); i >= 0 {
		return cwd[i+1:]
	}
	return cwd
}

// watchMachines opens a health item for a pull machine that has been
// unreachable for a while, and closes it once the machine is back. Laptops
// (push machines) being away is normal and never alerts.
func (s *Server) watchMachines() {
	ms, err := s.st.Machines()
	if err != nil {
		return
	}
	for _, m := range ms {
		cm, ok := s.cfg.Machine(m.Name)
		if !ok || cm.Mode == "push" || cm.Sleepy {
			continue
		}
		key := "watchdog:machine:" + m.Name
		if m.FailingSince > 0 && time.Since(time.Unix(m.FailingSince, 0)) > unreachableAfter {
			s.st.UpsertItem(store.Item{
				ID: ulid.ItemID(), Kind: "health", Blocking: true, Source: ingest.SrcWatchdog, Machine: m.Name, DedupeKey: key,
				Title: fmt.Sprintf("%s has been unreachable since %s", m.Name, time.Unix(m.FailingSince, 0).In(s.cfg.loc).Format("Mon 15:04")),
				Body:  "docket can't collect from it or deliver answers to it. Last error: " + m.LastError,
			})
			s.st.Bump()
		} else if m.FailingSince == 0 {
			if n := s.resolveKey(key, m.Name+" is reachable again"); n > 0 {
				s.st.Bump()
			}
		}
	}
}

func (s *Server) resolveKey(key, why string) int {
	items, _ := s.st.Items("open", "snoozed")
	n := 0
	for _, it := range items {
		if it.DedupeKey == key {
			s.st.SetStatus(it.ID, "done", why, "")
			n++
		}
	}
	return n
}

// Brief is the morning summary shown at the top of the queue.
type Brief struct {
	Date     string
	Text     string
	Open     int
	Blocking int
}

// brief writes the morning brief once a day at brief_at.
func (s *Server) brief(ctx context.Context) {
	now := s.cfg.Now()
	today := now.Format("2006-01-02")
	if s.st.Get("brief_date") == today || now.Hour()*60+now.Minute() < minutes(s.cfg.BriefAt) {
		return
	}
	text, open, blocking := s.composeBrief(now)
	s.st.Set("brief_text", text)
	s.st.Set("brief_date", today)
	s.st.Bump()
	if s.cfg.BriefPushOn() {
		summary := fmt.Sprintf("%d open, %d blocking", open, blocking)
		if open == 0 {
			summary = "Nothing needs you right now."
		}
		if err := s.push(ctx, "Morning brief", summary, strings.TrimRight(s.cfg.PublicURL, "/")+"/", prioLow, "sunrise"); err != nil {
			s.log.Printf("brief push: %v", err)
		}
	}
}

func (s *Server) composeBrief(now time.Time) (string, int, int) {
	nowItems, _, _, _ := s.Queue()
	since := now.Add(-24 * time.Hour).Unix()
	closed, _ := s.st.RecentlyClosed(since, 200)
	var b strings.Builder
	blocking := 0
	for _, r := range nowItems {
		if r.Blocking {
			blocking++
		}
	}
	fmt.Fprintf(&b, "%s. %d open (%d blocking); %d resolved in the last 24 hours.\n", now.Format("Monday, January 2"), len(nowItems), blocking, len(closed))
	if len(nowItems) > 0 {
		b.WriteString("\nTop of the queue:\n")
		for i, r := range nowItems {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "- %s%s, waiting %s\n", r.Title, where(r.Item), age(now, r.CreatedAt))
		}
	}
	ms, _ := s.st.Machines()
	var down []string
	for _, m := range ms {
		if m.FailingSince > 0 {
			down = append(down, m.Name)
		}
	}
	if len(down) > 0 {
		fmt.Fprintf(&b, "\nUnreachable: %s.\n", strings.Join(down, ", "))
	}
	sessions, _ := s.st.Sessions(true, 500)
	nudges, hits := 0, 0
	for _, x := range sessions {
		nudges += x.Nudges
		hits += x.NudgeHits
	}
	if nudges > 0 {
		fmt.Fprintf(&b, "\nSelf-check: %d nudges so far, %d led to a filed item.\n", nudges, hits)
	}
	return b.String(), len(nowItems), blocking
}

func age(now time.Time, unix int64) string {
	d := now.Sub(time.Unix(unix, 0))
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
