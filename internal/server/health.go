package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
	"github.com/blobspire/docket/internal/usage"
)

// Quota alerts go to the queue only (no push): the human sees them on their
// next look, and nothing is ever paused automatically.
const (
	quotaWarn5h  = 90.0
	quotaWarn7d  = 85.0
	quotaClearAt = 70.0
)

// How long a session may sit on a dialog before it becomes an item.
const waitingAfter = 5 * time.Minute

// collectHealth gathers quota on every pull and token usage every 15 minutes.
func (s *Server) collectHealth(ctx context.Context, m Machine) {
	if out, err := s.remote(ctx, m, nil, "quota"); err == nil {
		s.ApplyQuota(m.Name, out)
	}
	key := "usage:" + m.Name + ":at"
	last, _ := strconv.ParseInt(s.st.Get(key), 10, 64)
	if time.Since(time.Unix(last, 0)) < 15*time.Minute {
		return
	}
	if out, err := s.remote(ctx, m, nil, "usage", "--json", "--since", "8d"); err == nil {
		s.ApplyUsage(m.Name, out)
		s.st.Set(key, strconv.FormatInt(time.Now().Unix(), 10))
	}
}

// ApplyQuota stores a machine's quota snapshot.
func (s *Server) ApplyQuota(machine string, data []byte) {
	var q usage.Quota
	if len(data) == 0 || string(data) == "null" || json.Unmarshal(data, &q) != nil {
		return
	}
	for win, w := range q.Windows {
		s.st.SetQuota(store.Quota{Machine: machine, Account: q.Account, Window: win, UsedPct: w.UsedPct, ResetsAt: w.ResetsAt, CapturedAt: q.CapturedAt})
	}
}

// ApplyUsage stores a machine's per-session token totals.
func (s *Server) ApplyUsage(machine string, data []byte) {
	var days []usage.Day
	if len(data) == 0 || json.Unmarshal(data, &days) != nil {
		return
	}
	for _, d := range days {
		s.st.SetUsage(store.Usage{Machine: machine, SessionID: d.SessionID, Day: d.Day, Cwd: d.Cwd, Input: d.Input, Output: d.Output, CacheWrite: d.CacheWrite, CacheRead: d.CacheRead})
	}
}

// watchHealth turns quota pressure, sessions stuck on a dialog, and always-on
// leads that stopped into items.
func (s *Server) watchHealth(ctx context.Context) {
	quotas, _ := s.st.Quotas()
	for _, q := range quotas {
		limit := quotaWarn7d
		label := "weekly"
		if q.Window == "five_hour" {
			limit, label = quotaWarn5h, "5-hour"
		}
		key := "quota:" + firstNonEmptyStr(q.Account, q.Machine) + ":" + q.Window
		if q.UsedPct >= limit && time.Since(time.Unix(q.CapturedAt, 0)) < 6*time.Hour {
			reset := ""
			if q.ResetsAt > 0 {
				reset = " It resets " + time.Unix(q.ResetsAt, 0).In(s.cfg.loc).Format("Mon 15:04") + "."
			}
			s.st.UpsertItem(store.Item{ID: ulid.ItemID(), Kind: "health", Source: ingest.SrcWatchdog, Machine: q.Machine, DedupeKey: key,
				Title: fmt.Sprintf("Claude %s limit at %.0f%% (%s)", label, q.UsedPct, firstNonEmptyStr(q.Account, q.Machine)),
				Body:  "Nothing is paused automatically. The Health page shows which leads are spending." + reset})
		} else if q.UsedPct < quotaClearAt {
			s.resolveKey(key, "usage dropped")
		}
	}

	all, _ := s.st.Sessions(false, 500)
	now := time.Now()
	for _, x := range all {
		key := "waiting:" + x.ID
		if x.State == "waiting" && x.WaitingSince > 0 && now.Sub(time.Unix(x.WaitingSince, 0)) > waitingAfter && !x.HasPlugin {
			s.st.UpsertItem(store.Item{ID: ulid.ItemID(), Kind: "blocker", Blocking: true, Source: ingest.SrcNotification,
				Machine: x.Machine, SessionID: x.ID, Cwd: x.Cwd, DedupeKey: key, CreatedAt: x.WaitingSince,
				Title: fmt.Sprintf("Session %s is waiting on you (%s)", firstNonEmptyStr(x.Name, short(x.ID)), firstNonEmptyStr(x.WaitingFor, "input")),
				Body:  "It has been waiting since " + time.Unix(x.WaitingSince, 0).In(s.cfg.loc).Format("Mon 15:04") + ". Open the session to answer."})
		} else if x.State != "waiting" {
			s.resolveKey(key, "the session moved on")
		}
	}

	for _, l := range s.cfg.Leads {
		if !l.On() || !l.AlwaysOn {
			continue
		}
		key := "lead-down:" + l.Name
		sess, ok := s.leadSession(l)
		if ok && sess.State != "ended" {
			s.resolveKey(key, l.Name+" is running again")
			continue
		}
		s.st.UpsertItem(store.Item{ID: ulid.ItemID(), Kind: "health", Blocking: true, Source: ingest.SrcWatchdog, Machine: l.Machine, Lead: l.Name, DedupeKey: key,
			Title: fmt.Sprintf("Lead %s isn't running", l.Name),
			Body:  "It is marked always_on. Wake it from the Leads page to start or resume it."})
	}
	s.st.Bump()
}

// Spend is one lead's (or project's) token use over a window.
type Spend struct {
	Name                                 string
	Output, InputWrite, CacheRead, Units int64
	Share                                float64
}

// LeadSpend attributes the last week's tokens to leads (or projects).
// Units weight tokens roughly by price: output 5, input and cache writes 1,
// cache reads 0.1.
func (s *Server) LeadSpend(days int) []Spend {
	since := time.Now().AddDate(0, 0, -days).UTC().Format("2006-01-02")
	rows, _ := s.st.UsageSince(since)
	by := map[string]*Spend{}
	var total int64
	for _, u := range rows {
		sess, _, _ := s.st.GetSession(u.SessionID)
		if sess.ID == "" {
			sess = store.Session{Machine: u.Machine, Cwd: u.Cwd}
		}
		if sess.Cwd == "" {
			sess.Cwd = u.Cwd
		}
		name := s.leadOf(store.Item{Machine: u.Machine, Cwd: u.Cwd}, sess)
		if name == "" {
			name = project(u.Cwd) + " (" + u.Machine + ")"
		}
		sp := by[name]
		if sp == nil {
			sp = &Spend{Name: name}
			by[name] = sp
		}
		sp.Output += u.Output
		sp.InputWrite += u.Input + u.CacheWrite
		sp.CacheRead += u.CacheRead
		units := 5*u.Output + u.Input + u.CacheWrite + u.CacheRead/10
		sp.Units += units
		total += units
	}
	var out []Spend
	for _, sp := range by {
		if total > 0 {
			sp.Share = 100 * float64(sp.Units) / float64(total)
		}
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Units > out[j].Units })
	return out
}

// short is the first 8 characters of an id (or all of a shorter one).
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
