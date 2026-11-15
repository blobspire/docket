package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/agentctx"
	"github.com/blobspire/docket/internal/store"
)

func TestNotWorthMyTime(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.PatchSession("live", store.SessionPatch{Machine: store.Str("box"), State: store.Str("idle"), HasPlugin: store.Bool(true)})
	st.UpsertItem(store.Item{ID: "dk-n0000001", Kind: "review", Title: "PR #165 merged: review report", Project: "vlm-teacher", Source: "cli", Machine: "box", SessionID: "live"})
	if err := e.s.Decide("dk-n0000001", Action{Kind: "noise", Text: "Merged-PR reports don't need me."}); err != nil {
		t.Fatal(err)
	}
	it, _, _ := st.GetItem("dk-n0000001")
	if it.Status != "dismissed" || !strings.Contains(it.Resolution, "Merged-PR reports don't need me.") {
		t.Fatalf("item = %+v", it)
	}
	// The filing session hears why, without being woken or resumed.
	ds, _ := st.ItemDeliveries("dk-n0000001")
	if len(ds) != 1 || !ds[0].LiveOnly || !ds[0].Quiet || !strings.Contains(ds[0].Text, "Merged-PR reports don't need me.") {
		t.Fatalf("deliveries = %+v", ds)
	}
	// Every agent sees it next to the rules.
	e.s.syncRules(e.ctx)
	md, _ := os.ReadFile(filepath.Join(os.Getenv("DOCKET_HOME"), "rules.md"))
	if !strings.Contains(string(md), "Recently marked not worth Cole's time") || !strings.Contains(string(md), "PR #165 merged: review report (vlm-teacher): Merged-PR reports don't need me.") {
		t.Fatalf("rules.md = %s", md)
	}
	// Undo brings it back and takes it out of the guidance.
	if err := e.s.Decide("dk-n0000001", Action{Kind: "undo"}); err != nil {
		t.Fatal(err)
	}
	if it, _, _ := st.GetItem("dk-n0000001"); it.Status != "open" {
		t.Fatalf("after undo: %s", it.Status)
	}
	if strings.Contains(e.s.RulesMarkdown(), "PR #165") {
		t.Fatal("an undone mark is still in rules.md")
	}
}

