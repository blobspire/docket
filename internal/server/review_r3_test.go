package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/blobspire/docket/internal/agentctx"
	"github.com/blobspire/docket/internal/ingest"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// undoing an action withdraws the delivery that action queued.
func TestUndoWithdrawsWhatItQueued(t *testing.T) {
	cases := []struct {
		kind string
		a    Action
	}{
		{"approval", Action{Kind: "approve"}},
		{"approval", Action{Kind: "decline"}},
		{"ask", Action{Kind: "answer", Text: "room 2"}},
		{"choice", Action{Kind: "choose", Option: 0}},
		{"human_task", Action{Kind: "done"}},
		{"ask", Action{Kind: "noise"}},
	}
	for _, c := range cases {
		e := newEnv(t, "")
		st := e.s.st
		st.UpsertItem(store.Item{ID: "dk-r3u00001", Kind: c.kind, Title: "go?", SessionID: "s", Machine: "box", Source: "cli", Options: []string{"a", "b"}})
		if err := e.s.Decide("dk-r3u00001", c.a); err != nil {
			t.Fatalf("%s: %v", c.a.Kind, err)
		}
		if ds, _ := st.ItemDeliveries("dk-r3u00001"); len(ds) != 1 {
			t.Fatalf("%s queued %d deliveries", c.a.Kind, len(ds))
		}
		if err := e.s.Decide("dk-r3u00001", Action{Kind: "undo"}); err != nil {
			t.Fatalf("undo %s: %v", c.a.Kind, err)
		}
		if ds, _ := st.ItemDeliveries("dk-r3u00001"); len(ds) != 0 {
			t.Errorf("undone %s still queued for the agent: %q", c.a.Kind, ds[0].Text)
		}
	}
}

