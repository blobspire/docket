package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "docket.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIngestIsIdempotentAndAtomic(t *testing.T) {
	s := open(t)
	evs := []RawEvent{{ID: "a", Event: "Stop", Body: []byte(`{"id":"a"}`)}, {ID: "b", Event: "Stop", Body: []byte(`{"id":"b"}`)}}
	n, err := s.IngestBatch("lab", evs, "seg-1.jsonl:10")
	if err != nil || n != 2 {
		t.Fatalf("first ingest n=%d err=%v", n, err)
	}
	n, err = s.IngestBatch("lab", evs, "seg-1.jsonl:10") // re-delivered after a lost ack
	if err != nil || n != 0 {
		t.Fatalf("re-ingest n=%d err=%v", n, err)
	}
	if c, _ := s.Cursor("lab"); c != "seg-1.jsonl:10" {
		t.Fatalf("cursor = %q", c)
	}
	todo, _ := s.Unprocessed(10)
	if len(todo) != 2 || todo[0].ID != "a" {
		t.Fatalf("unprocessed = %+v", todo)
	}
	s.MarkProcessed("a")
	todo, _ = s.Unprocessed(10)
	if len(todo) != 1 {
		t.Fatalf("after mark: %d", len(todo))
	}
}

func TestDedupeCountsMentionsWhileActive(t *testing.T) {
	s := open(t)
	id1, new1, err := s.UpsertItem(Item{ID: "dk-1", Kind: "approval", Title: "run x", DedupeKey: "perm:s:x"})
	if err != nil || !new1 {
		t.Fatal(err)
	}
	id2, new2, _ := s.UpsertItem(Item{ID: "dk-2", Kind: "approval", Title: "run x again", DedupeKey: "perm:s:x"})
	if new2 || id2 != id1 {
		t.Fatalf("expected a mention of %s, got new=%v id=%s", id1, new2, id2)
	}
	it, _, _ := s.GetItem(id1)
	if it.MentionCount != 2 || it.Title != "run x again" {
		t.Fatalf("item = %+v", it)
	}
	// Once resolved, the same key opens a fresh item.
	s.SetStatus(id1, "done", "ok", "done")
	id3, new3, _ := s.UpsertItem(Item{ID: "dk-3", Kind: "approval", Title: "run x", DedupeKey: "perm:s:x"})
	if !new3 || id3 != "dk-3" {
		t.Fatalf("after resolve: new=%v id=%s", new3, id3)
	}
}

func TestOngoingRecheckIsNotAMention(t *testing.T) {
	s := open(t)
	id, _, _ := s.UpsertItem(Item{ID: "dk-1", Kind: "blocker", Title: "waiting", DedupeKey: "waiting:s1", Ongoing: true})
	for i := 0; i < 5; i++ {
		s.UpsertItem(Item{ID: "dk-x", Kind: "blocker", Title: "still waiting", DedupeKey: "waiting:s1", Ongoing: true})
	}
	it, _, _ := s.GetItem(id)
	if it.MentionCount != 1 || it.Title != "still waiting" {
		t.Fatalf("item = %+v", it)
	}
}

func TestReingestedItemIsNotDuplicated(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "dk-9", Kind: "ask", Title: "q"})
	_, isNew, err := s.UpsertItem(Item{ID: "dk-9", Kind: "ask", Title: "q"})
	if err != nil || isNew {
		t.Fatalf("re-ingest created a duplicate: new=%v err=%v", isNew, err)
	}
}

func TestUndoRestoresPreviousStatus(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "dk-1", Kind: "ask", Title: "q"})
	s.SetStatus("dk-1", "dismissed", "", "dismiss")
	if a, err := s.Undo("dk-1"); err != nil || a != "dismiss" {
		t.Fatalf("undo: %v %v", a, err)
	}
	it, _, _ := s.GetItem("dk-1")
	if it.Status != "open" || it.ResolvedAt != 0 {
		t.Fatalf("after undo: %+v", it)
	}
	if _, err := s.Undo("dk-1"); err == nil {
		t.Fatal("second undo should have nothing to undo")
	}
}

func TestUndoRefusesAfterDelivery(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "dk-1", Kind: "ask", Title: "q", SessionID: "s"})
	s.SetAnswer("dk-1", "yes", "answer")
	s.AddDelivery(Delivery{ID: "d1", ItemID: "dk-1", Machine: "m", SessionID: "s", Text: "answer " + AnswerMark + " yes", Nonce: "n1"})
	s.DeliverySent("d1", "mailbox")
	if _, err := s.Undo("dk-1"); err == nil {
		t.Fatal("undo of a delivered answer should be refused")
	}
	if ok, err := s.ConfirmDelivery("n1"); !ok || err != nil {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	it, _, _ := s.GetItem("dk-1")
	if it.Status != "delivered" {
		t.Fatalf("status = %s", it.Status)
	}
}

func TestResolveBySessionOnlyTouchesNamedSources(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "p", Kind: "blocker", Title: "permission", SessionID: "s1", Source: "notification"})
	s.UpsertItem(Item{ID: "q", Kind: "ask", Title: "question", SessionID: "s1", Source: "cli"})
	n, _ := s.ResolveBySession("s1", "session moved on", "notification")
	if n != 1 {
		t.Fatalf("resolved %d", n)
	}
	q, _, _ := s.GetItem("q")
	if q.Status != "open" {
		t.Fatal("an agent's question was closed by session activity")
	}
}

