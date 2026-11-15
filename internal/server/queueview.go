package server

import (
	"net/url"
	"sort"
	"strings"

	"github.com/blobspire/docket/internal/store"
)

// queueView is everything the dashboard page shows.
type queueView struct {
	Sort    string // priority | deadline | newest
	Mode    string // all | now | project | place
	Project string
	Place   string
	Heading string
	Sub     string // the project's description, in a project view

	NowCount, AllCount int
	SideProjects       []sideEntry
	SidePlaces         []sideEntry

	Now      []Ranked // the act-now band
	Sections []section
	Snoozed  []Ranked

	Grouped     bool           // project view: rows sit under component headings
	ProjectInfo *store.Project // project view: for the edit form
	Projects    []string       // every project name, for "move to"
	Empty       bool
}

type sideEntry struct {
	Name, Desc string
	Count      int
	HasNow     bool
	On         bool
	URL        string
}

type section struct {
	Title, Sub, Anchor, URL string
	Count                   int
	Groups                  []group
}

type group struct {
	Title string
	Items []Ranked
}

// itemView is one row: the item, whether it starts expanded, and the
// project names for its "move to" control.
type itemView struct {
	Ranked
	Expanded      bool
	Projects      []string
	Back          string
	HideComponent bool // its group heading already says it
	InBand        bool // shown in the act-now band, which already says "act now"
}

// BandRow is a row in the act-now band.
func (q *queueView) BandRow(r Ranked) itemView {
	v := q.Row(r)
	v.InBand = true
	return v
}

func (q *queueView) Row(r Ranked) itemView {
	return itemView{Ranked: r, Projects: q.Projects, Back: q.Link(q.Project, q.Place, q.Mode == "now"), HideComponent: q.Grouped && r.Urgency != "now"}
}

// Open is a row shown expanded on its own page.
func (q *queueView) Open(r *Ranked) itemView {
	return itemView{Ranked: *r, Expanded: true, Projects: q.Projects, Back: "/item/" + r.ID}
}