// two racing closes can't both land (store-level, below Decide's check).
func TestSecondCloseIsRefused(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-r3s00001", Kind: "issue", Title: "x"})
	if err := st.SetStatus("dk-r3s00001", "done", "", "done"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("dk-r3s00001", "dismissed", "", "dismiss"); err == nil {
		t.Fatal("a second close of a closed item was accepted")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// a push that never reached ntfy (stuck connecting) is not counted as sent.
func TestUnsentPushTimeoutRetries(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = rtFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	defer func() { http.DefaultTransport = old }()
	e := pushEnv(t, "http://ntfy.invalid")
	e.s.st.UpsertItem(store.Item{ID: "dk-r3n00001", Kind: "issue", Title: "down", Urgency: "now", UrgentSince: time.Now().Add(-time.Hour).Unix()})
	e.s.pushes(e.ctx)
	if it, _, _ := e.s.st.GetItem("dk-r3n00001"); it.PushedAt != 0 {
		t.Fatal("a push that never left was marked sent; it will never retry")
	}
}

// a human moving one keyed item doesn't swallow the session's other keys.
func TestMovedItemOnlyTakesItsOwnKey(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s", record.Item{ID: "dk-r3m00001", Kind: "issue", Title: "cert expiring", Key: "cert", Project: "site"})
	e.s.Decide("dk-r3m00001", Action{Kind: "reclassify", Project: "ops"})
	e.file(t, "s", record.Item{ID: "dk-r3m00002", Kind: "issue", Title: "dns broken", Key: "dns", Project: "site"})
	if a := active(t, e.s.st); len(a) != 2 {
		t.Fatalf("a new key merged into the moved item: %+v", a)
	}
}

// an answer to a re-filed item names the id this session was told.
func TestAnswerNamesTheIdThisSessionWasTold(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s1", record.Item{ID: "dk-r3a00001", Kind: "ask", Title: "db?", Key: "db", Project: "w"})
	e.file(t, "s1", record.Item{ID: "dk-r3a00002", Kind: "ask", Title: "db? (still)", Key: "db", Project: "w"})
	e.s.Decide("dk-r3a00001", Action{Kind: "answer", Text: "sqlite"})
	ds, _ := e.s.st.ItemDeliveries("dk-r3a00001")
	if len(ds) != 1 || !contains(ds[0].Text, "filed as dk-r3a00002") {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// an idle_prompt while a permission dialog is open keeps the session waiting.
func TestIdlePromptKeepsAnOpenDialog(t *testing.T) {
	e := newEnv(t, "")
	b1, _ := json.Marshal(map[string]any{"session_id": "L1", "notification_type": "permission_prompt"})
	e.send(t, record.Record{Event: "Notification", SessionID: "L1", Payload: b1}, ingest.Options{})
	b2, _ := json.Marshal(map[string]any{"session_id": "L1", "notification_type": "idle_prompt"})
	e.send(t, record.Record{Event: "Notification", SessionID: "L1", Payload: b2}, ingest.Options{})
	if s, _, _ := e.s.st.GetSession("L1"); s.State != "waiting" || s.WaitingFor == "" {
		t.Fatalf("an idle_prompt cleared an open permission dialog: %+v", s)
	}
}

// the "move to" select can't drop an item into the unsorted bucket by name.
func TestReclassifyIntoUnsortedRefused(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-r3d00001", Kind: "issue", Title: "x", Project: "w"})
	if err := e.s.Decide("dk-r3d00001", Action{Kind: "reclassify", Project: store.Unsorted}); err == nil {
		t.Fatal("moved into unsorted")
	}
}

// the seen window covers the overlay's whole retention, or an entry
// the server closed comes back on the machine.
func TestSeenCoversRetention(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-r3w00001", Kind: "ask", Title: "x", Machine: "box", CreatedAt: time.Now().Add(-agentctx.Retention).Unix()})
	data, _ := e.s.ContextFor("box")
	var c agentctx.Context
	json.Unmarshal(data, &c)
	if !contains(","+joinIDs(c.Seen)+",", ",dk-r3w00001,") {
		t.Fatalf("seen = %v", c.Seen)
	}
}

func joinIDs(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestMovedKeyLookupStaysInItsProject(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	e.file(t, "s-m1", record.Item{ID: "dk-mk000001", Kind: "review", Title: "API CI fix", Key: "ci", Project: "api"})
	e.s.Decide("dk-mk000001", Action{Kind: "reclassify", Project: "platform"})
	// Another session, another project, same key: its own item.
	e.file(t, "s-m2", record.Item{ID: "dk-mk000002", Kind: "review", Title: "Tools CI fix", Key: "ci", Project: "tools"})
	if a := active(t, st); len(a) != 2 {
		t.Fatalf("a filing in tools merged into the moved api item: %+v", a)
	}
	// done --key with a project that isn't where the moved item is leaves it.
	e.send(t, record.Record{Event: "cli.done", SessionID: "s-m3", Done: &record.Done{Key: "ci", Project: "ops", Explicit: true}}, ingest.Options{})
	if it, _, _ := st.GetItem("dk-mk000001"); it.Status != "open" {
		t.Fatalf("done --key --project ops closed the platform item")
	}
	// In the project it was moved to, the key still reaches it.
	e.file(t, "s-m1", record.Item{ID: "dk-mk000003", Kind: "review", Title: "API CI fix, v2", Key: "ci", Project: "platform"})
	if it, _, _ := st.GetItem("dk-mk000001"); it.Title != "API CI fix, v2" {
		t.Fatalf("a filing in the new project didn't reach the moved item: %q", it.Title)
	}
}

func TestUndoKeepsAnEarlierAnswerGoing(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-ue000001", Kind: "ask", Title: "rename?", SessionID: "s", Machine: "box", Source: "cli"})
	e.s.Decide("dk-ue000001", Action{Kind: "answer", Text: "yes, v2"})
	st.DB.Exec(`UPDATE deliveries SET created_at=created_at-10 WHERE item_id='dk-ue000001'`)
	st.DB.Exec(`UPDATE decisions SET ts=ts-10 WHERE item_id='dk-ue000001'`)
	st.DB.Exec(`UPDATE items SET status='open' WHERE id='dk-ue000001'`) // a later re-file reopened it
	e.s.Decide("dk-ue000001", Action{Kind: "noise"})
	e.s.Decide("dk-ue000001", Action{Kind: "undo"})
	ds, _ := st.ItemDeliveries("dk-ue000001")
	if len(ds) != 1 || !strings.Contains(ds[0].Text, "yes, v2") {
		t.Fatalf("undoing the noise withdrew the earlier answer: %+v", ds)
	}
}

func TestAnAnswerCantReopenAClosedItem(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-ar000001", Kind: "ask", Title: "q"})
	st.SetStatus("dk-ar000001", "dismissed", "", "dismiss")
	if err := st.SetAnswer("dk-ar000001", "yes", "answer"); err == nil {
		t.Fatal("an answer landed on a dismissed item")
	}
}

func TestLegacyKeyNamedLikeTheProjectGetsIt(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-lp000001", Kind: "ask", Title: "ci?", Source: "cli", Machine: "box", DedupeKey: "key:shared:ci"})
	e.file(t, "s", record.Item{ID: "dk-lp000002", Kind: "ask", Title: "ci? v2", Key: "ci", Project: "shared"})
	if it, _, _ := st.GetItem("dk-lp000001"); it.Project != "shared" || it.Title != "ci? v2" {
		t.Fatalf("legacy item = %+v", it)
	}
}

func TestAMergedItemsPageShowsItself(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.EnsureProject("tools", "")
	st.EnsureProject("robot", "")
	e.file(t, "a", record.Item{ID: "dk-pg000001", Kind: "ask", Title: "from tools", Key: "k", Project: "tools"})
	e.file(t, "b", record.Item{ID: "dk-pg000002", Kind: "ask", Title: "from robot", Key: "k", Project: "robot"})
	st.RenameProject("tools", "robot")
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/item/dk-pg000001", nil))
	if !strings.Contains(rec.Body.String(), "from tools") {
		t.Fatal("the merged item's page shows the survivor instead")
	}
}

func TestUpgradeClosesOldPerSessionSignInItems(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	st.UpsertItem(store.Item{ID: "dk-up000001", Kind: "health", Title: "signed out", Source: "stop_failure", Machine: "mac", SessionID: "sQ", DedupeKey: "auth:mac:auth_failed"})
	st.DB.Exec(`DELETE FROM kv WHERE k='upgrade:agents-file'`)
	again, _ := store.Open(st.Path())
	again.Close()
	if it, _, _ := st.GetItem("dk-up000001"); it.Status != "dismissed" {
		t.Fatalf("an old per-session sign-in item survived the upgrade: %s", it.Status)
	}
}
