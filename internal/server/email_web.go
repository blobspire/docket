package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mail"
	"github.com/blobspire/docket/internal/store"
)

// emailSection is the bottom of the docket: replies that need you, then the
// digest of what's worth reading.
type emailSection struct {
	Rows     []itemView // reply items
	Waiting  []itemView // replies on their way out, or waiting on work
	Digest   []digestLine
	Earlier  []digestLine // seen before the latest pass came in
	Pass     store.EmailPass
	HasPass  bool
	Running  bool
	Next     string // when the next pass runs
	Problems []string
}

// digestLine is one line of the digest.
type digestLine struct {
	store.EmailThread
	Label string // the account's tag
	When  string
}

func (e *emailSection) Need() int { return len(e.Rows) }

// splitEmail separates email reply items from the rest of the docket.
func splitEmail(rs []Ranked) (rest, email []Ranked) {
	for _, r := range rs {
		if r.Kind == "email" || r.Project == emailProject {
			email = append(email, r)
		} else {
			rest = append(rest, r)
		}
	}
	return rest, email
}

func (s *Server) buildEmail(open, waiting []Ranked, projects []string) *emailSection {
	e := &emailSection{}
	SortItems(open, "newest")
	e.Rows = s.rows(open, "docket", false, projects)
	e.Waiting = s.rows(waiting, "docket", false, projects)
	e.Pass, e.HasPass = s.st.LastPass()
	e.Running = s.st.RunningPass()
	now := s.cfg.Now()
	lines, _ := s.st.DigestLines(now)
	for _, t := range lines {
		a, _ := s.cfg.Email.Account(t.Account)
		d := digestLine{EmailThread: t, Label: a.Label, When: shortWhen(now, time.Unix(t.InboundAt, 0).In(s.cfg.loc))}
		if t.SeenAt > 0 && e.HasPass && t.SeenAt < e.Pass.StartedAt {
			e.Earlier = append(e.Earlier, d)
		} else {
			e.Digest = append(e.Digest, d)
		}
	}
	if last, err := parseUnix(s.st.Get("email:last_pass")); err == nil {
		var next time.Time
		for _, sch := range s.cfg.Email.Schedule {
			if sc, err := parseSchedule(sch); err == nil {
				if n := sc.next(time.Unix(last, 0).In(s.cfg.loc)); next.IsZero() || n.Before(next) {
					next = n
				}
			}
		}
		if !next.IsZero() {
			e.Next = shortWhen(now, next)
		}
	}
	for _, a := range s.cfg.Email.Accounts {
		if st := s.st.EmailAccountState(a.Name); st.Error != "" {
			e.Problems = append(e.Problems, "Can't read "+a.Label+" mail")
		}
	}
	return e
}

// shortWhen is "14:05" today, "Mon 14:05" this week, else "Nov 3".
func shortWhen(now, t time.Time) string {
	y1, m1, d1 := now.Date()
	y2, m2, d2 := t.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04")
	case t.After(now.AddDate(0, 0, -6)) && t.Before(now.AddDate(0, 0, 6)):
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2")
}

