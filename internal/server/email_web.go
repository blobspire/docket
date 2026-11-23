package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	netmail "net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mail"
	"github.com/blobspire/docket/internal/store"
)

// emailSection is the bottom of the docket: replies that need you, then the
// digest of what's worth reading.
type emailSection struct {
	Rows       []itemView // reply items
	Waiting    []itemView // replies on their way out, or waiting on work
	Digest     []digestLine
	Earlier    []digestLine // seen before the latest pass came in
	Pass       store.EmailPass
	HasPass    bool
	Running    bool
	Next       string // when the next pass runs
	Problems   []string
	DigestDays int
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
	// Seen lines fold into Earlier once a later check brought new ones.
	foldBefore := s.st.LastDigestPass()
	for _, t := range lines {
		a, _ := s.cfg.Email.Account(t.Account)
		d := digestLine{EmailThread: t, Label: a.Label, When: shortWhen(now, time.Unix(t.InboundAt, 0).In(s.cfg.loc))}
		if t.SeenAt > 0 && foldBefore > 0 && t.SeenAt < foldBefore {
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
	if e.HasPass && strings.Contains(e.Pass.Error, "triage: ") {
		e.Problems = append(e.Problems, "The last check couldn't sort new mail")
	}
	e.DigestDays = s.cfg.Email.DigestDays
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
	if b := r.URL.Query().Get("back"); localPath(b) {
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
	if extra, rerr := s.replyAct(w, r, t, action); rerr != errUnknownAction {
		s.bumpEmail()
		if r.Header.Get("X-Requested-With") == "fetch" {
			w.Header().Set("Content-Type", "application/json")
			if rerr != nil {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]string{"error": rerr.Error()})
				return
			}
			if extra == nil {
				extra = map[string]any{}
			}
			extra["ok"] = "1"
			json.NewEncoder(w).Encode(extra)
			return
		}
		back := "/item/" + t.ItemID
		if rerr != nil {
			back += "?err=" + url.QueryEscape(rerr.Error())
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
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
			what := t.Subject
			if t.Verdict == "digest" && t.Line != "" {
				what = t.Line
			}
			line = oneLine("Reply to "+firstName(t.From.Display())+": "+what, 120)
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
	if !localPath(back) {
		back = "/"
	}
	if itemID != "" {
		back = "/item/" + itemID
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// firstName is the first name in a display name: "Nancy Smith" and
// Outlook's "Smith, Nancy" both give Nancy.
func firstName(s string) string {
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), `"'`))
	if last, first, ok := strings.Cut(s, ","); ok && strings.TrimSpace(first) != "" && !strings.Contains(last, "@") {
		s = strings.TrimSpace(first)
	}
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
	r.ParseForm()
	if ids := r.Form["id"]; len(ids) > 0 {
		// The lines on your screen, not ones a check added since.
		if len(ids) > 500 {
			ids = ids[:500]
		}
		s.st.MarkReadIDs(ids)
	} else {
		s.st.MarkAllRead(time.Now())
	}
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
	// Closing the reply ("Don't reply", Dismiss, Not worth it, Stash)
	// cancels a send still in its Undo window.
	switch a.Kind {
	case "done", "dismiss", "noise", "stash":
		if o, ok := s.st.PendingSend(t.ID); ok && o.Status == "held" {
			s.st.UndoSend(o.ID)
		}
	}
	s.bumpEmail()
}

// emailItem is what a reply item's sheet shows: the draft and its email.
type emailItem struct {
	Thread   store.EmailThread
	Label    string
	Latest   readerMsg
	Earlier  int
	OpenURL  string
	OpenName string

	Draft       *store.Draft
	To, Cc      []recipient
	Outsider    bool          // a recipient isn't in the thread
	Preview     template.HTML // the draft as it will look, links annotated
	Holding     template.HTML
	EditHTML    string        // the draft for the editor (styled, no annotations)
	Signature   template.HTML // the account's signature, for the preview
	Job         *store.EmailJob
	Writing     bool // a draft is being written
	Pending     *store.Outgoing
	PendingMs   int64 // left of its Undo window
	Failed      string
	Stale       bool   // they wrote again after this draft
	Seen        int64  // the newest stored message this sheet shows (an edit made from it knows up to here)
	Sent        bool   // this version's reply went out
	SentAt      int64  // when
	HoldingSent bool   // its holding reply went out
	SaveName    string // "Save to Gmail drafts"
	HoldSecs    int    // the Undo window
}

type recipient struct {
	mail.Addr
	Outside bool // not anyone in this thread
}

func (s *Server) emailItem(it store.Item) *emailItem {
	t, ok := s.st.ThreadByItem(it.ID)
	if !ok {
		return nil
	}
	seen := s.st.LastMessageRow() // read before the thread is, so it never overstates what the sheet shows
	v := s.readerFor(t)
	e := &emailItem{Seen: seen, Thread: t, Label: v.Label, Latest: v.Latest, Earlier: len(v.Messages), OpenURL: v.OpenURL, OpenName: v.OpenName,
		SaveName: "Save to " + v.Label + " drafts", HoldSecs: int(s.cfg.Email.SendHold.Duration.Seconds())}
	if j, ok := s.st.Job(t.ID); ok {
		e.Job = &j
		e.Writing = j.Status == "queued" || j.Status == "running"
	}
	if d, ok := s.st.LatestDraft(t.ID); ok {
		e.Draft = &d
		people := s.threadPeople(t)
		for _, x := range d.To {
			r := recipient{Addr: x, Outside: !people[strings.ToLower(x.Email)]}
			e.Outsider = e.Outsider || r.Outside
			e.To = append(e.To, r)
		}
		for _, x := range d.Cc {
			r := recipient{Addr: x, Outside: !people[strings.ToLower(x.Email)]}
			e.Outsider = e.Outsider || r.Outside
			e.Cc = append(e.Cc, r)
		}
		// Safe to put in the page as is: Clean left only plain tags and
		// http/https/mailto links, and Preview adds docket's own styles.
		e.Preview = template.HTML(mail.Preview(d.HTML))
		if d.Holding != "" {
			e.Holding = template.HTML(mail.Preview(d.Holding))
		}
		e.EditHTML = mail.Style(d.HTML)
		if a, ok := s.cfg.Email.Account(t.Account); ok {
			if sig, err := a.LoadSignature(true); err == nil {
				switch {
				case sig.HTML != "":
					e.Signature = template.HTML(sig.HTML) // the account owner's own file
				case sig.Text != "":
					e.Signature = template.HTML(strings.ReplaceAll(template.HTMLEscapeString(sig.Text), "\n", "<br>"))
				}
			}
		}
	}
	if e.Draft != nil {
		e.Stale = s.st.WroteSince(t.Account, t.Thread, e.Draft.Basis, s.inbound(t.Account))
		kinds, at := s.st.SentKinds(t.ID, e.Draft.Version)
		e.Sent, e.HoldingSent, e.SentAt = kinds["reply"], kinds["holding"], at
	}
	if o, ok := s.st.PendingSend(t.ID); ok {
		e.Pending = &o
		if left := o.HoldUntil - time.Now().UnixMilli(); left > 0 {
			e.PendingMs = left
		}
	} else if o, ok := s.st.LastSend(t.ID); ok && o.Status == "failed" {
		e.Failed = o.Error
	}
	return e
}

// replyAct handles a reply item's email buttons.
//
//	send / holding   hold the shown version for the Undo window, then send it
//	undo-send        withdraw it
//	revise           ask the drafting agent for changes, in your words
//	redraft          write the draft again (keeping your edits and asked-for changes in this reply)
//	retry            run the job that failed again, as it was
//	edit             save your own edit of the draft (and recipients)
//	save-draft       put the draft in the account's Drafts folder
func (s *Server) replyAct(w http.ResponseWriter, r *http.Request, t store.EmailThread, action string) (map[string]any, error) {
	version, _ := strconv.Atoi(r.FormValue("version"))
	switch action {
	case "revise", "redraft", "retry", "edit":
		if _, ok := s.st.PendingSend(t.ID); ok {
			return nil, errors.New("a reply is on its way; undo it first to change the draft")
		}
	}
	switch action {
	case "send", "holding":
		kind := "reply"
		if action == "holding" {
			kind = "holding"
		}
		o, err := s.QueueSend(t.ID, version, kind)
		if err != nil {
			return nil, err
		}
		return map[string]any{"outbox": o.ID, "hold_ms": o.HoldUntil - time.Now().UnixMilli()}, nil
	case "undo-send":
		return nil, s.UndoSend(t.ID, r.FormValue("outbox"))
	case "revise":
		text := strings.TrimSpace(r.FormValue("text"))
		if text == "" {
			return nil, errors.New("say what to change")
		}
		if err := s.st.QueueDraft(t.ID, "revise", clipRunes(text, 4000)); err != nil {
			return nil, err
		}
		kick(s.emailOutKick)
		return nil, nil
	case "redraft":
		if err := s.st.QueueDraft(t.ID, "draft", ""); err != nil {
			return nil, err
		}
		kick(s.emailOutKick)
		return nil, nil
	case "retry":
		// The job that failed, as it was: a revision keeps what you asked for.
		j, ok := s.st.Job(t.ID)
		if !ok || j.Status != "failed" {
			return nil, errors.New("there's nothing to try again")
		}
		if err := s.st.QueueDraft(t.ID, j.Kind, j.Instruction); err != nil {
			return nil, err
		}
		kick(s.emailOutKick)
		return nil, nil
	case "edit":
		d, ok := s.st.LatestDraft(t.ID)
		if !ok {
			return nil, errors.New("there's no draft to edit")
		}
		if d.Version != version {
			return nil, fmt.Errorf("the draft changed while you were editing (now version %d); copy your text, reopen it and edit again", d.Version)
		}
		if j, ok := s.st.Job(t.ID); ok && (j.Status == "queued" || j.Status == "running") {
			return nil, errors.New("a new version is being written; wait for it, then edit")
		}
		body, removed := mail.Clean(r.FormValue("html"))
		if strings.TrimSpace(mail.Text(body)) == "" {
			return nil, errors.New("the draft can't be empty")
		}
		to, err := strictAddrs(r.FormValue("to"))
		if err != nil {
			return nil, err
		}
		cc, err := strictAddrs(r.FormValue("cc"))
		if err != nil {
			return nil, err
		}
		if len(to) == 0 {
			return nil, errors.New("who should it go to? Write at least one address in To")
		}
		_ = removed // your own edit: formatting the editor added is simply dropped
		nd := d
		nd.HTML, nd.To, nd.Cc, nd.Source, nd.Instruction, nd.Removed = body, to, cc, "edit", "", nil
		nd.HandRecipients = d.HandRecipients || !sameAddrs(to, d.To) || !sameAddrs(cc, d.Cc)
		// Your own edit is written with the thread in front of you: it
		// knows what the sheet you edited in showed (a message that came
		// while you typed isn't in it), never more than is stored.
		if seen, err := strconv.ParseInt(r.FormValue("seen"), 10, 64); err == nil && seen > d.Basis {
			nd.Basis = min(seen, s.st.LastMessageRow())
		}
		v, err := s.st.AddDraft(nd)
		if err != nil {
			return nil, err
		}
		s.st.TouchItem(t.ItemID)
		return map[string]any{"version": v}, nil
	case "save-draft":
		d, ok := s.st.DraftVersion(t.ID, version)
		if !ok {
			return nil, errors.New("no such draft")
		}
		a, _ := s.cfg.Email.Account(t.Account)
		sig, err := a.LoadSignature(false)
		if err != nil {
			return nil, err
		}
		b, a, err := s.builtReply(t, d, "reply", sig)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		c, err := a.Dial(ctx)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		drafts := s.st.EmailAccountState(a.Name).DraftsFolder
		if err := c.Append(drafts, b.Raw, true); err != nil {
			return nil, err
		}
		s.st.AppendToItem(t.ItemID, "_Saved draft "+strconv.Itoa(d.Version)+" to your "+a.Label+" drafts "+time.Now().In(s.cfg.loc).Format("Mon 15:04")+"._")
		return nil, nil
	}
	return nil, errUnknownAction
}

var errUnknownAction = errors.New("unknown action")

// strictAddrs parses a To or Cc you typed (commas or semicolons), refusing
// the whole field when one address doesn't parse, so nobody is silently
// dropped.
func strictAddrs(field string) ([]mail.Addr, error) {
	// Semicolons (Outlook) separate like commas, except inside a quoted
	// name ("Smith, Nancy" stays one name).
	var b strings.Builder
	quoted, escaped := false, false
	for _, r := range field {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quoted:
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ';' && !quoted:
			r = ','
		}
		b.WriteRune(r)
	}
	field = b.String()
	if strings.TrimSpace(field) == "" {
		return nil, nil
	}
	if list, err := netmail.ParseAddressList(field); err == nil {
		out := make([]mail.Addr, 0, len(list))
		for _, a := range list {
			out = append(out, mail.Addr{Name: a.Name, Email: strings.ToLower(a.Address)})
		}
		return out, nil
	}
	var out []mail.Addr
	for _, part := range strings.FieldsFunc(field, func(r rune) bool { return r == ',' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a := mail.ParseAddrs(part)
		if len(a) != 1 || !strings.Contains(a[0].Email, "@") {
			return nil, fmt.Errorf("%q isn't an email address I can send to", part)
		}
		out = append(out, a[0])
	}
	return out, nil
}

// editAddrs writes addresses for the edit form: readable names (no MIME
// encoding), quoted when they hold a comma or quote.
func editAddrs(list []mail.Addr) string {
	var out []string
	for _, a := range list {
		n := strings.TrimSpace(a.Name)
		switch {
		case n == "":
			out = append(out, a.Email)
		case strings.ContainsAny(n, `,;"<>()[]@:.\`):
			out = append(out, `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(n)+`" <`+a.Email+`>`)
		default:
			out = append(out, n+" <"+a.Email+">")
		}
	}
	return strings.Join(out, ", ")
}

// sameAddrs reports whether two recipient lists name the same addresses
// (names and order aside).
func sameAddrs(a, b []mail.Addr) bool {
	set := func(l []mail.Addr) map[string]bool {
		m := map[string]bool{}
		for _, x := range l {
			m[strings.ToLower(x.Email)] = true
		}
		return m
	}
	x, y := set(a), set(b)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}
