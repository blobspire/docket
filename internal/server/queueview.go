package server

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// queueView is everything the docket page shows.
type queueView struct {
	Sort    string // priority | deadline | newest
	Mode    string // all | now | project | place
	Project string
	Place   string
	Heading string
	Desc    string // the project's description, in a project view

	Need, Waiting      int // in this view: open items, and ones waiting on agents
	NowCount, AllCount int // open items everywhere, for the sidebar
	SideProjects       []sideEntry
	SidePlaces         []sideEntry

	Now      []itemView // the act-now band
	Sections []section
	Snoozed  []itemView

	ProjectInfo *store.Project // project view: for the edit form
	Projects    []string       // every project name, for "move to"
}

type sideEntry struct {
	Name, Desc string
	Count      int
	HasNow     bool
	On         bool
	URL        string
}

// section is one project's part of the docket (or, in a project view, the
// whole list): its open items, then the ones waiting on agents, folded.
type section struct {
	Title, Desc, URL string // no Title: the view's own list
	Key              string // the view's own list: which project
	Rows             []itemView
	Waiting          []itemView
	Stuck            int // waiting items whose answer hasn't reached the agent
}

// WaitKey names a section's "Waiting on agents" fold, so it stays open or
// closed across refreshes.
func (s section) WaitKey() string {
	if s.Title == "" {
		return "view:" + s.Key
	}
	return s.Title
}

// itemView is one row (or the item sheet): the item and what its row shows.
type itemView struct {
	Ranked
	List        string // docket | stash | done | list (the other pages)
	ShowProject bool
	Images      []store.Evidence // fetched images, for thumbnails and the viewer
	Docs        int              // other attachments
	Delivery    store.Delivery   // its answer's latest delivery
	LastAction  string           // the human's latest logged action on it
	Projects    []string         // where it can be moved
	Person      string
}

// AnswerVerb and AnswerWhat split Answered for templates.
func (v itemView) AnswerVerb() string { a, _ := v.Answered(); return a }
func (v itemView) AnswerWhat() string { _, b := v.Answered(); return b }

// DeliveryText and Stuck split DeliveryNote for templates.
func (v itemView) DeliveryText() string { a, _ := v.DeliveryNote(); return a }
func (v itemView) Stuck() bool          { _, b := v.DeliveryNote(); return b }

// Waiting reports a docket item whose answer is with its agent.
func (v itemView) Waiting() bool {
	return v.Status == "answered" || v.Status == "delivered"
}

// Closed reports an item that's no longer on the docket's to-do list.
func (v itemView) Closed() bool { return closedStatus(v.Status) }

// Kept reports an item on the Stash, or closed: its sheet offers only to
// put it back or keep it.
func (v itemView) Kept() bool { return v.Stashed() || v.Closed() }

// Stashed reports an item on the Stash.
func (v itemView) Stashed() bool { return v.StashedAt >= store.StashCutoff() }

// Thumbs is the images a row shows (two at most).
func (v itemView) Thumbs() []store.Evidence {
	if len(v.Images) > 2 {
		return v.Images[:2]
	}
	return v.Images
}

// MoreImages is how many images the row's thumbnails leave out.
func (v itemView) MoreImages() int {
	if len(v.Images) > 2 {
		return len(v.Images) - 2
	}
	return 0
}

// Answered describes the human's answer in their own terms: "You chose" and
// the option, "You replied" and the words, "You approved".
func (v itemView) Answered() (verb, what string) {
	a := strings.TrimSpace(v.Answer)
	switch {
	case a == "":
		return "", ""
	case strings.HasPrefix(a, "Chose: "):
		for _, o := range v.Options {
			if strings.HasPrefix(a, "Chose: "+o+".") {
				return "You chose", o
			}
		}
		return "You chose", strings.TrimSuffix(strings.TrimPrefix(a, "Chose: "), ".")
	case strings.HasPrefix(a, "Approved."):
		return "You approved it", ""
	case strings.HasPrefix(a, "Declined: "):
		return "You declined it", ""
	}
	return "You replied", clip(a, 80)
}

// DeliveryNote says where a waiting item's answer is, and whether it's stuck.
func (v itemView) DeliveryNote() (note string, stuck bool) {
	if v.Status == "delivered" {
		if v.Session.State == "busy" {
			return "The agent is on it", false
		}
		return "Delivered, no reply yet", false
	}
	if e := strings.TrimSpace(v.Delivery.Error); e != "" && v.Delivery.Status == "pending" {
		return "Not delivered yet: " + clip(e, 90), true
	}
	// Sent, retried, and still never confirmed: it may never have arrived.
	if v.Delivery.Status == "sent" && v.Delivery.Attempts >= 4 && time.Since(time.Unix(v.Delivery.SentAt, 0)) > 15*time.Minute {
		return "Sent, but the agent never confirmed it", true
	}
	return "On its way to the agent", false
}

