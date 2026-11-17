package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
)

//go:embed web/*
var webFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"age": func(unix int64) string { return age(time.Now(), unix) },
	"ago": func(unix int64) string {
		if time.Since(time.Unix(unix, 0)) < time.Minute {
			return "just now"
		}
		return age(time.Now(), unix) + " ago"
	},
	"md":          renderMarkdown,
	"kindClass":   kindClass,
	"until":       until,
	"dueState":    dueState,
	"dueLabel":    dueLabel,
	"list":        func(v ...string) []string { return v },
	"closed":      func(status string) bool { return status == "done" || status == "dismissed" || status == "expired" },
	"projectName": projectName,
	"mdShort":     renderMarkdownShort,
	"trimDot":     func(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") },
	"recommended": recommended,
	"project":     project,
	"kindName":    kindName,
	"lower":       strings.ToLower,
	"add":         func(a, b int) int { return a + b },
	"pct":         func(f float64) string { return strconv.FormatFloat(f, 'f', 0, 64) + "%" },
	"tokens": func(n int64) string {
		switch {
		case n >= 1_000_000_000:
			return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + "B"
		case n >= 1_000_000:
			return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
		case n >= 1000:
			return strconv.FormatFloat(float64(n)/1e3, 'f', 0, 64) + "k"
		}
		return strconv.FormatInt(n, 10)
	},
	"winName": func(w string) string {
		return map[string]string{"five_hour": "5-hour", "seven_day": "weekly"}[w]
	},
	"closedAt": func(it store.Item) int64 {
		at := it.UpdatedAt
		switch {
		case it.Status == "delivered":
		case it.ResolvedAt > 0:
			at = it.ResolvedAt
		case it.AnsweredAt > 0:
			at = it.AnsweredAt
		}
		// One that aged off the Stash closed, as far as Done goes, then.
		if off := it.StashedAt + store.StashSeconds; it.StashedAt > 0 && off <= time.Now().Unix() && off > at {
			at = off
		}
		return at
	},
	"bytes": func(n int64) string {
		switch {
		case n >= 1<<20:
			return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
		case n >= 1<<10:
			return strconv.FormatInt(n>>10, 10) + " KB"
		}
		return strconv.FormatInt(n, 10) + " B"
	},
	"tilde": func(p string) string {
		for _, home := range []string{"/Users/", "/home/"} {
			if rest, ok := strings.CutPrefix(p, home); ok {
				if _, after, ok := strings.Cut(rest, "/"); ok {
					return "~/" + after
				}
				return "~"
			}
		}
		return p
	},
	"reply": func(id, placeholder string, hide bool) replyForm {
		return replyForm{ID: id, Placeholder: placeholder, Hide: hide}
	},
	"replyHint": func(kind string) string {
		switch kind {
		case "review", "fyi":
			return "Reply with anything to change"
		case "human_task":
			return "Reply to the agent, e.g. with what you found"
		}
		return "Reply to the agent"
	},
	"doneHint": func(v itemView) string {
		switch {
		case v.SessionID == "" || v.Source != "cli":
			return "Close it"
		case v.Kind == "human_task" || v.Kind == "blocker":
			return "Close it, and tell the agent it's done"
		}
		return "Close it. The agent hears it's done the next time its session is active."
	},
	"imgs": func(es []store.Evidence) string {
		type img struct {
			Src  string `json:"src"`
			Name string `json:"name"`
		}
		out := make([]img, len(es))
		for i, e := range es {
			out[i] = img{"/blob/" + e.SHA256 + "?type=" + url.QueryEscape(e.Mime), e.Name}
		}
		b, _ := json.Marshal(out)
		return string(b)
	},
	"isImage": func(m string) bool { return strings.HasPrefix(m, "image/") },
	"isVideo": func(m string) bool { return strings.HasPrefix(m, "video/") },
	"rcURL": func(bridge string) string {
		if bridge == "" {
			return ""
		}
		return "https://claude.ai/code/" + bridge
	},
	"clock": func(unix int64) string {
		if unix == 0 {
			return ""
		}
		return time.Unix(unix, 0).Format("Mon Jan 2 15:04")
	},
	"paras": func(s string) []string {
		var out []string
		for _, p := range strings.Split(strings.TrimSpace(s), "\n\n") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	},
}).ParseFS(webFS, "web/*.html"))

