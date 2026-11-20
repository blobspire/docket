package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/store"
)

func sectionTitles(q *queueView) []string {
	var out []string
	for _, s := range q.Sections {
		out = append(out, s.Title)
	}
	return out
}

// A–Z puts the docket's projects in name order, "unsorted" after them, and
// projects that only have answers waiting on agents last of all, as under
// Priority (nothing in them needs the human).
// Items in each project stay in priority order, and act-now items stay in
// the red band at the top.
func TestAZSortsProjectsByName(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	old := time.Now().Add(-2 * time.Hour).Unix()
	for _, it := range []store.Item{
		{ID: "dk-az000001", Kind: "ask", Title: "Zeta deadline today", Project: "zeta", Urgency: "soon", Due: time.Now().Format("2006-01-02")},
		{ID: "dk-az000002", Kind: "ask", Title: "Zebra report", Project: "alpha", CreatedAt: old},
		{ID: "dk-az000003", Kind: "ask", Title: "Apple decision", Project: "alpha", Urgency: "soon"},
		{ID: "dk-az000004", Kind: "ask", Title: "No project", Project: ""},
		{ID: "dk-az000005", Kind: "ask", Title: "Answered in mid", Project: "mid"},
		{ID: "dk-az000007", Kind: "ask", Title: "Answered in beta", Project: "beta"},
		{ID: "dk-az000006", Kind: "issue", Title: "Logins expired", Project: "zeta", Urgency: "now"},
	} {
		if _, _, err := st.UpsertItem(it); err != nil {
			t.Fatal(err)
		}
	}
	st.DB.Exec(`UPDATE items SET status='answered', answer='yes', answered_at=? WHERE id IN ('dk-az000005','dk-az000007')`, time.Now().Unix())

	pr, err := e.s.buildQueue(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	// The priority order this test contrasts with: the most pressing
	// project first, the waiting-only one last.
	if got := strings.Join(sectionTitles(pr), ","); got != "zeta,alpha,unsorted,beta,mid" {
		t.Fatalf("test setup: priority sections %s", got)
	}
	if pr.Sections[0].Title != "zeta" {
		t.Fatalf("test setup: the most pressing project isn't first under Priority: %v", sectionTitles(pr))
	}
	if pr.Sections[len(pr.Sections)-1].Title != "mid" {
		t.Fatalf("test setup: the waiting-only project isn't last under Priority: %v", sectionTitles(pr))
	}

	az, err := e.s.buildQueue(url.Values{"sort": {"az"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sectionTitles(az), ","); got != "alpha,zeta,unsorted,beta,mid" {
		t.Fatalf("A–Z sections: %s, want alpha,zeta,unsorted,beta,mid", got)
	}
	var alpha []string
	for _, r := range az.Sections[0].Rows {
		alpha = append(alpha, r.Title)
	}
	if strings.Join(alpha, "|") != "Apple decision|Zebra report" {
		t.Fatalf("items in a project keep priority order (soon before normal): %v", alpha)
	}
	if len(az.Now) != 1 || az.Now[0].ID != "dk-az000006" {
		t.Fatalf("act-now items stay in the band: %+v", az.Now)
	}
	for _, s := range az.Sections {
		for _, r := range s.Rows {
			if r.ID == "dk-az000006" {
				t.Fatal("the act-now item is also in its project's section")
			}
		}
	}
	if az.SortName() != "A–Z" {
		t.Fatalf("sort menu label %q", az.SortName())
	}
}

// The sort is remembered: choosing one sets a cookie, and a plain / (the
// docket mark, Back to All) uses it. Choosing Priority says so in its link,
// so it replaces a remembered A–Z.
func TestSortChoiceIsRemembered(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-az000011", Kind: "ask", Title: "One", Project: "alpha"})
	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("GET %s: %d", path, rec.Code)
		}
		return rec
	}
	chose := get("/?sort=az", nil)
	var c *http.Cookie
	for _, k := range chose.Result().Cookies() {
		if k.Name == sortCookie {
			c = k
		}
	}
	if c == nil || c.Value != "az" {
		t.Fatalf("choosing A–Z sets no cookie: %v", chose.Result().Cookies())
	}
	if !strings.Contains(chose.Body.String(), `aria-checked="true" href="/?sort=az"`) {
		t.Fatal("the A–Z menu item isn't checked")
	}
	home := get("/", c).Body.String()
	if !strings.Contains(home, `<span class="sl-t">A–Z</span>`) {
		t.Fatal("/ with the cookie isn't sorted A–Z")
	}
	if !strings.Contains(home, `href="/?sort=priority"`) {
		t.Fatal("the Priority menu link doesn't name its sort, so it can't replace A–Z")
	}
	if !strings.Contains(home, `href="/?p=alpha&amp;sort=az"`) {
		t.Fatal("project links don't keep A–Z")
	}
	if strings.Contains(get("/", &http.Cookie{Name: sortCookie, Value: "bogus"}).Body.String(), `<span class="sl-t"></span>`) {
		t.Fatal("an unknown remembered sort isn't ignored")
	}
	if !strings.Contains(get("/", nil).Body.String(), `<span class="sl-t">Priority</span>`) {
		t.Fatal("with nothing remembered, the docket isn't sorted by priority")
	}
}
