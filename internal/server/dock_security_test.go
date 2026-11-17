package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/store"
)

// putBlob stores an agent's attachment as the collector would.
func putBlob(t *testing.T, e *env, itemID, name, mime string, body []byte, declared int64) string {
	t.Helper()
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Dir(e.s.blobPath(sha)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.s.blobPath(sha), body, 0o600); err != nil {
		t.Fatal(err)
	}
	e.s.st.AddEvidence(store.Evidence{SHA256: sha, ItemID: itemID, Name: name, Mime: mime, Size: declared})
	e.s.st.DB.Exec(`UPDATE evidence SET fetched=1 WHERE sha256=?`, sha)
	return sha
}

// An agent's file opened on its own is inert whatever type it claims, in
// any letter case: no script runs with the docket's origin.
func TestBlobsAreAlwaysSandboxed(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-sec00001", Kind: "review", Title: "report", Project: "w"})
	sha := putBlob(t, e, "dk-sec00001", "report.md", "TEXT/HTML", []byte("<script>parent.pwned=1</script>"), 30)
	for _, typ := range []string{"Text%2FHTML", "image%2FSVG%2BXML", "text%2Fplain", "image%2Fpng", ""} {
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/blob/"+sha+"?type="+typ, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != "sandbox" {
			t.Errorf("type %q: code %d, CSP %q", typ, rec.Code, rec.Header().Get("Content-Security-Policy"))
		}
	}
}

// The resume command only ever runs claude: an agent-reported session id
// that isn't a plain id gets no command at all.
func TestResumeCommandIsSafeToPaste(t *testing.T) {
	e := newEnv(t, "")
	st := e.s.st
	for id, sid := range map[string]string{"dk-sec00002": "0f3c2a9e; touch PWNED; :", "dk-sec00003": "-p evil", "dk-sec00004": "0f3c2a9e-5b1d-4c7a-9e2f-6a8b1c3d4e5f"} {
		st.DB.Exec(`INSERT INTO sessions(id, machine, cwd, state) VALUES (?, 'box', '/w/a b', 'ended')`, sid)
		st.UpsertItem(store.Item{ID: id, Kind: "ask", Title: "q", Project: "w", SessionID: sid, Machine: "box", Source: "cli"})
	}
	if s := sheetHTML(t, e, "dk-sec00002"); strings.Contains(s, "data-copy") || strings.Contains(s, "PWNED") && strings.Contains(s, "--resume") {
		t.Error("a command was offered for a session id with shell syntax")
	}
	if s := sheetHTML(t, e, "dk-sec00003"); strings.Contains(s, "data-copy") {
		t.Error("a command was offered for a session id that's an option")
	}
	if s := sheetHTML(t, e, "dk-sec00004"); !strings.Contains(s, `data-copy="cd &#39;/w/a b&#39; &amp;&amp; claude --resume &#39;0f3c2a9e-5b1d-4c7a-9e2f-6a8b1c3d4e5f&#39;"`) {
		t.Errorf("no (or an unquoted) resume command for a plain id:\n%s", s[:min(len(s), 4000)])
	}
}

// An item id goes into URLs and pages: one an agent crafted is replaced.
func TestCraftedItemIDsAreReplaced(t *testing.T) {
	e := newEnv(t, "")
	e.file(t, "s", record.Item{ID: "../blob/abc?type=text/html&x=", Kind: "review", Title: "crafted", Project: "w"})
	items, _ := e.s.st.Items()
	for _, it := range items {
		if it.Title == "crafted" && (strings.ContainsAny(it.ID, "/?&.") || it.ID == "") {
			t.Fatalf("crafted id kept: %q", it.ID)
		}
	}
}

// A large or pathological markdown attachment can't tie up the server: past
// a small size it shows as plain text, sized from the file itself.
func TestBigMarkdownShowsAsText(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-sec00005", Kind: "review", Title: "notes", Project: "w"})
	putBlob(t, e, "dk-sec00005", "notes.md", "text/markdown", []byte(strings.Repeat("[a](", 60000)), 100) // declares 100 bytes
	putBlob(t, e, "dk-sec00005", "small.md", "text/markdown", []byte("**bold** note"), 13)
	start := time.Now()
	s := sheetHTML(t, e, "dk-sec00005")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the sheet took %v", d)
	}
	if !strings.Contains(s, "<pre>[a](") || !strings.Contains(s, "<strong>bold</strong>") {
		t.Fatal("the big file should show as text and the small one as markdown")
	}
}
