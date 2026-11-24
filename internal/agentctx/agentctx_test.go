package agentctx

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestClosedKeyOnlyHidesWhatItClosed(t *testing.T) {
	now := time.Now().Unix()
	c := Context{Open: []Item{
		{ID: "dk-old", Key: "ci", Project: "windward", CreatedAt: now - 3600},
		{ID: "dk-other", Key: "ci", Project: "cwant", CreatedAt: now - 3600},
		{ID: "dk-new", Key: "ci", Project: "windward", CreatedAt: now + 5},
	}}
	o := Overlay{Closed: []Closed{{Key: "ci", Project: "windward", At: now}}}
	got := map[string]bool{}
	for _, it := range c.Merged(o).Open {
		got[it.ID] = true
	}
	if got["dk-old"] || !got["dk-other"] || !got["dk-new"] {
		t.Fatalf("open after done --key ci --project windward: %v (want only dk-other and the later re-file dk-new)", got)
	}
}

func TestParallelFilingsKeepEveryEntry(t *testing.T) {
	t.Setenv("DOCKET_HOME", t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Remember(func(o *Overlay) {
				o.Filed = append(o.Filed, Item{ID: fmt.Sprintf("dk-%08d", i), CreatedAt: time.Now().Unix()})
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if n := len(LoadOverlay().Filed); n != 40 {
		t.Fatalf("kept %d of 40 filings", n)
	}
}

func TestOverlayDefersToTheServerOnceSeen(t *testing.T) {
	now := time.Now().Unix()
	o := Overlay{
		Projects: []NewThing{{Name: "robots", Description: "x", ID: "dk-1", At: now}},
		Dirs:     []NewThing{{Name: "/w/robot", Project: "robots", ID: "dk-1", At: now}},
	}
	// Not processed yet: the new project and folder default apply.
	m := Context{}.Merged(o)
	if !m.HasProject("robots") || m.ProjectForDir("/w/robot/sub") != "robots" {
		t.Fatalf("before the server saw it: %+v", m)
	}
	// Processed, and since renamed away: the server's view wins.
	m = Context{Projects: []Project{{Name: "site"}}, Seen: []string{"dk-1"}}.Merged(o)
	if m.HasProject("robots") || m.ProjectForDir("/w/robot") != "" {
		t.Fatalf("after the server saw it: %+v", m)
	}
}

func TestOldEntriesDontComeBackOnAQuietMachine(t *testing.T) {
	old := time.Now().Add(-4 * 24 * time.Hour).Unix()
	o := Overlay{
		Filed:    []Item{{ID: "dk-oldoldol", Title: "Approve the Q3 budget?", CreatedAt: old}},
		Projects: []NewThing{{Name: "arms", ID: "dk-p", At: old}},
		Dirs:     []NewThing{{Name: "/w/repo", Project: "arms", ID: "dk-p", At: old}},
	}
	// The server's seen window has long passed these, and it no longer lists them.
	m := Context{Projects: []Project{{Name: "arm-work"}}, Dirs: []Dir{{Dir: "/w/repo", Project: "arm-work"}}}.Merged(o)
	if len(m.Open) != 0 || m.HasProject("arms") || m.ProjectForDir("/w/repo") != "arm-work" {
		t.Fatalf("stale entries came back: open=%+v projects=%+v dir=%q", m.Open, m.Projects, m.ProjectForDir("/w/repo"))
	}
}

func TestDoneByKeyHidesOnlyWhatTheServerCloses(t *testing.T) {
	now := time.Now().Unix()
	c := Context{Open: []Item{
		{ID: "dk-win", Key: "ci", Project: "windward", SessionID: "s1", CreatedAt: now - 60},
		{ID: "dk-cwa", Key: "ci", Project: "cwant", SessionID: "s2", CreatedAt: now - 30},
	}}
	// Resolved from the folder (windward), not typed: the server closes windward's.
	tgt, ok := c.KeyTarget("ci", "windward", false, "s9")
	if !ok || tgt.ID != "dk-win" {
		t.Fatalf("target = %+v %v", tgt, ok)
	}
	m := c.Merged(Overlay{Closed: []Closed{{ID: tgt.ID, RecordID: "r1", At: now}}})
	if len(m.Open) != 1 || m.Open[0].ID != "dk-cwa" {
		t.Fatalf("open = %+v", m.Open)
	}
	// Once the server has applied the done record, its copy is the truth.
	m = Context{Open: c.Open, Seen: []string{"r1"}}.Merged(Overlay{Closed: []Closed{{ID: "dk-win", RecordID: "r1", At: now}}})
	if len(m.Open) != 2 {
		t.Fatalf("after the server applied it, open = %+v", m.Open)
	}
	// An explicit project with no match there hides nothing elsewhere.
	if _, ok := c.KeyTarget("ci", "ladr", true, "s1"); ok {
		t.Fatal("an explicit project fell back to another project's item")
	}
}

func TestServerFolderMappingWinsATie(t *testing.T) {
	now := time.Now().Unix()
	o := Overlay{Dirs: []NewThing{{Name: "/w/repo", Project: "errand", ID: "dk-new", At: now}, {Name: "/w/repo/sub", Project: "deeper", ID: "dk-new2", At: now}}}
	m := Context{Dirs: []Dir{{Dir: "/w/repo", Project: "established"}}}.Merged(o)
	if got := m.ProjectForDir("/w/repo"); got != "established" {
		t.Fatalf("an unprocessed overlay entry beat the server's mapping for the same folder: %q", got)
	}
	if got := m.ProjectForDir("/w/repo/sub/x"); got != "deeper" {
		t.Fatalf("a deeper overlay folder should still apply: %q", got)
	}
}

// Re-filing with the key of an open item (docket propose run again, say)
// updates that item: docket list shows it once, under the newest filing's
// id, which reaches the right item whether the server merges it (an alias)
// or the open item was closed meanwhile (a new item).
func TestRefileWithTheSameKeyIsListedOnce(t *testing.T) {
	now := time.Now().Unix()
	c := Context{Open: []Item{
		{ID: "dk-first01", Title: "Old plan", Key: "idea-abcdefgh", Project: "docket", Urgency: "now", CreatedAt: now - 600},
		{ID: "dk-aliased", Title: "Known by another id", Project: "docket", Urgency: "normal", CreatedAt: now - 600, Aliases: []string{"dk-alias01"}},
	}}
	o := Overlay{Filed: []Item{
		{ID: "dk-second1", Title: "New plan", Key: "idea-abcdefgh", Project: "docket", CreatedAt: now}, // no --urgency: "now" stands
		{ID: "dk-alias01", Title: "Known by another id", Project: "docket", CreatedAt: now},
		{ID: "dk-elsewhr", Title: "Same key, other project", Key: "idea-abcdefgh", Project: "cwant", CreatedAt: now},
		// Two filings the server hasn't seen yet, with one key: one entry.
		{ID: "dk-unseen1", Title: "Nightly report v1", Key: "nightly", Project: "docket", Urgency: "soon", CreatedAt: now},
		{ID: "dk-unseen2", Title: "Nightly report v2", Key: "nightly", Project: "docket", CreatedAt: now + 1},
	}}
	m := c.Merged(o)
	got := map[string]string{}
	for _, it := range m.Open {
		got[it.ID] = it.Title + " / " + it.Urgency
	}
	want := map[string]string{
		"dk-second1": "New plan / now",
		"dk-aliased": "Known by another id / normal",
		"dk-elsewhr": "Same key, other project / normal",
		"dk-unseen2": "Nightly report v2 / soon",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("open = %v\nwant   %v", got, want)
	}
	// Closed here before the re-file: the re-file is a new item, listed.
	o3 := o
	o3.Closed = []Closed{{ID: "dk-first01", At: now - 10}}
	got3 := map[string]string{}
	for _, it := range c.Merged(o3).Open {
		got3[it.ID] = it.Title
	}
	if len(got3) != 4 || got3["dk-second1"] != "New plan" || got3["dk-first01"] != "" {
		t.Fatalf("a re-file after a local done: %v, want it listed once under dk-second1 among 4", got3)
	}
	// Closing it locally after the re-file, by either id, hides it.
	for _, id := range []string{"dk-first01", "dk-second1"} {
		o2 := o
		o2.Closed = []Closed{{ID: id, At: now + 5}}
		for _, it := range c.Merged(o2).Open {
			if it.Title == "New plan" {
				t.Fatalf("docket done %s left it listed", id)
			}
		}
	}
}

// A done and a re-file with the same key in the same second are told apart
// by the order this machine made them in (Remember numbers them).
func TestDoneAndRefileInTheSameSecond(t *testing.T) {
	t.Setenv("DOCKET_HOME", t.TempDir())
	now := time.Now().Unix()
	c := Context{Open: []Item{{ID: "dk-aaaaaaaa", Title: "First", Key: "k", Project: "docket", CreatedAt: now - 60}}}
	list := func() []string {
		var ids []string
		for _, it := range c.Merged(LoadOverlay()).Open {
			ids = append(ids, it.ID)
		}
		return ids
	}
	// done, then re-file: the re-file is a new item.
	Remember(func(o *Overlay) { o.Closed = append(o.Closed, Closed{ID: "dk-aaaaaaaa", RecordID: "r1", At: now}) })
	Remember(func(o *Overlay) {
		o.Filed = append(o.Filed, Item{ID: "dk-bbbbbbbb", Title: "Second", Key: "k", Project: "docket", CreatedAt: now})
	})
	if got := list(); fmt.Sprint(got) != "[dk-bbbbbbbb]" {
		t.Fatalf("done then re-file in one second: open = %v, want the re-file", got)
	}
	// ...then done of the re-file, same second: nothing open.
	Remember(func(o *Overlay) { o.Closed = append(o.Closed, Closed{ID: "dk-bbbbbbbb", RecordID: "r2", At: now}) })
	if got := list(); len(got) != 0 {
		t.Fatalf("after closing the re-file: open = %v", got)
	}
	// Two unseen filings and a done of the first, in one second, each order.
	t.Setenv("DOCKET_HOME", t.TempDir())
	c = Context{}
	Remember(func(o *Overlay) {
		o.Filed = append(o.Filed, Item{ID: "dk-u1111111", Key: "k2", Project: "p", CreatedAt: now})
	})
	Remember(func(o *Overlay) { o.Closed = append(o.Closed, Closed{ID: "dk-u1111111", RecordID: "r3", At: now}) })
	Remember(func(o *Overlay) {
		o.Filed = append(o.Filed, Item{ID: "dk-u2222222", Key: "k2", Project: "p", CreatedAt: now})
	})
	if got := list(); fmt.Sprint(got) != "[dk-u2222222]" {
		t.Fatalf("file, done, re-file in one second: open = %v", got)
	}
	t.Setenv("DOCKET_HOME", t.TempDir())
	Remember(func(o *Overlay) {
		o.Filed = append(o.Filed, Item{ID: "dk-u1111111", Key: "k2", Project: "p", CreatedAt: now})
	})
	Remember(func(o *Overlay) {
		o.Filed = append(o.Filed, Item{ID: "dk-u2222222", Key: "k2", Project: "p", CreatedAt: now})
	})
	Remember(func(o *Overlay) { o.Closed = append(o.Closed, Closed{ID: "dk-u1111111", RecordID: "r4", At: now}) })
	if got := list(); len(got) != 0 {
		t.Fatalf("file, re-file, done in one second: open = %v (the re-file merged into the closed one)", got)
	}
}
