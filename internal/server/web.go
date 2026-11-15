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
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/store"
)

//go:embed web/*
var webFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"age":         func(unix int64) string { return age(time.Now(), unix) },
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
	One         *Ranked
	Closed      []store.Item
	Item        *store.Item
	Session     *store.Session
	Evidence    []store.Evidence
	Delivery    []store.Delivery
	Physical    bool
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
			}
		}
	}
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
	s.render(w, "queue.html", page{Title: title, Nav: "queue", Queue: q, Error: r.URL.Query().Get("err")})
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
	q, _ := s.buildQueue(url.Values{})
	one := Ranked{Item: it, Session: sess}
	s.render(w, "item.html", page{Title: it.Title, Nav: "queue", One: &one, Queue: q, Session: &sess, Evidence: ev, Delivery: ds,
		Physical: s.physical(it), Error: r.URL.Query().Get("err")})
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
	s.render(w, "done.html", page{Title: "Done", Nav: "done", Closed: closed})
}

func (s *Server) pageSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var res []store.Item
	if q != "" {
		res, _ = s.st.Search(q, 200)
	}
	s.render(w, "search.html", page{Title: "Search", Nav: "search", Query: q, Closed: res})
}

func (s *Server) pageEvidence(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, _ := s.st.AllEvidence(q, 300)
	s.render(w, "evidence.html", page{Title: "Evidence", Nav: "evidence", Query: q, Gallery: rows})
}

func (s *Server) pageChecklist(w http.ResponseWriter, r *http.Request) {
	tasks, _ := s.st.HumanTasks()
	s.render(w, "checklist.html", page{Title: "Only you", Nav: "checklist", Tasks: tasks})
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
	if strings.Contains(mime, "html") || strings.Contains(mime, "svg") || strings.Contains(mime, "xml") {
		// Agent-made pages are untrusted: render them inert.
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
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