func TestSnoozeAndWake(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "dk-1", Kind: "ask", Title: "q"})
	s.Snooze("dk-1", time.Now().Add(-time.Second).Unix())
	if n, _ := s.Wake(); n != 1 {
		t.Fatalf("woke %d", n)
	}
	it, _, _ := s.GetItem("dk-1")
	if it.Status != "open" {
		t.Fatalf("status %s", it.Status)
	}
}

func TestPatchSession(t *testing.T) {
	s := open(t)
	s.PatchSession("s1", SessionPatch{Machine: Str("lab"), State: Str("busy"), HasPlugin: Bool(true)})
	s.PatchSession("s1", SessionPatch{State: Str("idle")})
	x, ok, err := s.GetSession("s1")
	if !ok || err != nil || x.Machine != "lab" || x.State != "idle" || !x.HasPlugin {
		t.Fatalf("session = %+v ok=%v err=%v", x, ok, err)
	}
}

func TestSearch(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "a", Kind: "ask", Title: "Re-fine-tune sept7 checkpoint?", Body: "the old run diverged"})
	s.UpsertItem(Item{ID: "b", Kind: "review", Title: "Basket knock-over demo", Body: "t3 demo video"})
	s.SetAnswer("a", "No, reuse the existing sept7 weights", "answer")
	got, err := s.Search("sept7", 10)
	if err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("search sept7: %+v %v", got, err)
	}
	got, _ = s.Search("reuse weights", 10) // matches the answer
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("search answer text: %+v", got)
	}
	got, _ = s.Search(`bask"et*) (`, 10) // hostile syntax must not error
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("sanitized search: %+v", got)
	}
}

func TestMigrateAddsColumnsToOldDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The sessions table as the first deployed version created it.
	old.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, machine TEXT, cwd TEXT, lead TEXT, name TEXT, tmux TEXT, bridge_id TEXT,
		pid INTEGER, state TEXT, waiting_for TEXT, transcript_path TEXT, started_at INTEGER, last_event_at INTEGER, ended_at INTEGER,
		pending_permission TEXT, last_message TEXT, has_plugin INTEGER NOT NULL DEFAULT 0, nudges INTEGER NOT NULL DEFAULT 0,
		nudge_hits INTEGER NOT NULL DEFAULT 0)`)
	old.Exec(`CREATE TABLE decisions (id INTEGER PRIMARY KEY AUTOINCREMENT, item_id TEXT NOT NULL, ts INTEGER NOT NULL,
		action TEXT NOT NULL, answer TEXT NOT NULL DEFAULT '', prev_status TEXT NOT NULL DEFAULT '')`)
	old.Exec(`INSERT INTO sessions(id, machine) VALUES ('s1', 'lab')`)
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PatchSession("s1", SessionPatch{WaitingSince: Int(5), Kind: Str("interactive")}); err != nil {
		t.Fatal(err)
	}
	x, ok, err := s.GetSession("s1")
	if err != nil || !ok || x.WaitingSince != 5 || x.Kind != "interactive" || x.Machine != "lab" {
		t.Fatalf("got %+v %v %v", x, ok, err)
	}
}

func TestExactAgentKeys(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "dk-1", Kind: "ask", Title: "a", DedupeKey: "key:p:build:x", SessionID: "s"})
	s.UpsertItem(Item{ID: "dk-2", Kind: "ask", Title: "b", DedupeKey: "key:p:Deploy", SessionID: "s"})
	s.UpsertItem(Item{ID: "dk-3", Kind: "ask", Title: "c", DedupeKey: `key:p:ab`, SessionID: "s"})
	for _, k := range []string{"x", "deploy", `a\b`, "a_b", "%"} {
		if it, ok := s.ActiveByAgentKey(k, "s", ""); ok {
			t.Errorf("--key %q matched %s (%s)", k, it.ID, it.DedupeKey)
		}
	}
	if it, ok := s.ActiveByAgentKey("build:x", "s", ""); !ok || it.ID != "dk-1" {
		t.Errorf("exact key not found: %+v %v", it, ok)
	}
}

func TestUndoExpiryKeepsItOpen(t *testing.T) {
	s := open(t)
	s.UpsertItem(Item{ID: "dk-e", Kind: "ask", Title: "launch?", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	if n, _ := s.Expire(time.Now().Unix()); n != 1 {
		t.Fatalf("expired %d", n)
	}
	if _, err := s.Undo("dk-e"); err != nil {
		t.Fatal(err)
	}
	s.Expire(time.Now().Unix())
	if it, _, _ := s.GetItem("dk-e"); it.Status != "open" || it.ExpiresAt != 0 {
		t.Fatalf("after undo: %s expires=%d", it.Status, it.ExpiresAt)
	}
}

func TestConcurrentMigrationsOfAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Make it look like a database from before these columns existed.
	for _, a := range added {
		if a.table == "items" || a.table == "deliveries" {
			if _, err := s.DB.Exec(`ALTER TABLE ` + a.table + ` DROP COLUMN ` + a.column); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent migration failed: %v", err)
	}
}
