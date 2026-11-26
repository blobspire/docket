package server

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// The timeline page: a project's history as the human saw it, newest first,
// grouped by day. Results is one of its filters, so a result from months
// ago is two taps away.

// timelineLimit is how many entries one page of the timeline shows.
const timelineLimit = 150

type timelineView struct {
	Project  string // "" for every project
	Desc     string
	Filter   string // "" for everything
	Filters  []tlChip
	Projects []tlChip
	Days     []tlDay
	Older    string // the next page's link, when there is one
	Empty    string // what to say when there's nothing
}

type tlChip struct {
	Label, URL string
	On         bool
}

type tlDay struct {
	Label   string
	Entries []tlEntry
}

// tlEntry is one entry as the page shows it.
type tlEntry struct {
	store.TimelineEntry
	View        *itemView // the item, as a row would show it (not for shipped ideas)
	Label       string    // the kind, in a word
	Class       string    // its colour
	What        string    // a decision: what the human did
	Summary     string    // a result's or issue's first line
	OnDocket    bool      // a result, action item or problem still waiting on the human
	Outcome     string    // how an action item or problem ended, or what the human answered
	OutIcon     string    // its icon
	ClosedAt    int64     // when it closed (0: still open)
	ShowProject bool
	Note        template.HTML // a shipped idea's note (with its pull request link)
}

var tlNames = map[string]struct{ label, class string }{
	store.TLResult:   {"Result", "review"},
	store.TLAction:   {"", ""}, // the item's own kind: Decide or Do
	store.TLProgress: {"Progress", "progress"},
	store.TLIssue:    {"Problem", "problem"},
	store.TLDecision: {"Decision", "decide"},
	store.TLShipped:  {"Shipped", "do"},
}

// tlFilterNames are the filter chips' labels.
var tlFilterNames = map[string]string{
	store.TLResult: "Results", store.TLAction: "Action items", store.TLProgress: "Progress", store.TLDecision: "Decisions",
	store.TLIssue: "Problems", store.TLShipped: "Shipped",
}

func (s *Server) pageTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := timelineView{Project: store.Slug(q.Get("p")), Filter: q.Get("f")}
	if _, ok := tlFilterNames[v.Filter]; !ok {
		v.Filter = ""
	}
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	entries, more, err := s.st.Timeline(v.Project, v.Filter, before, timelineLimit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	link := func(project, filter string, before int64) string {
		u := url.Values{}
		if project != "" {
			u.Set("p", project)
		}
		if filter != "" {
			u.Set("f", filter)
		}
		if before > 0 {
			u.Set("before", strconv.FormatInt(before, 10))
		}
		if len(u) == 0 {
			return "/timeline"
		}
		return "/timeline?" + u.Encode()
	}
	v.Filters = append(v.Filters, tlChip{Label: "All", URL: link(v.Project, "", 0), On: v.Filter == ""})
	for _, f := range store.TimelineFilters {
		v.Filters = append(v.Filters, tlChip{Label: tlFilterNames[f], URL: link(v.Project, f, 0), On: v.Filter == f})
	}
	v.Projects = append(v.Projects, tlChip{Label: "All projects", URL: link("", v.Filter, 0), On: v.Project == ""})
	if ps, err := s.st.Projects(false); err == nil {
		for _, p := range ps {
			if p.Name == store.Unsorted || p.Name == "email" {
				continue
			}
			v.Projects = append(v.Projects, tlChip{Label: p.Name, URL: link(p.Name, v.Filter, 0), On: v.Project == p.Name})
			if p.Name == v.Project {
				v.Desc = p.Description
			}
		}
	}
	v.Days = s.tlDays(entries, v.Project == "")
	if more {
		v.Older = link(v.Project, v.Filter, entries[len(entries)-1].At)
	}
	switch {
	case before > 0:
		v.Empty = "Nothing older."
	case v.Filter == store.TLResult:
		v.Empty = "No results yet. The results agents send you with docket review are kept here, after you mark them done too."
	case v.Filter == store.TLAction:
		v.Empty = "No action items yet. What agents ask of you shows up here, and stays after it's done."
	case v.Filter == store.TLProgress:
		v.Empty = "No progress posted yet. Agents post a line here when they finish a major stage."
	default:
		v.Empty = "Nothing here yet. Results, action items, progress, problems, your decisions and what shipped show up here as they happen."
	}
	title := "Timeline"
	if v.Project != "" {
		title = v.Project + " · Timeline"
	}
	s.render(w, "timeline.html", page{Title: title, Nav: "timeline", Timeline: &v})
}

// tlDays turns entries into days, with each item shown as its row would be.
func (s *Server) tlDays(entries []store.TimelineEntry, showProject bool) []tlDay {
	var items []store.Item
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Item != nil && !seen[e.Item.ID] {
			seen[e.Item.ID] = true
			items = append(items, *e.Item)
		}
	}
	views := map[string]*itemView{}
	for _, v := range s.rows(ranked(items), "list", showProject, nil) {
		v := v
		views[v.ID] = &v
	}
	now := time.Now()
	var days []tlDay
	for _, e := range entries {
		n := tlNames[e.Kind]
		te := tlEntry{TimelineEntry: e, Label: n.label, Class: n.class, ShowProject: showProject}
		if e.Item != nil {
			te.View = views[e.Item.ID]
		}
		switch e.Kind {
		case store.TLResult, store.TLAction, store.TLIssue:
			te.Summary = firstLine(e.Item.Body)
			te.OnDocket = e.Item.Status == "open" || e.Item.Status == "snoozed"
			if e.Kind == store.TLAction {
				te.Label, te.Class = kindName(e.Item.Kind), kindClass(e.Item.Kind)
			}
			if e.Kind != store.TLResult && te.View != nil {
				te.outcome(*te.View)
			}
		case store.TLDecision:
			te.What = decisionWords(e.Decision, e.Item)
		case store.TLShipped:
			te.Note = renderMarkdown(e.Idea.Note)
		}
		label := dayLabel(now, e.At)
		if len(days) == 0 || days[len(days)-1].Label != label {
			days = append(days, tlDay{Label: label})
		}
		days[len(days)-1].Entries = append(days[len(days)-1].Entries, te)
	}
	return days
}

// outcome says how an action item or problem ended: closed (and when), or
// answered and waiting on its agent. One still open says nothing here; the
// "on your docket" mark says it.
func (te *tlEntry) outcome(v itemView) {
	switch {
	case v.Closed():
		te.Outcome, te.ClosedAt = v.ClosedHow(), closedAt(v.Item)
		te.OutIcon = "check"
		if v.Status == "expired" {
			te.OutIcon = "clock"
		}
	case v.Waiting():
		te.Outcome, te.OutIcon = v.ClosedHow()+", waiting on the agent", "clock"
	}
}

// decisionWords says what the human did, in a few words.
func decisionWords(d *store.Decision, it *store.Item) string {
	switch d.Action {
	case "approve":
		return "You approved"
	case "decline":
		return "You declined"
	case "choose":
		if c := store.AnswerChoice(d.Answer, it.Options); c != "" {
			return "You chose “" + c + "”"
		}
		return "You chose"
	}
	return "You replied"
}

// dayLabel names the day of a unix time: Today, Yesterday, or the date.
func dayLabel(now time.Time, unix int64) string {
	t := time.Unix(unix, 0).In(now.Location())
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location()); {
	case day.Equal(today):
		return "Today"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case t.Year() == now.Year():
		return t.Format("Mon, Jan 2")
	}
	return t.Format("Mon, Jan 2, 2006")
}