func TestLiveOnlyNeverResumes(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[machine]]
name = "laptop"
mode = "push"
`)
	st := e.s.st
	st.PatchSession("gone", store.SessionPatch{Machine: store.Str("box"), State: store.Str("ended"), HasPlugin: store.Bool(true), Cwd: store.Str("/tmp")})
	st.AddDelivery(store.Delivery{ID: "d1", Machine: "box", SessionID: "gone", Nonce: "n1", Text: "[docket #n1] x", LiveOnly: true})
	e.s.deliverAll(e.ctx)
	if ds, _ := st.Deliveries("dropped"); len(ds) != 1 {
		t.Fatalf("a live-only delivery to an ended session wasn't dropped: %+v", ds)
	}
	// On a laptop too, and for a session without the plugin.
	st.PatchSession("old", store.SessionPatch{Machine: store.Str("laptop"), State: store.Str("idle")})
	st.AddDelivery(store.Delivery{ID: "d2", Machine: "laptop", SessionID: "old", Nonce: "n2", Text: "[docket #n2] x", LiveOnly: true})
	reply, err := e.s.Receive("laptop", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: "-", Context: "-"})
	if err != nil || len(reply.Deliveries) != 0 {
		t.Fatalf("reply = %+v %v", reply, err)
	}
	if ds, _ := st.Deliveries("dropped"); len(ds) != 2 {
		t.Fatalf("dropped = %+v", ds)
	}
}

func TestContextReachesMachines(t *testing.T) {
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[[machine]]
name = "laptop"
mode = "push"
[[lead]]
name = "robotics"
machine = "box"
dir = "/w/robot"
project = "vlm-teacher"
`)
	st := e.s.st
	st.EnsureProject("vlm-teacher", "Robots")
	st.EnsureProject("admin", "Admin")
	st.SetDir("box", "/w/robot", "vlm-teacher")
	st.UpsertItem(store.Item{ID: "dk-c0000001", Kind: "ask", Title: "Mine", Project: "vlm-teacher", Component: "FR5", Place: "lab", Machine: "box", DedupeKey: "key:vlm-teacher:arm", Source: "cli"})
	st.UpsertItem(store.Item{ID: "dk-c0000002", Kind: "ask", Title: "The laptop's", Project: "admin", Machine: "laptop", Source: "cli"})
	st.UpsertItem(store.Item{ID: "dk-c0000003", Kind: "health", Title: "box is unreachable", Project: "system", Machine: "box", Source: "watchdog"})
	e.s.syncContext(e.ctx, e.m)
	c, ok := agentctx.Load()
	if !ok || !c.HasProject("vlm-teacher") || !c.HasProject("admin") || c.Leads["robotics"] != "vlm-teacher" || len(c.Dirs) != 1 {
		t.Fatalf("context = %+v", c)
	}
	if len(c.Open) != 1 || c.Open[0].ID != "dk-c0000001" || c.Open[0].Key != "arm" {
		t.Fatalf("a machine must only see its own agent-filed items: %+v", c.Open)
	}
	if len(c.Places) != 1 || c.Places[0] != "lab" {
		t.Fatalf("places = %v", c.Places)
	}
	// Unchanged content is byte-identical, so it isn't resent.
	a, _ := e.s.ContextFor("box")
	b, _ := e.s.ContextFor("box")
	if string(a) != string(b) {
		t.Fatal("context isn't deterministic")
	}
	// A laptop gets it only when its copy differs.
	reply, _ := e.s.Receive("laptop", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: "", Context: ""})
	if reply.Context == nil || reply.Rules == nil {
		t.Fatalf("reply = %+v", reply)
	}
	var lc agentctx.Context
	json.Unmarshal([]byte(*reply.Context), &lc)
	if len(lc.Open) != 1 || lc.Open[0].ID != "dk-c0000002" {
		t.Fatalf("laptop context = %+v", lc)
	}
	reply, _ = e.s.Receive("laptop", strings.NewReader(`{"_cursor":"","_more":false}`+"\n"), nil, Hashes{Rules: agentctx.Hash([]byte(*reply.Rules)), Context: agentctx.Hash([]byte(*reply.Context))})
	if reply.Context != nil || reply.Rules != nil {
		t.Fatal("resent an unchanged context or rules")
	}
}