// Link builds a dashboard URL that keeps the current sort.
func (q *queueView) Link(project, place string, now bool) string {
	v := url.Values{}
	switch {
	case now:
		v.Set("view", "now")
	case project != "":
		v.Set("p", project)
	case place != "":
		v.Set("place", place)
	}
	if q.Sort != "" && q.Sort != "priority" {
		v.Set("sort", q.Sort)
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// SortLink is the current view with another sort order.
func (q *queueView) SortLink(by string) string {
	saved := q.Sort
	q.Sort = by
	defer func() { q.Sort = saved }()
	return q.Link(q.Project, q.Place, q.Mode == "now")
}

func (s *Server) buildQueue(query url.Values) (*queueView, error) {
	active, snoozed, err := s.Queue()
	if err != nil {
		return nil, err
	}
	q := &queueView{Sort: query.Get("sort"), Project: store.Slug(query.Get("p")), Place: store.Slug(query.Get("place"))}
	switch q.Sort {
	case "deadline", "newest":
	default:
		q.Sort = "priority"
	}
	switch {
	case query.Get("view") == "now":
		q.Mode = "now"
	case q.Project != "":
		q.Mode = "project"
	case q.Place != "":
		q.Mode = "place"
	default:
		q.Mode = "all"
	}

	// Sidebar: every known project (muted when empty), then places in use.
	// An archived project drops out of the sidebar once it has nothing open.
	projects, _ := s.st.Projects(true)
	desc := map[string]string{}
	count, hasNow, placeCount := map[string]int{}, map[string]bool{}, map[string]int{}
	for _, p := range projects {
		desc[p.Name] = p.Description
		if !p.Archived {
			count[p.Name] += 0
		}
	}
	for _, r := range active {
		p := projectName(r.Item)
		count[p]++
		q.AllCount++
		if r.Urgency == "now" {
			hasNow[p] = true
			q.NowCount++
		}
		if r.Place != "" {
			placeCount[r.Place]++
		}
	}
	var names []string
	for p := range count {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool { return projectLess(names[i], names[j]) })
	q.Projects = names
	for _, p := range names {
		q.SideProjects = append(q.SideProjects, sideEntry{Name: p, Desc: desc[p], Count: count[p], HasNow: hasNow[p], On: q.Mode == "project" && q.Project == p, URL: q.Link(p, "", false)})
	}
	var places []string
	for p := range placeCount {
		places = append(places, p)
	}
	sort.Strings(places)
	for _, p := range places {
		q.SidePlaces = append(q.SidePlaces, sideEntry{Name: p, Count: placeCount[p], On: q.Mode == "place" && q.Place == p, URL: q.Link("", p, false)})
	}

	// What this view shows.
	var shown []Ranked
	for _, r := range active {
		switch q.Mode {
		case "now":
			if r.Urgency != "now" {
				continue
			}
		case "project":
			if projectName(r.Item) != q.Project {
				continue
			}
		case "place":
			if r.Place != q.Place {
				continue
			}
		}
		shown = append(shown, r)
	}
	SortItems(shown, q.Sort)
	var rest []Ranked
	for _, r := range shown {
		if r.Urgency == "now" {
			q.Now = append(q.Now, r)
		} else {
			rest = append(rest, r)
		}
	}
	for _, r := range snoozed {
		if (q.Mode == "project" && projectName(r.Item) != q.Project) || (q.Mode == "place" && r.Place != q.Place) || q.Mode == "now" {
			continue
		}
		q.Snoozed = append(q.Snoozed, r)
	}
	SortItems(q.Snoozed, q.Sort)

	switch q.Mode {
	case "all":
		q.Heading = "All projects"
		q.Sections = byProject(rest, desc, q)
	case "now":
		q.Heading = "Act now"
		q.Sub = "Something is down or blocked until you act."
	case "project":
		q.Heading = q.Project
		q.Sub = desc[q.Project]
		if p, ok := s.st.GetProject(q.Project); ok {
			q.ProjectInfo = &p
		}
		if len(rest) > 0 {
			gs := byComponent(rest)
			q.Grouped = len(gs) > 1
			q.Sections = []section{{Count: len(rest), Groups: gs}}
		}
	case "place":
		q.Heading = "At " + q.Place
		q.Sub = "Everything that needs you there, so one trip covers it."
		q.Sections = byProject(rest, desc, q)
	}
	q.Empty = len(q.Now) == 0 && len(q.Sections) == 0
	return q, nil
}

func projectName(it store.Item) string {
	if it.Project == "" {
		return store.Unsorted
	}
	return it.Project
}

// projectLess sorts projects A–Z with "unsorted" last.
func projectLess(a, b string) bool {
	if (a == store.Unsorted) != (b == store.Unsorted) {
		return b == store.Unsorted
	}
	return a < b
}

func byProject(rs []Ranked, desc map[string]string, q *queueView) []section {
	idx := map[string]int{}
	var out []section
	for _, r := range rs {
		p := projectName(r.Item)
		i, ok := idx[p]
		if !ok {
			i = len(out)
			idx[p] = i
			out = append(out, section{Title: p, Sub: desc[p], Anchor: "p-" + p, URL: q.Link(p, "", false), Groups: []group{{}}})
		}
		out[i].Count++
		out[i].Groups[0].Items = append(out[i].Groups[0].Items, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return projectLess(out[i].Title, out[j].Title) })
	return out
}

// byComponent groups a project's items by component, items without one
// first. Headings only help when some component has several items; otherwise
// it's one list and each row shows its component.
func byComponent(rs []Ranked) []group {
	idx := map[string]int{}
	var out []group
	for _, r := range rs {
		c := strings.TrimSpace(r.Component)
		i, ok := idx[strings.ToLower(c)]
		if !ok {
			i = len(out)
			idx[strings.ToLower(c)] = i
			out = append(out, group{Title: c})
		}
		out[i].Items = append(out[i].Items, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Title == "") != (out[j].Title == "") {
			return out[i].Title == ""
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	many := false
	for _, g := range out {
		many = many || len(g.Items) > 1
	}
	if len(out) == 1 || !many {
		return []group{{Items: rs}}
	}
	if out[0].Title == "" {
		out[0].Title = "General"
	}
	return out
}