func parseUnix(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// readerView is one email, opened from the digest (or a reply item).
type readerView struct {
	Thread   store.EmailThread
	Label    string
	Messages []readerMsg
	Latest   readerMsg
	OpenURL  string // in Gmail or Outlook
	OpenName string
	Back     string
	Skipped  bool
}

type readerMsg struct {
	store.EmailMessage
	When   string
	ToLine string
	CcLine string
	Frame  string // the HTML part, as a sandboxed iframe's srcdoc
	FromMe bool
}

// frameDoc wraps an incoming HTML email for a sandboxed iframe: no scripts
// (the sandbox), nothing loaded from the network (the CSP: tracking pixels
// and remote images stay blocked), links open in a new tab.
func frameDoc(body string) string {
	return `<!doctype html><html><head><meta charset="utf-8">` +
		`<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data: cid:; style-src 'unsafe-inline'; font-src data:">` +
		`<base target="_blank"><style>body{font:15px/1.5 -apple-system,system-ui,sans-serif;margin:12px;color:#222;background:#fff;word-wrap:break-word}img{max-width:100%;height:auto}</style>` +
		`</head><body>` + body + `</body></html>`
}

func (s *Server) readerFor(t store.EmailThread) readerView {
	a, _ := s.cfg.Email.Account(t.Account)
	v := readerView{Thread: t, Label: a.Label, Back: "/"}
	msgs, _ := s.st.ThreadMessages(t.Account, t.Thread)
	for _, m := range msgs {
		rm := readerMsg{EmailMessage: m, When: time.Unix(m.Date, 0).In(s.cfg.loc).Format("Mon Jan 2, 3:04 PM"), FromMe: m.FromMe,
			ToLine: addrList(m.To), CcLine: addrList(m.Cc)}
		if m.HTML != "" {
			rm.Frame = frameDoc(m.HTML)
		}
		v.Messages = append(v.Messages, rm)
	}
	if n := len(v.Messages); n > 0 {
		v.Latest = v.Messages[n-1]
		v.Messages = v.Messages[:n-1]
	}
	v.OpenURL, v.OpenName = openInMail(a, v.Latest.MessageID, t.Subject)
	return v
}

func addrList(list []mail.Addr) string {
	var out []string
	for _, a := range list {
		if a.Name != "" {
			out = append(out, a.Name+" <"+a.Email+">")
		} else {
			out = append(out, a.Email)
		}
	}
	return strings.Join(out, ", ")
}

// openInMail links to the message in the account's own web mail: Gmail
// finds it by Message-ID in the right account; Outlook on the web opens
// a search for its subject.
func openInMail(a mail.Account, messageID, subject string) (string, string) {
	switch a.Web {
	case "gmail":
		if messageID == "" {
			return "", ""
		}
		return "https://mail.google.com/mail/u/" + url.PathEscape(a.Address) + "/#search/" + url.PathEscape("rfc822msgid:"+messageID), "Open in Gmail"
	case "outlook":
		q := strings.TrimSpace(subject)
		return "https://outlook.office.com/mail/search?q=" + url.QueryEscape(q), "Find in Outlook"
	}
	return "", ""
}

func (s *Server) pageEmail(w http.ResponseWriter, r *http.Request) {
	t, ok := s.st.Thread(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Opening a digest line counts as reading it.
	if t.Verdict == "digest" && t.ReadAt == 0 && r.URL.Query().Get("keep") == "" {
		s.st.MarkRead(t.ID, true)
		s.bumpEmail()
		t, _ = s.st.Thread(t.ID)
	}
	v := s.readerFor(t)
	if b := r.URL.Query().Get("back"); strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
		v.Back = b
	}
	s.render(w, "email.html", page{Title: oneLine(firstNonEmptyStr(t.Line, t.Subject), 80), Nav: "email", Email: &v})
}

// emailAct handles a digest line's or the reader's buttons:
//
//	read / unread   tick a line off, or bring it back
//	reply           it needs a reply after all: file a reply item
//	digest          it was worth reading (from the Skipped list)
//	noise           not worth your time: off the digest, and triage learns
func (s *Server) emailAct(w http.ResponseWriter, r *http.Request) {
	t, ok := s.st.Thread(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	action := r.PathValue("action")
	note := strings.TrimSpace(r.FormValue("note"))
	var itemID string
	var err error
	switch action {
	case "read":
		err = s.st.MarkRead(t.ID, true)
	case "unread":
		err = s.st.MarkRead(t.ID, false)
	case "reply":
		if t.Verdict != "reply" {
			s.st.AddEmailFeedback(store.EmailFeedback{ThreadID: t.ID, Was: firstNonEmptyStr(t.Verdict, "skip"), Should: "reply", Line: t.Line, From: t.From.Email, Note: note})
		}
		line := t.Line
		if t.Verdict != "reply" || line == "" {
			line = oneLine("Reply to "+firstName(t.From.Display())+": "+t.Subject, 120)
		}
		s.st.SetVerdict(t.ID, t.PassID, store.Verdict{Verdict: "reply", Line: line, Why: t.Why, Project: t.Project, Work: t.Work, FirstTime: t.FirstTime})
		itemID, err = s.fileReply(t.ID)
	case "digest":
		if t.Verdict != "digest" {
			s.st.AddEmailFeedback(store.EmailFeedback{ThreadID: t.ID, Was: firstNonEmptyStr(t.Verdict, "skip"), Should: "digest", Line: t.Line, From: t.From.Email, Note: note})
		}
		err = s.st.SetVerdict(t.ID, t.PassID, store.Verdict{Verdict: "digest", Line: t.Line, Why: t.Why, Project: t.Project, Work: t.Work,
			FirstTime: t.FirstTime, UntilAt: time.Now().AddDate(0, 0, s.cfg.Email.DigestDays).Unix()})
	case "noise":
		s.st.AddEmailFeedback(store.EmailFeedback{ThreadID: t.ID, Was: firstNonEmptyStr(t.Verdict, "skip"), Should: "skip", Line: t.Line, From: t.From.Email, Note: note})
		err = s.st.SetVerdict(t.ID, t.PassID, store.Verdict{Verdict: "skip", Line: t.Line, Why: t.Why, Project: t.Project, Work: t.Work, FirstTime: t.FirstTime})
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	s.bumpEmail()
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "1", "item": itemID})
		return
	}
	back := r.FormValue("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/"
	}
	if itemID != "" {
		back = "/item/" + itemID
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func firstName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " @"); i > 0 {
		return s[:i]
	}
	return s
}

