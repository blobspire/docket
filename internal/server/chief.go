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

// The chief of staff is deterministic: it orders the queue, pushes only what
// an agent marked "act now", expires moot items and watches machines. It
// never answers anything on the human's behalf.

// pushGrace gives someone at the terminal (or an agent that recovers on its
// own, like a watchdog alert that clears) a moment before the phone buzzes.
const pushGrace = 90 * time.Second

// Machines that stay unreachable this long become a health item.
const unreachableAfter = 10 * time.Minute

var urgencyRank = map[string]int{"now": 0, "soon": 1, "normal": 2}

// Ranked is an item with its session, for the queue view.
type Ranked struct {
	store.Item
	Session store.Session
}

// Paused reports whether the filing agent is waiting on this item.
func (r Ranked) Paused() bool {
	return r.Blocking && (r.Session.State == "idle" || r.Session.State == "waiting")
}

// Stale reports an item nobody has touched in a week.
func (r Ranked) Stale() bool {
	return time.Since(time.Unix(r.UpdatedAt, 0)) > 7*24*time.Hour
}

// Queue returns the active items (open, answered, snoozed), with sessions.
// Rule proposals live on the Brain page, not here.
func (s *Server) Queue() (active, snoozed []Ranked, err error) {
	items, err := s.st.Items("open", "answered", "snoozed")
	if err != nil {
		return
	}
	for _, it := range items {
		if it.Kind == "rule" {
			continue
		}
		sess, _, gerr := s.st.GetSession(it.SessionID)
		if gerr != nil {
			s.log.Printf("queue: session %s: %v", short(it.SessionID), gerr)
		}
		it.Lead = s.leadOf(it, sess)
		r := Ranked{Item: it, Session: sess}
		if it.Status == "snoozed" {
			snoozed = append(snoozed, r)
		} else {
			active = append(active, r)
		}
	}
	return
}

// SortItems orders items: "priority" (urgency, then deadline, then oldest
// first), "deadline" (soonest deadline first, then urgency) or "newest".
func SortItems(rs []Ranked, by string) {
	due := func(r Ranked) string {
		if r.Due == "" {
			return "9999"
		}
		return r.Due
	}
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		switch by {
		case "newest":
			return a.CreatedAt > b.CreatedAt
		case "deadline":
			if due(a) != due(b) {
				return due(a) < due(b)
			}
			if urgencyRank[a.Urgency] != urgencyRank[b.Urgency] {
				return urgencyRank[a.Urgency] < urgencyRank[b.Urgency]
			}
			return a.CreatedAt < b.CreatedAt
		}
		if urgencyRank[a.Urgency] != urgencyRank[b.Urgency] {
			return urgencyRank[a.Urgency] < urgencyRank[b.Urgency]
		}
		if a.Paused() != b.Paused() {
			return a.Paused()
		}
		if due(a) != due(b) {
			return due(a) < due(b)
		}
		return a.CreatedAt < b.CreatedAt
	})
}

// pushWorthy decides whether an open item deserves a phone push now: only
// what an agent (or a login failure) marked "act now", once it has stayed
// open through the grace period.
func pushWorthy(it store.Item, now time.Time) bool {
	if it.Status != "open" || it.PushedAt != 0 || it.Urgency != "now" || it.Kind == "rule" {
		return false
	}
	since := it.UrgentSince
	if since == 0 {
		since = it.CreatedAt
	}
	return now.Sub(time.Unix(since, 0)) >= pushGrace
}

func (s *Server) chiefTick(ctx context.Context) {
	if n, _ := s.st.Wake(); n > 0 {
		s.st.Bump()
	}
	if n, _ := s.st.Expire(s.cfg.Now().Unix()); n > 0 {
		s.st.Bump()
	}
	s.watchMachines()
	s.watchHealth(ctx)
	s.pushes(ctx)
	s.syncRules(ctx)
	s.rulesTick(ctx)
}

// pushes sends one phone notification for the act-now items that came due,
// whatever the hour: "act now" means now.
func (s *Server) pushes(ctx context.Context) {
	items, err := s.st.Items("open")
	if err != nil {
		return
	}
	now := s.cfg.Now()
	var due []store.Item
	for _, it := range items {
		if pushWorthy(it, now) {
			due = append(due, it)
		}
	}
	if len(due) == 0 {
		return
	}
	var title, body, click string
	if len(due) == 1 {
		it := due[0]
		title = it.Title
		body = pushBody(it)
		click = s.itemURL(it.ID)
	} else {
		title = fmt.Sprintf("%d things need you now", len(due))
		var lines []string
		for i, it := range due {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("…and %d more", len(due)-5))
				break
			}
			lines = append(lines, "• "+it.Title+" ("+firstNonEmptyStr(it.Project, store.Unsorted)+")")
		}
		body = strings.Join(lines, "\n")
		click = strings.TrimRight(s.cfg.PublicURL, "/") + "/?view=now"
	}
	if err := s.push(ctx, title, body, click, prioUrgent, "rotating_light"); err != nil {
		s.log.Printf("push: %v", err)
		return
	}
	ids := make([]string, len(due))
	for i, it := range due {
		ids[i] = it.ID
	}
	s.st.MarkPushed(ids)
}

// pushBody is "project · type", then the first line of the details.
func pushBody(it store.Item) string {
	b := firstNonEmptyStr(it.Project, store.Unsorted) + " · " + kindName(it.Kind)
	for _, line := range strings.Split(it.Body, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(line, "#*->` ")); line != "" {
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			return b + "\n" + line
		}
	}
	return b
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
			s.watch(store.Item{
				ID: ulid.ItemID(), Ongoing: true, Kind: "health", Blocking: true, Source: ingest.SrcWatchdog, Machine: m.Name, DedupeKey: key,
				Project: s.cfg.SystemProject, Urgency: "soon",
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

// watch files or refreshes one of docket's own health items, unless the
// human marked that check "not worth my time" in the last week.
func (s *Server) watch(it store.Item) {
	if it.DedupeKey != "" {
		if _, noised := s.st.RecentlyNoised(it.DedupeKey, time.Now().Add(-7*24*time.Hour).Unix()); noised {
			return
		}
	}
	if _, _, err := s.st.UpsertItem(it); err != nil {
		s.log.Printf("watch %s: %v", it.DedupeKey, err)
	}
}
