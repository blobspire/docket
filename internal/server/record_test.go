package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blobspire/docket/internal/cli"
)

// --record travels from the CLI to the item and its sheet; a key re-file
// replaces it, and docket add can change it after the item is closed.
func TestRecordReachesTheSheet(t *testing.T) {
	e := newEnv(t, "")
	agent := getenv(map[string]string{"DOCKET_SESSION": "sess-1"})
	var out bytes.Buffer
	if err := cli.File("review", []string{"Seed 1004: carry-over confirmed", "--project", "vlm", "--new-project", "VLM teacher experiments", "--key", "seed-1004", "--record", "results/lp/seed_1004 @ 3f2a1c9"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	items, _ := e.s.st.Items("open")
	if len(items) != 1 || items[0].Record != "results/lp/seed_1004 @ 3f2a1c9" {
		t.Fatalf("items = %+v", items)
	}
	id := items[0].ID
	sheet := func() string {
		rec := httptest.NewRecorder()
		e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/item/"+id, nil))
		return rec.Body.String()
	}
	if b := sheet(); !strings.Contains(b, "<b>Record:</b> <code>results/lp/seed_1004 @ 3f2a1c9</code>") || !strings.Contains(b, "on box") {
		t.Fatalf("sheet has no record line:\n%s", b)
	}

	// A re-file with the same key replaces the record; one without keeps it.
	if err := cli.File("review", []string{"Seed 1004: carry-over confirmed (rerun)", "--project", "vlm", "--key", "seed-1004", "--record", "https://github.com/o/vfp/pull/12"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	if err := cli.File("review", []string{"Seed 1004: carry-over confirmed (final)", "--project", "vlm", "--key", "seed-1004"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	it, _, _ := e.s.st.GetItem(id)
	if it.Record != "https://github.com/o/vfp/pull/12" || it.Title != "Seed 1004: carry-over confirmed (final)" {
		t.Fatalf("after re-files: %+v", it)
	}
	if b := sheet(); !strings.Contains(b, `<a href="https://github.com/o/vfp/pull/12" target="_blank" rel="noopener noreferrer">`) {
		t.Fatalf("a URL record isn't a link:\n%s", b)
	}

	// Closed, then the agent says where the record moved: it stays closed.
	if err := e.s.Decide(id, Action{Kind: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := cli.Add([]string{id, "--record", "results/lp/seed_1004 @ 9e8d7c6"}, &out, agent); err != nil {
		t.Fatal(err)
	}
	e.cycle()
	it, _, _ = e.s.st.GetItem(id)
	if it.Record != "results/lp/seed_1004 @ 9e8d7c6" || it.Status != "done" {
		t.Fatalf("after docket add --record: status %s record %q", it.Status, it.Record)
	}
}

// A record can't smuggle a script link into the sheet.
func TestRecordIsNotALinkUnlessHTTP(t *testing.T) {
	for _, s := range []string{"javascript:alert(1)", "results/x", "ftp://h/x", "https://"} {
		if isURL(s) {
			t.Errorf("isURL(%q) = true", s)
		}
	}
	if !isURL("https://claude.ai/artifact/x") || !isURL(" http://h/x ") {
		t.Error("a web address isn't a URL")
	}
}