// emailSeen records digest lines that scrolled into view.
func (s *Server) emailSeen(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	ids := r.Form["id"]
	if len(ids) > 200 {
		ids = ids[:200]
	}
	s.st.MarkSeen(ids)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) emailCheck(w http.ResponseWriter, r *http.Request) {
	s.CheckEmailNow()
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":"1"}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) emailReadAll(w http.ResponseWriter, r *http.Request) {
	s.st.MarkAllRead(time.Now())
	s.bumpEmail()
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":"1"}`))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// skippedView is what a pass left out, to check triage's judgment.
type skippedView struct {
	Pass  store.EmailPass
	Lines []digestLine
}

func (s *Server) pageSkipped(w http.ResponseWriter, r *http.Request) {
	p, ok := s.st.LastPass()
	if id := r.URL.Query().Get("pass"); id != "" {
		p.ID, ok = id, true
	}
	v := skippedView{Pass: p}
	if ok {
		ts, _ := s.st.SkippedIn(p.ID)
		now := s.cfg.Now()
		for _, t := range ts {
			a, _ := s.cfg.Email.Account(t.Account)
			v.Lines = append(v.Lines, digestLine{EmailThread: t, Label: a.Label, When: shortWhen(now, time.Unix(t.InboundAt, 0).In(s.cfg.loc))})
		}
	}
	s.render(w, "skipped.html", page{Title: "Skipped email", Nav: "email", Skipped: &v})
}

// emailDecided follows a reply item's closing actions into the thread: "not
// worth my time" teaches triage the email didn't need a reply.
func (s *Server) emailDecided(it store.Item, a Action) {
	if it.Kind != "email" {
		return
	}
	t, ok := s.st.ThreadByItem(it.ID)
	if !ok {
		return
	}
	if a.Kind == "noise" {
		s.st.AddEmailFeedback(store.EmailFeedback{ThreadID: t.ID, Was: "reply", Should: "skip", Line: t.Line, From: t.From.Email, Note: a.Text})
	}
	s.bumpEmail()
}

// emailItem is what a reply item's sheet shows of its email.
type emailItem struct {
	Thread   store.EmailThread
	Label    string
	Latest   readerMsg
	Earlier  int
	OpenURL  string
	OpenName string
}

func (s *Server) emailItem(it store.Item) *emailItem {
	t, ok := s.st.ThreadByItem(it.ID)
	if !ok {
		return nil
	}
	v := s.readerFor(t)
	return &emailItem{Thread: t, Label: v.Label, Latest: v.Latest, Earlier: len(v.Messages), OpenURL: v.OpenURL, OpenName: v.OpenName}
}
