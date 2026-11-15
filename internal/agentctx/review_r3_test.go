package agentctx

import (
	"testing"
	"time"
)

// done --key without a project match prefers this session's item, as the server does.
func TestKeyTargetPrefersThisSession(t *testing.T) {
	now := time.Now().Unix()
	c := Context{Open: []Item{
		{ID: "dk-mine", Key: "ci", Project: "a", SessionID: "s1", CreatedAt: now - 60},
		{ID: "dk-other", Key: "ci", Project: "b", SessionID: "s2", CreatedAt: now - 30},
	}}
	if got, ok := c.KeyTarget("ci", "zzz", false, "s1"); !ok || got.ID != "dk-mine" {
		t.Fatalf("target = %+v", got)
	}
	if got, _ := c.KeyTarget("ci", "zzz", false, "s9"); got.ID != "dk-other" {
		t.Fatalf("machine fallback should be the newest: %+v", got)
	}
}

// done with an id the agent was told (an alias) hides the item it means.
func TestDoneByAliasHides(t *testing.T) {
	now := time.Now().Unix()
	c := Context{Open: []Item{{ID: "dk-first", Aliases: []string{"dk-told"}, CreatedAt: now - 60}}}
	if m := c.Merged(Overlay{Closed: []Closed{{ID: "dk-told", RecordID: "r", At: now}}}); len(m.Open) != 0 {
		t.Fatalf("open = %+v", m.Open)
	}
}

// a closing past retention stops hiding (the server's copy is the truth).
func TestOldClosingExpires(t *testing.T) {
	now := time.Now().Unix()
	c := Context{Open: []Item{{ID: "dk-reopened", CreatedAt: now - 5*86400}}}
	if m := c.Merged(Overlay{Closed: []Closed{{ID: "dk-reopened", RecordID: "r", At: now - 4*86400}}}); len(m.Open) != 1 {
		t.Fatalf("a 4-day-old closing still hides a reopened item: %+v", m.Open)
	}
}

// retention stays inside the server's 48 h seen window.
func TestRetentionInsideSeenWindow(t *testing.T) {
	if Retention >= 48*time.Hour {
		t.Fatalf("Retention %v outlives the server's seen window", Retention)
	}
}