// kindName is how an item's type reads on the page: what it asks of the
// human, in one word.
func kindName(k string) string {
	switch k {
	case "ask", "approval":
		return "Decide"
	case "human_task":
		return "Do"
	case "review", "fyi":
		return "Review"
	case "issue", "health":
		return "Problem"
	case "blocker":
		return "Waiting"
	case "rule":
		return "Rule"
	}
	return k
}

func kindClass(k string) string { return strings.ToLower(kindName(k)) }

// until is a short "in 2d" / "in 5h" for a future unix time.
func until(unix int64) string {
	d := time.Until(time.Unix(unix, 0))
	switch {
	case d < 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// dueState is "overdue", "today", "soon" (within 3 days) or "".
func dueState(due string) string {
	if due == "" {
		return ""
	}
	today := time.Now().Format("2006-01-02")
	switch {
	case due < today:
		return "overdue"
	case due == today:
		return "today"
	case due <= time.Now().AddDate(0, 0, 3).Format("2006-01-02"):
		return "soon"
	}
	return ""
}

// dueLabel reads a deadline the way people say it: today, tomorrow, Fri, Nov 20.
func dueLabel(due string) string {
	t, err := time.ParseInLocation("2006-01-02", due, time.Local)
	if err != nil {
		return due
	}
	// Count calendar days in UTC, so a DST change (a 23- or 25-hour day)
	// doesn't make tomorrow read as today.
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch days := int(day.Sub(today).Hours() / 24); {
	case days == 0:
		return "today"
	case days == 1:
		return "tomorrow"
	case days == -1:
		return "yesterday"
	case days > 1 && days < 7:
		return t.Format("Mon")
	}
	return t.Format("Jan 2")
}

// Handler serves the queue UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.pageQueue)
	mux.HandleFunc("GET /item/{id}", s.pageItem)
	mux.HandleFunc("POST /item/{id}/act", s.act)
	mux.HandleFunc("POST /project/{name}", s.editProject)
	mux.HandleFunc("GET /done", s.pageDone)
	mux.HandleFunc("GET /stash", s.pageStash)
	mux.HandleFunc("GET /search", s.pageSearch)
	mux.HandleFunc("GET /evidence", s.pageEvidence)
	mux.HandleFunc("GET /checklist", s.pageChecklist)
	mux.HandleFunc("GET /leads", s.pageLeads)
	mux.HandleFunc("GET /brain", s.pageBrain)
	mux.HandleFunc("POST /brain/propose", s.proposeNow)
	mux.HandleFunc("POST /rule/{id}/retire", s.retireRule)
	mux.HandleFunc("POST /lead/{name}/wake", s.wakeLead)
	mux.HandleFunc("GET /health", s.pageHealth)
	mux.HandleFunc("GET /blob/{sha}", s.blob)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"v": s.st.QueueDigest()})
	})
	mux.HandleFunc("POST /api/collect", func(w http.ResponseWriter, r *http.Request) {
		s.KickPull("")
		w.WriteHeader(http.StatusNoContent)
	})
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	return s.guard(mux)
}