// ClosedHow says how a closed (or stashed) item got that way.
func (v itemView) ClosedHow() string {
	res := strings.TrimRight(strings.TrimSpace(v.Resolution), ".")
	if verb, what := v.Answered(); verb != "" && (v.Waiting() || v.LastAction == "answer" || v.LastAction == "choose" || v.LastAction == "approve" || v.LastAction == "decline") {
		if what != "" {
			verb += " “" + what + "”"
		}
		if v.Status == "done" && res != "" && res != "resolved by the agent" && res != "Stashed" {
			verb += "; the agent closed it: " + res
		}
		return verb
	}
	switch v.Status {
	case "done":
		switch {
		case v.LastAction == "stash":
			return "You stashed it"
		case v.LastAction == "done" || v.LastAction == "unstash":
			if res != "" && res != "Stashed" {
				return "You marked it done: " + res
			}
			return "You marked it done"
		case strings.HasPrefix(res, "merged into "):
			return "Merged into " + strings.TrimPrefix(res, "merged into ")
		case res == "" || res == "resolved by the agent":
			return "The agent closed it"
		}
		return "The agent closed it: " + res
	case "dismissed":
		if v.LastAction == "noise" {
			if _, note, ok := strings.Cut(res, "time: "); ok {
				return "Not worth your time: “" + note + "”"
			}
			return "Not worth your time"
		}
		if res != "" {
			return "Dismissed: " + res
		}
		return "Dismissed"
	case "expired":
		if res != "" && res != "Expired" {
			return "Expired: " + strings.TrimPrefix(res, "Expired: ")
		}
		return "Expired"
	}
	return res
}

// SessionTitle names the session an item came from.
func (v itemView) SessionTitle() string {
	if v.Session.Name != "" {
		return v.Session.Name
	}
	if v.Session.Cwd != "" {
		return project(v.Session.Cwd)
	}
	if v.Cwd != "" {
		return project(v.Cwd)
	}
	return ""
}

// SessionState says what the item's session is doing, in words.
func (v itemView) SessionState() string {
	switch v.Session.State {
	case "busy":
		return "working"
	case "idle":
		return "idle"
	case "waiting":
		if v.Session.WaitingFor != "" {
			return "waiting: " + v.Session.WaitingFor
		}
		return "waiting"
	case "ended":
		if v.Session.EndedAt > 0 {
			return "ended " + age(time.Now(), v.Session.EndedAt) + " ago"
		}
		return "ended"
	}
	return v.Session.State
}

