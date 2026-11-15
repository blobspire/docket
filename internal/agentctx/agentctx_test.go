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