// guard enforces the optional Tailscale login allowlist and blocks
// cross-site form posts.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.AllowLogins) > 0 {
			login := r.Header.Get("Tailscale-User-Login")
			ok := false
			for _, a := range s.cfg.AllowLogins {
				if strings.EqualFold(a, login) {
					ok = true
				}
			}
			if !ok {
				http.Error(w, "not allowed", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodPost {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type page struct {
	Title       string
	Nav         string
	Person      string
	Queue       *queueView
	One         *itemView
	Attachments []attachment
	Rows        []itemView
	Closed      []store.Item
	Item        *store.Item
	Session     *store.Session
	Evidence    []store.Evidence
	Delivery    []store.Delivery
	Machines    []machineRow
	Sessions    []store.Session
	Error       string
	Version     string
	Query       string
	Gallery     []store.EvidenceRow
	Tasks       []store.Item
	Leads       []leadRow
	Jobs        []store.Job
	Quotas      []store.Quota
	Spend       []Spend
	Rules       []store.Rule
	Corrections []store.Decision
	Noise       []store.Noise
	RuleCount   int
	Count       int  // open items on the docket, for the mark's badge
	Hot         bool // one of them needs action now
	StashCount  int
}

// attachment is one piece of evidence as the item sheet shows it.
type attachment struct {
	store.Evidence
	Kind  string        // image | video | doc | file | missing
	Index int           // an image's place among the item's images, for the viewer
	Doc   template.HTML // a text document's contents, rendered
}

// maxDoc is the largest text attachment shown inline; maxMarkdown the
// largest rendered as markdown (bigger ones show as plain text).
const (
	maxDoc      = 256 << 10
	maxMarkdown = 24 << 10
)

// replyForm is the dock's answer box.
type replyForm struct {
	ID, Placeholder string
	Hide            bool // folded until Reply (or r) opens it: the options come first
}

type leadRow struct {
	Lead
	Session  store.Session
	HasSess  bool
	Open     int
	LastWake int64
	NextWake string
}

type machineRow struct {
	store.Machine
	Mode   string
	Status string
}

func (s *Server) render(w http.ResponseWriter, name string, p page) {
	p.Person = s.cfg.Person
	p.Version = s.st.QueueDigest()
	if items, err := s.st.Items("open"); err == nil {
		for _, it := range items {
			if it.Kind == "rule" {
				p.RuleCount++
				continue
			}
			p.Count++
			p.Hot = p.Hot || it.Urgency == "now"
		}
	}
	p.StashCount = s.st.StashCount()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, p); err != nil {
		s.log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) pageQueue(w http.ResponseWriter, r *http.Request) {
	q, err := s.buildQueue(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	title := "docket"
	if q.Mode != "all" {
		title = q.Heading
	}
	s.render(w, "queue.html", page{Title: title, Nav: "docket", Queue: q, Error: r.URL.Query().Get("err")})
}

func (s *Server) pageItem(w http.ResponseWriter, r *http.Request) {
	// An item's own id shows that item (a merged one included); only an id
	// that isn't an item (an agent's re-filing alias) forwards.
	it, ok, err := s.st.GetItem(r.PathValue("id"))
	if err == nil && !ok {
		it, ok, err = s.st.ResolveItem(r.PathValue("id"))
	}
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	sess, _, _ := s.st.GetSession(it.SessionID)
	ev, _ := s.st.Evidence(it.ID)
	ds, _ := s.st.ItemDeliveries(it.ID)
	projects, _ := s.st.Projects(false)
	var names []string
	for _, p := range projects {
		if p.Name != store.Unsorted {
			names = append(names, p.Name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return projectLess(names[i], names[j]) })
	one := s.rows([]Ranked{{Item: it, Session: sess}}, "docket", true, names)[0]
	one.LastAction = s.st.LastDecision(it.ID)
	var atts []attachment
	images := 0
	for _, e := range ev {
		a := attachment{Evidence: e, Kind: "file"}
		switch {
		case e.Gone || !e.Fetched:
			a.Kind = "missing"
		case strings.HasPrefix(e.Mime, "image/"):
			a.Kind, a.Index = "image", images
			images++
		case strings.HasPrefix(e.Mime, "video/"):
			a.Kind = "video"
		case textDoc(e):
			// Sized from the file itself, not what the agent declared.
			// Markdown renders only while small (some inputs take the
			// renderer seconds per kilobyte); a bigger file shows as text.
			if info, err := os.Stat(s.blobPath(e.SHA256)); err == nil && info.Size() <= maxDoc {
				if b, err := os.ReadFile(s.blobPath(e.SHA256)); err == nil {
					a.Kind = "doc"
					if (strings.HasSuffix(strings.ToLower(e.Name), ".md") || strings.Contains(strings.ToLower(e.Mime), "markdown")) && len(b) <= maxMarkdown {
						a.Doc = renderMarkdown(string(b))
					} else {
						a.Doc = template.HTML("<pre>" + template.HTMLEscapeString(strings.ToValidUTF8(string(b), "\uFFFD")) + "</pre>")
					}
				}
			}
		}
		atts = append(atts, a)
	}
	// Pictures first, then documents, then the rest.
	rank := map[string]int{"image": 0, "video": 1, "doc": 2, "file": 3, "missing": 4}
	sort.SliceStable(atts, func(i, j int) bool { return rank[atts[i].Kind] < rank[atts[j].Kind] })
	images = 0
	for i := range atts {
		if atts[i].Kind == "image" {
			atts[i].Index = images
			images++
		}
	}
	s.render(w, "item.html", page{Title: it.Title, Nav: "docket", One: &one, Attachments: atts, Session: &sess, Delivery: ds, Error: r.URL.Query().Get("err")})
}

// textDoc reports evidence that reads as text: markdown, plain text, logs, CSV.
func textDoc(e store.Evidence) bool {
	m, n := strings.ToLower(e.Mime), strings.ToLower(e.Name)
	if strings.HasPrefix(m, "text/") && !strings.Contains(m, "html") {
		return true
	}
	for _, ext := range []string{".md", ".markdown", ".txt", ".log", ".csv", ".json", ".yaml", ".yml"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

// editProject saves a project's description and archived flag, or renames it
// (merging into an existing project of that name).
func (s *Server) editProject(w http.ResponseWriter, r *http.Request) {
	name := store.Slug(r.PathValue("name"))
	r.ParseForm()
	desc := strings.TrimSpace(r.FormValue("description"))
	archived := r.FormValue("archived") != ""
	target := name
	var err error
	rename := strings.TrimSpace(r.FormValue("rename"))
	switch to := store.Slug(rename); {
	case rename != "" && to == "":
		err = fmt.Errorf("%q isn't a usable project name: use letters, digits and dashes", rename)
	case to != "" && to != name:
		if err = s.st.RenameProject(name, to); err == nil {
			target = to
		}
	}
	// The description and archive box apply to the project as it's now named
	// (after a merge, the existing description is kept unless one was typed).
	if err == nil && target != store.Unsorted {
		if p, ok := s.st.GetProject(target); !ok {
			err = s.st.EnsureProject(target, desc)
		} else {
			// The form starts with this project's description; on a merge,
			// that untouched text mustn't replace the target's own. On a plain
			// save it's stored as typed (empty clears it).
			if target != name && desc == strings.TrimSpace(r.FormValue("old_description")) {
				desc = p.Description
			}
			// Likewise the archive box: an untouched box keeps the target's own flag.
			if target != name && archived == (r.FormValue("old_archived") != "") {
				archived = p.Archived
			}
			err = s.st.SetProject(target, desc, archived)
		}
	}
	back := "/?p=" + url.QueryEscape(target)
	if archived && err == nil {
		back = "/"
	}
	if err != nil {
		back = "/?p=" + url.QueryEscape(name) + "&err=" + url.QueryEscape(err.Error())
	}
	s.st.Bump()
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) pageDone(w http.ResponseWriter, r *http.Request) {
	closed, _ := s.st.RecentlyClosed(time.Now().Add(-7*24*time.Hour).Unix(), 300)
	s.render(w, "list.html", page{Title: "Done", Nav: "done", Rows: s.rows(ranked(closed), "done", true, nil)})
}

func (s *Server) pageStash(w http.ResponseWriter, r *http.Request) {
	stashed, _ := s.st.Stashed(300)
	s.render(w, "list.html", page{Title: "Stash", Nav: "stash", Rows: s.rows(ranked(stashed), "stash", true, nil)})
}

func ranked(items []store.Item) []Ranked {
	out := make([]Ranked, len(items))
	for i, it := range items {
		out[i] = Ranked{Item: it}
	}
	return out
}

func (s *Server) pageSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var res []store.Item
	if q != "" {
		res, _ = s.st.Search(q, 200)
	}
	s.render(w, "search.html", page{Title: "Search", Nav: "search", Query: q, Rows: s.rows(ranked(res), "list", true, nil)})
}

func (s *Server) pageEvidence(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, _ := s.st.AllEvidence(q, 300)
	s.render(w, "evidence.html", page{Title: "Evidence", Nav: "evidence", Query: q, Gallery: rows})
}

func (s *Server) pageChecklist(w http.ResponseWriter, r *http.Request) {
	tasks, _ := s.st.HumanTasks()
	s.render(w, "checklist.html", page{Title: "Only you", Nav: "checklist", Rows: s.rows(ranked(tasks), "list", true, nil)})
}

func (s *Server) pageLeads(w http.ResponseWriter, r *http.Request) {
	open, _ := s.st.Items("open", "answered", "snoozed")
	counts := map[string]int{}
	for _, it := range open {
		counts[it.Lead]++
	}
	now := s.cfg.Now()
	var rows []leadRow
	for _, l := range s.cfg.Leads {
		row := leadRow{Lead: l, Open: counts[l.Name]}
		row.Session, row.HasSess = s.leadSession(l)
		row.LastWake, _ = strconv.ParseInt(s.st.Get("lead:"+l.Name+":last_wake"), 10, 64)
		var next time.Time
		for _, spec := range l.Schedule {
			if sc, err := parseSchedule(spec); err == nil {
				base := now
				if row.LastWake > 0 {
					base = time.Unix(row.LastWake, 0).In(now.Location())
				}
				if n := sc.next(base); next.IsZero() || n.Before(next) {
					next = n
				}
			}
		}
		if !next.IsZero() && l.On() {
			row.NextWake = next.Format("Mon 15:04")
		}
		rows = append(rows, row)
	}
	jobs, _ := s.st.Jobs(100)
	s.render(w, "leads.html", page{Title: "Leads", Nav: "leads", Leads: rows, Jobs: jobs, Error: r.URL.Query().Get("err")})
}

func (s *Server) wakeLead(w http.ResponseWriter, r *http.Request) {
	l, ok := s.cfg.Lead(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		text = l.WakePrompt
	} else {
		text = s.cfg.Person + " says (from the docket queue): " + text
	}
	back := "/leads"
	if err := s.Wake(r.Context(), l, text); err != nil {
		back += "?err=" + url.QueryEscape(err.Error())
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) pageBrain(w http.ResponseWriter, r *http.Request) {
	rules, _ := s.st.Rules("approved")
	proposed, _ := s.st.Items("open")
	var rs []store.Item
	for _, it := range proposed {
		if it.Kind == "rule" {
			rs = append(rs, it)
		}
	}
	noise, _ := s.st.RecentNoise(50, false)
	ds, _ := s.st.DecisionsSince(time.Now().AddDate(0, 0, -14).Unix(), 500)
	var corr []store.Decision
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Action == "correction" {
			corr = append(corr, ds[i])
		}
	}
	msg := r.URL.Query().Get("err")
	if m := r.URL.Query().Get("msg"); m != "" {
		msg = m
	}
	s.render(w, "brain.html", page{Title: "Brain", Nav: "brain", Rules: rules, Closed: rs, Corrections: corr, Noise: noise, Error: msg})
}

func (s *Server) proposeNow(w http.ResponseWriter, r *http.Request) {
	n, err := s.ProposeRules(r.Context())
	q := "?msg=" + url.QueryEscape(strconv.Itoa(n)+" rule(s) proposed")
	if err != nil {
		q = "?err=" + url.QueryEscape(err.Error())
	} else if n == 0 {
		q = "?msg=" + url.QueryEscape("Nothing new to propose yet.")
	}
	http.Redirect(w, r, "/brain"+q, http.StatusSeeOther)
}

func (s *Server) retireRule(w http.ResponseWriter, r *http.Request) {
	q := ""
	if err := s.RetireRule(r.PathValue("id")); err != nil {
		q = "?err=" + url.QueryEscape(err.Error())
	}
	http.Redirect(w, r, "/brain"+q, http.StatusSeeOther)
}

func (s *Server) pageHealth(w http.ResponseWriter, r *http.Request) {
	ms, _ := s.st.Machines()
	var rows []machineRow
	known := map[string]bool{}
	for _, m := range ms {
		known[m.Name] = true
		cm, _ := s.cfg.Machine(m.Name)
		status := "ok"
		switch {
		case m.FailingSince > 0 && cm.Mode != "push":
			status = "unreachable since " + time.Unix(m.FailingSince, 0).Format("Mon 15:04")
		case cm.Mode == "push" && time.Since(time.Unix(m.LastSeenAt, 0)) > 5*time.Minute:
			status = "offline (last sync " + age(time.Now(), m.LastSeenAt) + " ago)"
		}
		rows = append(rows, machineRow{Machine: m, Mode: cm.Mode, Status: status})
	}
	for _, cm := range s.cfg.Machines {
		if !known[cm.Name] {
			rows = append(rows, machineRow{Machine: store.Machine{Name: cm.Name}, Mode: cm.Mode, Status: "never seen"})
		}
	}
	sess, _ := s.st.Sessions(false, 200)
	quotas, _ := s.st.Quotas()
	s.render(w, "health.html", page{Title: "Health", Nav: "health", Machines: rows, Sessions: sess, Quotas: quotas, Spend: s.LeadSpend(7)})
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a := Action{Kind: r.FormValue("action"), Text: r.FormValue("text"), Project: r.FormValue("project"), Urgency: r.FormValue("urgency")}
	if o := r.FormValue("option"); o != "" {
		a.Option, _ = strconv.Atoi(o)
	}
	switch r.FormValue("until") {
	case "tomorrow":
		t := s.cfg.Now().AddDate(0, 0, 1)
		a.Until = time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, t.Location())
	case "week":
		a.Until = time.Now().Add(7 * 24 * time.Hour)
	case "", "hour":
		a.Until = time.Now().Add(time.Hour)
	default:
		// A specific local time, e.g. 2026-11-10T18:00.
		t, err := time.ParseInLocation("2006-01-02T15:04", r.FormValue("until"), s.cfg.loc)
		if err != nil || !t.After(time.Now()) {
			http.Error(w, "until must be hour, tomorrow, week or a future YYYY-MM-DDTHH:MM", http.StatusBadRequest)
			return
		}
		a.Until = t
	}
	err := s.Decide(id, a)
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "1"})
		return
	}
	back := r.FormValue("back")
	if !localPath(back) {
		back = "/"
	}
	if err != nil {
		back = "/item/" + url.PathEscape(id) + "?err=" + url.QueryEscape(err.Error())
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.blobPath(sha))
	if err != nil {
		http.Error(w, "not fetched from its machine yet", http.StatusNotFound)
		return
	}
	defer f.Close()
	mime := r.URL.Query().Get("type")
	if mime == "" || strings.ContainsAny(mime, "\r\n") {
		mime = "application/octet-stream"
	}
	// Agent-made files are untrusted: whatever type they claim, in any
	// letter case, a page opened from one is inert (no script, no origin).
	// Embedding as <img> or <video> is unaffected.
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	info, _ := f.Stat()
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// recommended reports whether an option is the one the agent recommended:
// the same text, or the option starts with the recommended label ("a" for
// "a: run it again").
func recommended(option, rec string) bool {
	o, r := strings.ToLower(strings.TrimSpace(option)), strings.ToLower(strings.TrimSpace(rec))
	if r == "" {
		return false
	}
	if o == r {
		return true
	}
	if !strings.HasPrefix(o, r) {
		return false
	}
	switch o[len(r)] {
	case ':', ')', '.', ' ', '-', ',':
		return true
	}
	return false
}

// localPath reports whether a redirect target stays on this site: a path
// like /?p=x, never //host, /\host or anything with control characters,
// which browsers may treat as another host.
func localPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Host == "" && u.Scheme == ""
}