func TestDashboardViews(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("cwant", "Trading")
	st.EnsureProject("vlm-teacher", "Robots")
	st.UpsertItem(store.Item{ID: "dk-v0000001", Kind: "issue", Title: "ARC login expired", Project: "vlm-teacher", Urgency: "now", Component: "training"})
	st.UpsertItem(store.Item{ID: "dk-v0000002", Kind: "human_task", Title: "Swap the gripper pads", Project: "vlm-teacher", Component: "FR5", Place: "tealab", Due: "2030-01-02"})
	st.UpsertItem(store.Item{ID: "dk-v0000003", Kind: "human_task", Title: "Measure the tool", Project: "vlm-teacher", Component: "FR5", Due: "2030-01-01"})
	st.UpsertItem(store.Item{ID: "dk-v0000004", Kind: "review", Title: "Model A results", Project: "vlm-teacher", Component: "Model A", Body: "**Result:** 65%\n\n- seed 1000\n\n<script>alert(1)</script> [bad](javascript:alert(1))"})
	st.UpsertItem(store.Item{ID: "dk-v0000005", Kind: "review", Title: "Send the ProphetX reply", Project: "cwant"})
	st.UpsertItem(store.Item{ID: "dk-v0000006", Kind: "rule", Title: "A proposed rule", Project: "cwant"})
	get := func(path string) string {
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	all := get("/")
	if !strings.Contains(all, "band-now") || !strings.Contains(all, "ARC login expired") || !strings.Contains(all, "Send the ProphetX reply") {
		t.Fatal("the All view is missing the act-now band or a project")
	}
	if strings.Contains(all, "A proposed rule") {
		t.Fatal("rule proposals belong on the Brain page, not the dashboard")
	}
	if strings.Index(all, ">cwant<") > strings.Index(all, ">vlm-teacher<") {
		t.Fatal("projects aren't in alphabetical order")
	}
	// Markdown renders, and agent-written HTML and script links don't.
	if !strings.Contains(all, "<strong>Result:</strong>") || !strings.Contains(all, "<li>seed 1000</li>") {
		t.Fatal("markdown didn't render")
	}
	if strings.Contains(all, "<script>alert") || strings.Contains(all, `href="javascript:`) {
		t.Fatal("unsafe markdown got through")
	}
	proj := get("/?p=vlm-teacher")
	if strings.Contains(proj, "Send the ProphetX reply") || !strings.Contains(proj, "<h3>FR5</h3>") {
		t.Fatal("the project view shows other projects or lacks component groups")
	}
	place := get("/?place=tealab")
	if !strings.Contains(place, "Swap the gripper pads") || strings.Contains(place, "Measure the tool") {
		t.Fatal("the place view is wrong")
	}
	byDeadline := get("/?p=vlm-teacher&sort=deadline")
	if strings.Index(byDeadline, "Measure the tool") > strings.Index(byDeadline, "Swap the gripper pads") {
		t.Fatal("deadline sort isn't soonest first")
	}
	brain := get("/brain")
	if !strings.Contains(brain, "A proposed rule") {
		t.Fatal("rule proposals are missing from the Brain page")
	}
}

func TestProjectRenameMovesItemsAndKeys(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("robots", "")
	st.EnsureProject("vlm-teacher", "Robots")
	st.SetDir("box", "/w/robot", "robots")
	st.UpsertItem(store.Item{ID: "dk-r0000001", Kind: "ask", Title: "a", Project: "robots", DedupeKey: "key:robots:arm"})
	st.UpsertItem(store.Item{ID: "dk-r0000002", Kind: "ask", Title: "b", Project: "robots", DedupeKey: "key:robots:dup"})
	st.UpsertItem(store.Item{ID: "dk-r0000003", Kind: "ask", Title: "c", Project: "vlm-teacher", DedupeKey: "key:vlm-teacher:dup"})
	if err := st.RenameProject("robots", "vlm-teacher"); err != nil {
		t.Fatal(err)
	}
	a, _, _ := st.GetItem("dk-r0000001")
	b, _, _ := st.GetItem("dk-r0000002")
	if a.Project != "vlm-teacher" || a.DedupeKey != "key:vlm-teacher:arm" || b.Status != "done" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if _, ok := st.GetProject("robots"); ok {
		t.Fatal("the old project still exists")
	}
	dirs, _ := st.ProjectDirs("box")
	if len(dirs) != 1 || dirs[0].Project != "vlm-teacher" {
		t.Fatalf("dirs = %+v", dirs)
	}
}

func TestExpiryAndWatchdogSnooze(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-e0000001", Kind: "ask", Title: "Launch before Friday?", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	st.UpsertItem(store.Item{ID: "dk-e0000002", Kind: "ask", Title: "No expiry"})
	e.s.chiefTick(e.ctx)
	a, _, _ := st.GetItem("dk-e0000001")
	b, _, _ := st.GetItem("dk-e0000002")
	if a.Status != "expired" || a.ResolvedAt == 0 || b.Status != "open" {
		t.Fatalf("a=%s b=%s", a.Status, b.Status)
	}
	// Dismissing a watchdog item hides it for a day instead of re-opening it on the next check.
	e.s.watch(store.Item{ID: "dk-e0000003", Kind: "health", Source: "watchdog", Title: "quota", DedupeKey: "quota:x", Ongoing: true})
	e.s.Decide("dk-e0000003", Action{Kind: "dismiss"})
	e.s.watch(store.Item{ID: "dk-e0000004", Kind: "health", Source: "watchdog", Title: "quota", DedupeKey: "quota:x", Ongoing: true})
	if items, _ := st.Items("open"); len(items) != 1 {
		t.Fatalf("open = %+v", items)
	}
	if c, _, _ := st.GetItem("dk-e0000003"); c.Status != "snoozed" {
		t.Fatalf("status = %s", c.Status)
	}
}

func TestMigrateToleratesAConcurrentMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	s1, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := store.Open(path) // all columns exist already
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}