// sessionIDOK is what a Claude Code session id looks like. Anything else
// (an agent can report any id) never goes into a command to paste.
var sessionIDOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ResumeCommand resumes the item's ended session in a terminal.
func (v itemView) ResumeCommand() string {
	if v.Session.State != "ended" || !sessionIDOK.MatchString(v.Session.ID) {
		return ""
	}
	cmd := "claude --resume " + shellQuote(v.Session.ID)
	if cwd := v.Session.Cwd; cwd != "" {
		cmd = "cd " + shellQuote(cwd) + " && " + cmd
	}
	return cmd
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Link builds a docket URL that keeps the current sort.
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

// SortName is the current sort order, as the menu names it.
func (q *queueView) SortName() string {
	return map[string]string{"priority": "Priority", "deadline": "Deadline", "newest": "Newest"}[q.Sort]
}

// Empty reports a view with nothing that needs the human.
func (q *queueView) Empty() bool { return q.Need == 0 }

// rows turns ranked items into rows, with their thumbnails and deliveries.
func (s *Server) rows(rs []Ranked, list string, showProject bool, projects []string) []itemView {
	ids := make([]string, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	ev := s.st.EvidenceFor(ids)
	var ds map[string]store.Delivery
	if list == "docket" {
		ds = s.st.LatestDeliveries(ids)
	}
	var last map[string]string
	if list != "docket" {
		last = s.st.LastDecisions(ids)
	}
	out := make([]itemView, len(rs))
	for i, r := range rs {
		v := itemView{Ranked: r, List: list, ShowProject: showProject, Delivery: ds[r.ID], LastAction: last[r.ID], Person: s.cfg.Person}
		if projects != nil {
			v.Projects = withProject(projects, projectName(r.Item))
		}
		for _, e := range ev[r.ID] {
			if e.Fetched && strings.HasPrefix(e.Mime, "image/") {
				v.Images = append(v.Images, e)
			} else if !e.Gone {
				v.Docs++
			}
		}
		out[i] = v
	}
	return out
}

// withProject makes sure an item's own project is among the choices, so its
// "move to" select shows where it is (an archived or unsorted one included).
func withProject(names []string, p string) []string {
	for _, n := range names {
		if n == p {
			return names
		}
	}
	out := append(append([]string(nil), names...), p)
	sort.Slice(out, func(i, j int) bool { return projectLess(out[i], out[j]) })
	return out
}

func (s *Server) buildQueue(query url.Values) (*queueView, error) {
	open, waiting, snoozed, err := s.Queue()
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

	// Sidebar: every known project (muted when nothing needs you there),
	// then places in use. An archived project drops out of the sidebar once
	// nothing is open in it.
	projects, _ := s.st.Projects(true)
	desc := map[string]string{}
	count, hasNow, placeCount := map[string]int{}, map[string]bool{}, map[string]int{}
	for _, p := range projects {
		desc[p.Name] = p.Description
		if !p.Archived {
			count[p.Name] += 0
		}
	}
	for _, r := range open {
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
	for _, r := range waiting {
		count[projectName(r.Item)] += 0 // its project stays in the sidebar
	}
	var names []string
	for p := range count {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool { return projectLess(names[i], names[j]) })
	// Where items can be moved: real projects ("unsorted" only shows as an
	// unsorted item's own current place).
	for _, n := range names {
		if n != store.Unsorted {
			q.Projects = append(q.Projects, n)
		}
	}
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
	in := func(r Ranked) bool {
		switch q.Mode {
		case "now":
			return r.Urgency == "now"
		case "project":
			return projectName(r.Item) == q.Project
		case "place":
			return r.Place == q.Place
		}
		return true
	}
	var shown, wait, snz []Ranked
	for _, r := range open {
		if in(r) {
			shown = append(shown, r)
		}
	}
	if q.Mode != "now" {
		for _, r := range waiting {
			if in(r) {
				wait = append(wait, r)
			}
		}
		for _, r := range snoozed {
			if in(r) {
				snz = append(snz, r)
			}
		}
	}
	SortItems(shown, q.Sort)
	// The longest-waiting answer first.
	sort.SliceStable(wait, func(i, j int) bool { return wait[i].AnsweredAt < wait[j].AnsweredAt })
	SortItems(snz, q.Sort)
	q.Need, q.Waiting = len(shown), len(wait)

	byProj := q.Mode != "project"
	var now, rest []Ranked
	for _, r := range shown {
		if r.Urgency == "now" && q.Mode != "now" {
			now = append(now, r)
		} else {
			rest = append(rest, r)
		}
	}
	q.Now = s.rows(now, "docket", byProj, q.Projects)
	q.Snoozed = s.rows(snz, "docket", byProj, q.Projects)

	switch q.Mode {
	case "all":
		q.Heading = "All"
	case "now":
		q.Heading = "Act now"
	case "project":
		q.Heading = q.Project
		q.Desc = desc[q.Project]
		if p, ok := s.st.GetProject(q.Project); ok {
			q.ProjectInfo = &p
		}
	case "place":
		q.Heading = q.Place
		q.Desc = "Everything that needs you there, so one trip covers it."
	}
	switch q.Mode {
	case "now":
		if len(rest) > 0 {
			q.Sections = []section{{Rows: s.rows(rest, "docket", true, q.Projects)}}
		}
	case "project":
		sec := section{Key: q.Project, Rows: s.rows(rest, "docket", false, q.Projects), Waiting: s.rows(wait, "docket", false, q.Projects)}
		sec.countStuck()
		if len(sec.Rows)+len(sec.Waiting) > 0 {
			q.Sections = []section{sec}
		}
	default:
		q.Sections = s.byProject(rest, wait, desc, q)
	}
	return q, nil
}

func (sec *section) countStuck() {
	for _, w := range sec.Waiting {
		if _, stuck := w.DeliveryNote(); stuck {
			sec.Stuck++
		}
	}
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

// byProject splits the docket into one section per project, in the order of
// each project's most pressing item (so a project with something due soon
// comes first). Projects with only answers waiting on agents come last.
func (s *Server) byProject(rest, wait []Ranked, desc map[string]string, q *queueView) []section {
	idx := map[string]int{}
	var order []string
	rows, waits := map[string][]Ranked{}, map[string][]Ranked{}
	for _, r := range rest {
		p := projectName(r.Item)
		if _, ok := idx[p]; !ok {
			idx[p] = len(order)
			order = append(order, p)
		}
		rows[p] = append(rows[p], r)
	}
	var waitOnly []string
	for _, r := range wait {
		p := projectName(r.Item)
		if _, ok := idx[p]; !ok {
			idx[p] = -1
			waitOnly = append(waitOnly, p)
		}
		waits[p] = append(waits[p], r)
	}
	sort.Slice(waitOnly, func(i, j int) bool { return projectLess(waitOnly[i], waitOnly[j]) })
	var out []section
	for _, p := range append(order, waitOnly...) {
		sec := section{Title: p, Desc: desc[p], URL: q.Link(p, "", false), Rows: s.rows(rows[p], "docket", false, q.Projects), Waiting: s.rows(waits[p], "docket", false, q.Projects)}
		sec.countStuck()
		out = append(out, sec)
	}
	return out
}
