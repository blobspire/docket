package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/cli"
	"github.com/blobspire/docket/internal/hook"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

func getOK(t *testing.T, e *env, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// startAgent starts session sess-1 in a project folder and returns its env.
func startAgent(t *testing.T) func(string) string {
	t.Helper()
	agent := getenv(map[string]string{"DOCKET_SESSION": "sess-1"})
	hook.Run("SessionStart", strings.NewReader(`{"session_id":"sess-1","cwd":"/w/vlm","source":"startup"}`), io.Discard, io.Discard, agent)
	return agent
}

// A progress entry is a line on the project's timeline: it never reaches the
// docket, its badge, a push or the agents' list of open items,
// but search finds it.
func TestProgressNeverAsksForAttention(t *testing.T) {
	e := newEnv(t, "")
	agent := startAgent(t)
	var out bytes.Buffer
	if err := cli.File("progress", []string{"Second seed analysis done: camera carry-over confirmed", "--next", "Writing the ablation plan", "--project", "vlm", "--new-project", "VLM teacher"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "progress noted in vlm") || !strings.Contains(out.String(), "no one is notified") {
		t.Fatalf("confirmation: %s", out.String())
	}
	// A hand-made record can't sneak urgency, a key, options or files in.
	e.file(t, "sess-1", record.Item{ID: "dk-sneaky01", Kind: "progress", Title: "Halfway through the sweep", Project: "vlm", Urgency: "now", Key: "sweep", Options: []string{"a", "b"}, Blocking: true, Due: "2026-11-25",
		Evidence: []record.Evidence{{SHA256: strings.Repeat("a", 64), Name: "x.png"}}})
	e.cycle()

	logged, _ := e.s.st.RecentProgress("", 0, 10)
	if len(logged) != 2 {
		t.Fatalf("progress entries = %+v", logged)
	}
	for _, it := range logged {
		if it.Status != store.StatusLogged || it.Urgency != "normal" || it.DedupeKey != "" || len(it.Options) != 0 || it.Blocking || it.Due != "" {
			t.Fatalf("entry = %+v", it)
		}
		if ev, _ := e.s.st.Evidence(it.ID); len(ev) != 0 {
			t.Fatalf("a progress entry kept evidence: %+v", ev)
		}
	}
	for _, it := range logged {
		if strings.HasPrefix(it.Title, "Second seed") && it.Next != "Writing the ablation plan" {
			t.Fatalf("next = %q", it.Next)
		}
	}
	if open, waiting, snoozed, _ := e.s.Queue(); len(open)+len(waiting)+len(snoozed) != 0 {
		t.Fatalf("progress on the docket: %+v %+v %+v", open, waiting, snoozed)
	}
	if n := len(active(t, e.s.st)); n != 0 {
		t.Fatalf("%d active items", n)
	}
	if b := getOK(t, e, "/"); strings.Contains(b, `class="bc`) {
		t.Fatal("progress counted in the badge")
	}
	if res, _ := e.s.st.Search("carry-over", 10); len(res) != 1 {
		t.Fatalf("search found %d", len(res))
	}
	var list bytes.Buffer
	cli.List([]string{"--all"}, &list, agent)
	if strings.Contains(list.String(), "Second seed") {
		t.Fatalf("docket list shows progress:\n%s", list.String())
	}
}

func TestProgressCommandRefusesWhatAsksForAttention(t *testing.T) {
	newEnv(t, "")
	agent := startAgent(t)
	for _, args := range [][]string{
		{"x", "--urgency", "now"}, {"x", "--key", "k"}, {"x", "--options", "a|b"}, {"x", "--due", "today"}, {"x", "--blocking"}, {"x", "--evidence", "f.png"},
		{strings.Repeat("w", 281)}, {"x", "--next", "two\nlines"}, {"x", "--next", strings.Repeat("n", 201)}, {},
	} {
		if err := cli.File("progress", append(args, "--project", "vlm"), io.Discard, agent); err == nil {
			t.Errorf("docket progress %q was accepted", args)
		}
	}
	if err := cli.File("progress", []string{strings.Repeat("w", 280), "--project", "vlm", "--new-project", "x"}, io.Discard, agent); err != nil {
		t.Errorf("280 characters refused: %v", err)
	}
}

// The human can ask about a progress entry or mark it not worth their time;
// nothing else applies to it.
func TestProgressEntryActions(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "sess-1", record.Item{ID: "dk-prog0001", Kind: "progress", Title: "Ran the nightly tests", Project: "vlm"})
	for _, k := range []string{"done", "dismiss", "stash", "snooze", "reopen", "answer", "approve", "choose", "handoff", "reclassify"} {
		if err := e.s.Decide("dk-prog0001", Action{Kind: k, Text: "x", Until: time.Now().Add(time.Hour), Project: "other"}); err == nil {
			t.Errorf("%s on a progress entry was accepted", k)
		}
	}
	if it, _, _ := e.s.st.GetItem("dk-prog0001"); it.Status != store.StatusLogged || it.Project != "vlm" {
		t.Fatalf("after refused actions: %+v", it)
	}
	if err := e.s.Decide("dk-prog0001", Action{Kind: "noise", Text: "routine, skip these"}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := e.s.st.GetItem("dk-prog0001")
	if it.Status != "dismissed" {
		t.Fatalf("after Not worth it: %s", it.Status)
	}
	ds, _ := e.s.st.ItemDeliveries("dk-prog0001")
	if len(ds) != 1 || !ds[0].Quiet || !strings.Contains(ds[0].Text, "progress entry") || !strings.Contains(ds[0].Text, "major stage") || !strings.Contains(ds[0].Text, "routine, skip these") {
		t.Fatalf("noise note = %+v", ds)
	}
	if tl, _, _ := e.s.st.Timeline("vlm", "", 0, 10); len(tl) != 0 {
		t.Fatalf("a dismissed entry is still on the timeline: %+v", tl)
	}
	if err := e.s.Decide("dk-prog0001", Action{Kind: "undo"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := e.s.st.GetItem("dk-prog0001"); it.Status != store.StatusLogged {
		t.Fatalf("after undo: %s", it.Status)
	}
}

// Ask the agent: a question about a result already marked done reaches the
// session that filed it, the item stays closed, and the agent's answer
// lands on the item, where the sheet shows it.
func TestAskTheAgentAboutAResult(t *testing.T) {
	e := newEnv(t, "")
	agent := startAgent(t)
	var out bytes.Buffer
	if err := cli.File("review", []string{"Seed 1004: carry-over confirmed", "--project", "vlm", "--new-project", "x", "--record", "results/lp/seed_1004 @ 3f2a1c9", "--why", "38.3% vs 33.5%"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	open, _, _, _ := e.s.Queue()
	id := open[0].ID
	if err := e.s.Decide(id, Action{Kind: "ask", Text: "Which checkpoint was that?"}); err == nil {
		t.Fatal("asked about an item still on the docket (it has Reply)")
	}
	if err := e.s.Decide(id, Action{Kind: "done"}); err != nil {
		t.Fatal(err)
	}
	heard(agent) // the "marked done" note
	if err := e.s.Decide(id, Action{Kind: "ask", Text: "   "}); err == nil {
		t.Fatal("an empty question was sent")
	}
	if err := e.s.Decide(id, Action{Kind: "ask", Text: "Which checkpoint was that,\nand how many episodes?"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := e.s.st.GetItem(id); it.Status != "done" {
		t.Fatalf("asking reopened it: %s", it.Status)
	}
	sheet := getOK(t, e, "/item/"+id)
	if !strings.Contains(sheet, "You asked") || !strings.Contains(sheet, "Which checkpoint was that,\nand how many episodes?") || !strings.Contains(sheet, "data-asking") {
		t.Fatalf("sheet while asking:\n%s", sheet)
	}
	if !strings.Contains(sheet, `value="ask"`) || !strings.Contains(sheet, "Ask the agent about this") {
		t.Fatal("a done result's sheet has no Ask box")
	}

	e.s.deliverAll(e.ctx)
	msg := heard(agent)
	for _, want := range []string{"Cole asks about your docket item", "Seed 1004: carry-over confirmed", "Which checkpoint was that,", "docket add " + id + " --why", "results/lp/seed_1004 @ 3f2a1c9", "don't reopen or re-file it"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the agent got %q, missing %q", msg, want)
		}
	}
	e.cycle() // the receipt
	if it, _, _ := e.s.st.GetItem(id); it.Status != "done" {
		t.Fatalf("the receipt reopened it: %s", it.Status)
	}
	if sheet := getOK(t, e, "/item/"+id); !strings.Contains(sheet, "the agent has it") || !strings.Contains(sheet, "data-asking") {
		t.Fatal("the sheet doesn't say the agent has the question")
	}

	// The agent's answer goes under the question, in the item's text.
	if err := cli.Add([]string{id, "--why", "Checkpoint step 40k, 517 episodes per arm."}, &out, agent); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	it, _, _ := e.s.st.GetItem(id)
	if it.Status != "done" || !strings.HasSuffix(it.Body, "38.3% vs 33.5%\n\n**You asked:** Which checkpoint was that, and how many episodes?\n\nCheckpoint step 40k, 517 episodes per arm.") {
		t.Fatalf("after the answer: %s %q", it.Status, it.Body)
	}
	sheet = getOK(t, e, "/item/"+id)
	if strings.Contains(sheet, "data-asking") || strings.Contains(sheet, `class="asks"`) || !strings.Contains(sheet, "<strong>You asked:</strong> Which checkpoint") {
		t.Fatalf("sheet after the answer:\n%s", sheet)
	}
	// A later note from the agent isn't labelled as an answer.
	cli.Add([]string{id, "--why", "Also: the B200 control is in."}, &out, agent)
	e.cycle()
	if it, _, _ := e.s.st.GetItem(id); strings.Count(it.Body, "You asked") != 1 {
		t.Fatalf("a later note was labelled as an answer: %q", it.Body)
	}
}

func TestAskNeedsAnAgentSession(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-nosess01", Kind: "review", Title: "Watchdog report", Status: "done", Source: "watchdog"})
	if err := e.s.Decide("dk-nosess01", Action{Kind: "ask", Text: "why?"}); err == nil {
		t.Fatal("asked an item no agent filed")
	}
	if b := getOK(t, e, "/item/dk-nosess01"); strings.Contains(b, "Ask the agent about this") {
		t.Fatal("Ask offered with no session")
	}
	// A progress entry can be asked about too.
	agent := startAgent(t)
	cli.File("progress", []string{"Sweep finished", "--project", "vlm", "--new-project", "x"}, io.Discard, agent)
	e.cycle()
	p, _ := e.s.st.RecentProgress("vlm", 0, 1)
	if err := e.s.Decide(p[0].ID, Action{Kind: "ask", Text: "Any failures?"}); err != nil {
		t.Fatal(err)
	}
	e.s.deliverAll(e.ctx)
	if msg := heard(agent); !strings.Contains(msg, "your progress entry") || !strings.Contains(msg, "Any failures?") {
		t.Fatalf("heard %q", msg)
	}
}

// The docket folds recent progress under the action items; the timeline
// keeps a project's history, with Results as one of its filters.
func TestProgressFoldAndTimeline(t *testing.T) {
	e := newEnv(t, "")
	agent := startAgent(t)
	file := func(cmd string, args ...string) {
		t.Helper()
		if err := cli.File(cmd, append(args, "--project", "vlm", "--new-project", "x"), io.Discard, agent); err != nil {
			t.Fatal(err)
		}
	}
	file("progress", "Judge v2 validated on 117 episodes", "--next", "Re-run the drawer tasks")
	file("review", "LIBERO-plus results: Model A 65.5% vs control 78.6%", "--record", "https://github.com/o/vfp/pull/12", "--why", "Model A loses 13 points.\n\nMore below.")
	file("review", "Try the new tab")
	file("issue", "ARC login expired")
	file("ask", "Retrain or drop Model A?", "--options", "retrain v1.5|drop it")
	e.cycle()
	ids := map[string]string{}
	all, _ := e.s.st.Items("open")
	for _, it := range all {
		ids[it.Title] = it.ID
	}
	e.s.Decide(ids["Try the new tab"], Action{Kind: "dismiss"})
	e.s.Decide(ids["LIBERO-plus results: Model A 65.5% vs control 78.6%"], Action{Kind: "done"})
	if err := e.s.Decide(ids["Retrain or drop Model A?"], Action{Kind: "choose", Option: 0}); err != nil {
		t.Fatal(err)
	}
	e.s.st.AddIdea(store.Idea{ID: "idea-abcdefgh", Project: "vlm", Title: "Results page", Status: store.IdeaBuilt, Note: "Shipped in https://github.com/o/docket/pull/21"})

	// The docket: one fold, closed, after the action items.
	q := getOK(t, e, "/")
	fold := q[strings.Index(q, `data-wt="progress:all"`)-40:]
	if !strings.Contains(fold, `<details class="wt pgf" data-wt="progress:all">`) || !strings.Contains(fold, "1 today") || !strings.Contains(fold, "Judge v2 validated on 117 episodes") || !strings.Contains(fold, "Next: Re-run the drawer tasks") {
		t.Fatalf("progress fold:\n%s", fold)
	}
	if p := getOK(t, e, "/?p=vlm"); !strings.Contains(p, `data-wt="progress:vlm"`) || !strings.Contains(p, `href="/timeline?p=vlm"`) {
		t.Fatal("the project view lacks its fold or its timeline link")
	}

	// Results: the done one is kept, the dismissed one isn't, progress isn't a result.
	res := getOK(t, e, "/timeline?p=vlm&f=result")
	if !strings.Contains(res, "LIBERO-plus results") || strings.Contains(res, "Try the new tab") || strings.Contains(res, "Judge v2") {
		t.Fatalf("results filter:\n%s", res)
	}
	if !strings.Contains(res, "Model A loses 13 points.") || !strings.Contains(res, `<a href="https://github.com/o/vfp/pull/12"`) {
		t.Fatal("a result lacks its summary or record link")
	}
	everything := getOK(t, e, "/timeline?p=vlm")
	for _, want := range []string{"Judge v2 validated", "ARC login expired", "You chose “retrain v1.5”", "Results page", `<a href="https://github.com/o/docket/pull/21"`, "Today"} {
		if !strings.Contains(everything, want) {
			t.Fatalf("timeline lacks %q:\n%s", want, everything)
		}
	}
	if !strings.Contains(getOK(t, e, "/timeline?f=progress"), "Judge v2 validated") {
		t.Fatal("the all-projects progress filter lacks the entry")
	}
	if b := getOK(t, e, "/timeline?p=nope"); !strings.Contains(b, "Nothing here yet") {
		t.Fatal("an empty timeline doesn't say so")
	}

	// A new progress entry changes the page version, so open pages refresh.
	v := e.s.st.Version()
	file("progress", "Drawer tasks re-run")
	e.cycle()
	if e.s.st.Version() == v {
		t.Fatal("posting progress didn't change the version")
	}
}

// Paging never loses an entry, even when many share the second a page ends
// in: the page takes that whole second, and the next starts before it.
func TestTimelineOlderPages(t *testing.T) {
	e := newEnv(t, "")
	base := time.Now().Add(-48 * time.Hour).Unix()
	n := timelineLimit + 40
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dk-p%07d", i)
		// Twenty entries a second, so page edges fall inside seconds.
		e.s.st.UpsertItem(store.Item{ID: id, Kind: "progress", Status: store.StatusLogged, Title: "step " + id, Project: "vlm", CreatedAt: base + int64(i/20)})
	}
	seen := map[string]bool{}
	path, pages := "/timeline?p=vlm", 0
	for path != "" {
		pages++
		if pages > 5 {
			t.Fatal("paging doesn't end")
		}
		b := getOK(t, e, path)
		for _, m := range regexp.MustCompile(`data-open="(dk-p\d+)"`).FindAllStringSubmatch(b, -1) {
			if seen[m[1]] {
				t.Fatalf("%s on two pages", m[1])
			}
			seen[m[1]] = true
		}
		path = ""
		if m := regexp.MustCompile(`href="(/timeline\?[^"]*before=[^"]*)"`).FindStringSubmatch(b); m != nil {
			path = strings.ReplaceAll(m[1], "&amp;", "&")
			if u, _ := url.Parse(path); u.Query().Get("p") != "vlm" {
				t.Fatalf("older link %q lost the project", path)
			}
		}
	}
	if len(seen) != n || pages != 2 {
		t.Fatalf("saw %d of %d entries on %d pages", len(seen), n, pages)
	}
}

func TestAnswerChoice(t *testing.T) {
	for _, c := range []struct{ answer, want string }{
		{"Chose: Build it.", "Build it"},
		{"Chose: raise to 10.5. thanks", "raise to 10.5"},
		{"Chose: raise to 10. thanks", "raise to 10"},
		{"Approved.", ""},
	} {
		if got := store.AnswerChoice(c.answer, []string{"Build it", "raise to 10", "raise to 10.5"}); got != c.want {
			t.Errorf("AnswerChoice(%q) = %q, want %q", c.answer, got, c.want)
		}
	}
}

// More entries than a page in one second: the page holds the whole second
// (it can't be split), and there's nothing older.
func TestTimelineOneCrowdedSecond(t *testing.T) {
	e := newEnv(t, "")
	at := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < timelineLimit+10; i++ {
		e.s.st.UpsertItem(store.Item{ID: fmt.Sprintf("dk-c%07d", i), Kind: "progress", Status: store.StatusLogged, Title: "bulk", Project: "vlm", CreatedAt: at})
	}
	e.s.st.UpsertItem(store.Item{ID: "dk-older001", Kind: "progress", Status: store.StatusLogged, Title: "older", Project: "vlm", CreatedAt: at - 60})
	entries, more, err := e.s.st.Timeline("vlm", "", 0, timelineLimit)
	if err != nil || len(entries) != timelineLimit+10 || !more {
		t.Fatalf("first page: %d entries, more=%v, err=%v", len(entries), more, err)
	}
	rest, more, _ := e.s.st.Timeline("vlm", "", entries[len(entries)-1].At, timelineLimit)
	if len(rest) != 1 || rest[0].Item.ID != "dk-older001" || more {
		t.Fatalf("second page: %+v more=%v", rest, more)
	}
}

// The timeline keeps action items, done or not, so a past one is easy to
// find: each says how it ended, and one dismissed isn't kept.
func TestTimelineKeepsActionItems(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	agent := func(id, kind, title string) {
		t.Helper()
		st.UpsertItem(store.Item{ID: id, Kind: kind, Title: title, Body: "Why: " + title, Project: "vlm", SessionID: "s", Machine: "box", Source: "cli", Options: []string{"Tonight", "Tomorrow"}})
	}
	agent("dk-ta000001", "human_task", "Plug the wrist camera back in")
	agent("dk-ta000002", "ask", "Deploy tonight?")
	agent("dk-ta000003", "human_task", "Swap the gripper pads")
	agent("dk-ta000004", "ask", "Which judge prompt?")
	agent("dk-ta000005", "approval", "Spend $40 on GPU time?")
	agent("dk-ta000006", "issue", "ARC login expired")
	st.UpsertItem(store.Item{ID: "dk-ta000007", Kind: "ask", Title: "Order more cubes?", Project: "vlm", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	st.UpsertItem(store.Item{ID: "dk-ta000008", Kind: "review", Title: "Ablation results", Project: "vlm"})
	for id, a := range map[string]Action{
		"dk-ta000001": {Kind: "done", Text: "reseated it"},
		"dk-ta000002": {Kind: "choose", Option: 0},
		"dk-ta000003": {Kind: "dismiss"},
		"dk-ta000006": {Kind: "done"},
	} {
		if err := e.s.Decide(id, a); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	st.SetStatus("dk-ta000005", "done", "approved in chat", "")
	if n, err := st.Expire(time.Now().Unix()); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}

	entry := func(page, id string) string {
		t.Helper()
		m := regexp.MustCompile(`(?s)<li class="tl-e [^"]*">(?:(?:[^<]|<[^/]|</[^l]|</l[^i])*?)data-open="` + id + `".*?</li>`).FindString(page)
		if m == "" {
			t.Fatalf("no timeline entry for %s:\n%s", id, page)
		}
		return m
	}
	acts := getOK(t, e, "/timeline?p=vlm&f=action")
	for id, want := range map[string]string{
		"dk-ta000001": "You marked it done: reseated it",
		"dk-ta000002": "You chose “Tonight”, waiting on the agent",
		"dk-ta000004": "on your docket",
		"dk-ta000005": "The agent closed it: approved in chat",
		"dk-ta000007": "Expired: no longer relevant after its expiry time ·",
	} {
		if en := entry(acts, id); !strings.Contains(en, want) {
			t.Errorf("%s lacks %q:\n%s", id, want, en)
		}
	}
	if en := entry(acts, "dk-ta000001"); !strings.Contains(en, `class="chip k-do"`) || !strings.Contains(en, "Why: Plug the wrist camera") || !strings.Contains(en, "reseated it · just now") || strings.Contains(en, `class="tl-m"`) {
		t.Errorf("a done task's entry: %s", en)
	}
	if !strings.Contains(entry(acts, "dk-ta000007"), "#i-clock") || !strings.Contains(entry(acts, "dk-ta000001"), "#i-check") {
		t.Error("an expired item should show a clock, a done one a check")
	}
	if en := entry(acts, "dk-ta000004"); !strings.Contains(en, `class="chip k-decide"`) || strings.Contains(en, "tl-out") {
		t.Errorf("an open ask's entry: %s", en)
	}
	for _, id := range []string{"dk-ta000003", "dk-ta000006", "dk-ta000008"} {
		if strings.Contains(acts, `data-open="`+id+`"`) {
			t.Errorf("the Action items filter shows %s", id)
		}
	}
	if !strings.Contains(acts, ">Action items</a>") {
		t.Error("no Action items chip")
	}
	// Everything: the problem marked done says so too; the dismissed task isn't kept.
	all := getOK(t, e, "/timeline?p=vlm")
	if en := entry(all, "dk-ta000006"); !strings.Contains(en, "You marked it done") {
		t.Errorf("a problem marked done: %s", en)
	}
	if strings.Contains(all, `data-open="dk-ta000003"`) || !strings.Contains(all, `data-open="dk-ta000001"`) {
		t.Error("the timeline keeps the dismissed task, or drops the done one")
	}
}
