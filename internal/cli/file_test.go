package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/record"
	"github.com/blobspire/docket/internal/spool"
)

func env(t *testing.T) func(string) string {
	t.Helper()
	t.Setenv("DOCKET_HOME", t.TempDir())
	m := map[string]string{"DOCKET_SESSION": "sess-1", "DOCKET_LEAD": "website"}
	return func(k string) string { return m[k] }
}

func last(t *testing.T) record.Record {
	t.Helper()
	lines, err := spool.Spool{Dir: config.SpoolDir()}.Tail(1)
	if err != nil || len(lines) != 1 {
		t.Fatalf("tail: %v %d", err, len(lines))
	}
	var r record.Record
	if err := json.Unmarshal(lines[0], &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAskWithFlagsAnywhere(t *testing.T) {
	getenv := env(t)
	var out bytes.Buffer
	err := File("ask", []string{"--options", "merge now|wait for CI", "Merge", "the", "PR?", "--recommend=merge now", "--blocking", "--due", "tomorrow"}, &out, getenv)
	if err != nil {
		t.Fatal(err)
	}
	r := last(t)
	it := r.Item
	if r.Event != "cli.file" || r.SessionID != "sess-1" || r.Lead != "website" {
		t.Fatalf("record = %+v", r)
	}
	want := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if it.Kind != "ask" || it.Title != "Merge the PR?" || len(it.Options) != 2 || it.Recommend != "merge now" || !it.Blocking || it.Due != want {
		t.Fatalf("item = %+v", it)
	}
	if !strings.Contains(out.String(), it.ID) || !strings.Contains(out.String(), "keep working") {
		t.Fatalf("confirmation = %q", out.String())
	}
}

func TestTaskBlocksImpliesBlocking(t *testing.T) {
	getenv := env(t)
	if err := File("task", []string{"Re-authenticate WARP", "--blocks", "lab SSH"}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	it := last(t).Item
	if it.Kind != "human_task" || !it.Blocking || it.Blocks != "lab SSH" {
		t.Fatalf("item = %+v", it)
	}
}

func TestEvidenceIsStoredByHash(t *testing.T) {
	getenv := env(t)
	dir := t.TempDir()
	png := filepath.Join(dir, "shot.png")
	os.WriteFile(png, []byte("fake png bytes"), 0o600)
	vid := filepath.Join(dir, "demo.mp4")
	os.WriteFile(vid, []byte("fake video"), 0o600)
	if err := File("review", []string{"New hero section", "--evidence", png, "--evidence=" + vid}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	it := last(t).Item
	if len(it.Evidence) != 2 || it.Evidence[0].Name != "shot.png" || it.Evidence[0].Mime != "image/png" || it.Evidence[1].Mime != "video/mp4" {
		t.Fatalf("evidence = %+v", it.Evidence)
	}
	blob := filepath.Join(config.SpoolDir(), "blobs", it.Evidence[0].SHA256)
	if b, err := os.ReadFile(blob); err != nil || string(b) != "fake png bytes" {
		t.Fatalf("blob %s: %v %q", blob, err, b)
	}
}

func TestBadInputIsRejectedWithHelp(t *testing.T) {
	getenv := env(t)
	for _, args := range [][]string{
		{},                              // no title
		{"x", "--due", "next week"},     // bad date
		{"x", "--nope"},                 // unknown flag
		{"x", "--options"},              // missing value
		{"x", "--evidence", "/no/such"}, // missing file
		{"x", "--blocking=maybe"},       // bad bool
	} {
		if err := File("ask", args, &bytes.Buffer{}, getenv); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestDone(t *testing.T) {
	getenv := env(t)
	if err := Done([]string{"dk-abcd1234", "--why", "pushed after approval"}, &bytes.Buffer{}, getenv); err != nil {
		t.Fatal(err)
	}
	r := last(t)
	if r.Event != "cli.done" || r.Done.ItemID != "dk-abcd1234" || r.Done.Note != "pushed after approval" {
		t.Fatalf("record = %+v", r)
	}
	if err := Done([]string{"not-an-id"}, &bytes.Buffer{}, getenv); err == nil {
		t.Fatal("accepted a non-docket id")
	}
}
