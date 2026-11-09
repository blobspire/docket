package server

import (
	"embed"
	"encoding/json"
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
	"age": func(unix int64) string { return age(time.Now(), unix) },
	"where": func(v any) string {
		switch it := v.(type) {
		case store.Item:
			return strings.TrimSpace(strings.Trim(where(it), " ()"))
		case *store.Item:
			return strings.TrimSpace(strings.Trim(where(*it), " ()"))
		}
		return ""
	},
	"project":  project,
	"kindName": kindName,
	"lower":    strings.ToLower,
	"add":      func(a, b int) int { return a + b },
	"isImage":  func(m string) bool { return strings.HasPrefix(m, "image/") },
	"isVideo":  func(m string) bool { return strings.HasPrefix(m, "video/") },
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

func kindName(k string) string {
	switch k {
	case "ask":
		return "decision"
	case "human_task":
		return "only you"
	case "approval":
		return "approval"
	case "blocker":
		return "waiting"
	}
	return k
}

// Handler serves the queue UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.pageQueue)
	mux.HandleFunc("GET /item/{id}", s.pageItem)
	mux.HandleFunc("POST /item/{id}/act", s.act)
	mux.HandleFunc("GET /done", s.pageDone)
	mux.HandleFunc("GET /search", s.pageSearch)
	mux.HandleFunc("GET /evidence", s.pageEvidence)
	mux.HandleFunc("GET /checklist", s.pageChecklist)
	mux.HandleFunc("GET /leads", s.pageLeads)
	mux.HandleFunc("POST /lead/{name}/wake", s.wakeLead)
	mux.HandleFunc("GET /health", s.pageHealth)
	mux.HandleFunc("GET /blob/{sha}", s.blob)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"v": s.st.Get("version")})
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
	Title    string
	Nav      string
	Person   string
	Brief    string
	BriefDay string
	Now      []Ranked
	Maybe    []Ranked
	Snoozed  []Ranked
	Closed   []store.Item
	Item     *store.Item
	Session  *store.Session
	Evidence []store.Evidence
	Delivery []store.Delivery
	Physical bool
	Machines []machineRow
	Sessions []store.Session
	Error    string
	Version  string
	Query    string
	Gallery  []store.EvidenceRow
	Tasks    []store.Item
	Leads    []leadRow
	Jobs     []store.Job
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
	p.Version = s.st.Get("version")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, p); err != nil {
		s.log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) pageQueue(w http.ResponseWriter, r *http.Request) {
	now, maybe, snoozed, err := s.Queue()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	p := page{Title: "Queue", Nav: "queue", Now: now, Maybe: maybe, Snoozed: snoozed}
	if s.st.Get("brief_date") == s.cfg.Now().Format("2006-01-02") {
		p.Brief, p.BriefDay = s.st.Get("brief_text"), s.st.Get("brief_date")
	}
	s.render(w, "queue.html", p)
}

func (s *Server) pageItem(w http.ResponseWriter, r *http.Request) {
	it, ok, err := s.st.GetItem(r.PathValue("id"))
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	sess, _, _ := s.st.GetSession(it.SessionID)
	ev, _ := s.st.Evidence(it.ID)
	ds, _ := s.st.ItemDeliveries(it.ID)
	s.render(w, "item.html", page{Title: it.Title, Nav: "queue", Item: &it, Session: &sess, Evidence: ev, Delivery: ds,
		Physical: s.physical(it), Error: r.URL.Query().Get("err")})
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
	s.render(w, "health.html", page{Title: "Health", Nav: "health", Machines: rows, Sessions: sess})
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	a := Action{Kind: r.FormValue("action"), Text: r.FormValue("text")}
	if o := r.FormValue("option"); o != "" {
		a.Option, _ = strconv.Atoi(o)
	}
	switch r.FormValue("until") {
	case "tomorrow":
		t := s.cfg.Now().AddDate(0, 0, 1)
		a.Until = time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, t.Location())
	case "week":
		a.Until = time.Now().Add(7 * 24 * time.Hour)
	default:
		a.Until = time.Now().Add(time.Hour)
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
	if back == "" || !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
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
