package server

import (
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// A row says when its item was filed, and when it last changed once that
// differs: a faded (week-old) row then shows why it's faded.
func TestUpdatedAgo(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	day := 24 * time.Hour
	for _, c := range []struct {
		name             string
		created, updated int64
		want             string
	}{
		{"never changed", at(8 * day), at(8 * day), ""},
		{"changed while filing", at(3 * time.Hour), at(3*time.Hour - 2*time.Minute), ""},
		{"changed days later", at(8 * day), at(6 * day), "6d ago"},
		{"changed, but reads the same", at(8*day + time.Hour), at(8 * day), ""},
		{"changed minutes ago", at(30 * time.Minute), at(5 * time.Minute), "5m ago"},
		{"changed just now", at(time.Hour), now.Unix(), "just now"},
	} {
		if got := updatedAgo(now, c.created, c.updated); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRowShowsFiledAndUpdated(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	day := 24 * time.Hour
	for _, id := range []string{"dk-fu000001", "dk-fu000002"} {
		if _, _, err := st.UpsertItem(store.Item{ID: id, Kind: "human_task", Title: "plug in " + id, Project: "w", SessionID: "s", Machine: "box", Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	filed := time.Now().Add(-8 * day).Unix()
	st.DB.Exec(`UPDATE items SET created_at=?, updated_at=? WHERE id='dk-fu000001'`, filed, filed)
	st.DB.Exec(`UPDATE items SET created_at=?, updated_at=? WHERE id='dk-fu000002'`, filed, time.Now().Add(-6*day).Unix())

	page := get(e, "/?p=w")
	only := rowHTML(t, page, "dk-fu000001")
	if !strings.Contains(only, "filed 8d ago</span>") || strings.Contains(only, "updated") {
		t.Errorf("a row never changed after filing should say only when it was filed: %s", only)
	}
	if !strings.Contains(only, ` stale"`) {
		t.Errorf("a row untouched for 8 days should fade: %s", only)
	}
	both := rowHTML(t, page, "dk-fu000002")
	if !strings.Contains(both, "filed 8d ago</span>") || !strings.Contains(both, "updated 6d ago</span>") {
		t.Errorf("a row changed after filing should say both: %s", both)
	}
	if strings.Contains(both, ` stale"`) {
		t.Errorf("a row changed 6 days ago shouldn't fade yet: %s", both)
	}

	if sh := sheetHTML(t, e, "dk-fu000002"); !strings.Contains(sh, "filed 8d ago</span>") || !strings.Contains(sh, "updated 6d ago</span>") {
		t.Error("the item's sheet should say when it was filed and last updated")
	}
	if sh := sheetHTML(t, e, "dk-fu000001"); !strings.Contains(sh, "filed 8d ago</span>") || strings.Contains(sh, "updated ") {
		t.Error("the sheet of an item never changed should say only when it was filed")
	}
}
