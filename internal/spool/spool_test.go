package spool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type rec struct {
	ID string `json:"id"`
	N  int    `json:"n,omitempty"`
}

// export runs one Export and returns the record lines and the trailer.
func export(t *testing.T, s Spool, from Cursor, max int64) ([]map[string]any, Trailer) {
	t.Helper()
	var buf bytes.Buffer
	tr, err := s.Export(&buf, from, max)
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var last Trailer
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("export produced invalid JSON %q: %v", sc.Text(), err)
		}
		if _, ok := m["_cursor"]; ok {
			json.Unmarshal(sc.Bytes(), &last)
			continue
		}
		lines = append(lines, m)
	}
	if last != tr {
		t.Fatalf("trailer line %+v != returned %+v", last, tr)
	}
	return lines, tr
}

func cursor(t *testing.T, tr Trailer) Cursor {
	t.Helper()
	c, err := ParseCursor(tr.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAppendExportResume(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	for i := 1; i <= 3; i++ {
		if err := s.Append(rec{ID: fmt.Sprint("r", i), N: i}); err != nil {
			t.Fatal(err)
		}
	}
	lines, tr := export(t, s, Cursor{}, 1<<20)
	if len(lines) != 3 || tr.More {
		t.Fatalf("got %d lines, more=%v", len(lines), tr.More)
	}
	// Nothing new: same cursor, no lines.
	lines2, tr2 := export(t, s, cursor(t, tr), 1<<20)
	if len(lines2) != 0 || tr2.Cursor != tr.Cursor {
		t.Fatalf("re-export after end: %d lines, cursor %s vs %s", len(lines2), tr2.Cursor, tr.Cursor)
	}
	s.Append(rec{ID: "r4"})
	lines3, _ := export(t, s, cursor(t, tr), 1<<20)
	if len(lines3) != 1 || lines3[0]["id"] != "r4" {
		t.Fatalf("resume got %v", lines3)
	}
}

func TestBudgetPagesButAlwaysProgresses(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	for i := 0; i < 10; i++ {
		s.Append(rec{ID: fmt.Sprint("r", i)})
	}
	var all []map[string]any
	c := Cursor{}
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("export never finished")
		}
		lines, tr := export(t, s, c, 1) // tiny budget: one line per page
		if len(lines) > 1 {
			t.Fatalf("budget 1 returned %d lines", len(lines))
		}
		all = append(all, lines...)
		c = cursor(t, tr)
		if !tr.More {
			break
		}
	}
	if len(all) != 10 {
		t.Fatalf("paged export got %d records, want 10", len(all))
	}
}

func TestPartialLastLineIsHeldBack(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	s.Append(rec{ID: "done"})
	seg := filepath.Join(s.Dir, SegmentName(time.Now()))
	f, _ := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"id":"half`) // a writer mid-line
	f.Close()
	lines, tr := export(t, s, Cursor{}, 1<<20)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want only the complete one", len(lines))
	}
	f, _ = os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`way"}` + "\n")
	f.Close()
	lines, _ = export(t, s, cursor(t, tr), 1<<20)
	if len(lines) != 1 || lines[0]["id"] != "halfway" {
		t.Fatalf("finished line not exported: %v", lines)
	}
}

func TestAbandonedPartialInOldSegmentIsSkipped(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	old := filepath.Join(s.Dir, "seg-2026010100.jsonl")
	os.WriteFile(old, []byte(`{"id":"a"}`+"\n"+`{"id":"crashed`), 0o600)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)
	s.Append(rec{ID: "b"})
	lines, tr := export(t, s, Cursor{}, 1<<20)
	if len(lines) != 2 || lines[0]["id"] != "a" || lines[1]["id"] != "b" || tr.More {
		t.Fatalf("got %v more=%v", lines, tr.More)
	}
}

func TestRawLinesGetStableIDs(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	seg := filepath.Join(s.Dir, SegmentName(time.Now()))
	os.MkdirAll(s.Dir, 0o700)
	os.WriteFile(seg, []byte(`{"v":0,"event":"Stop","raw":{"session_id":"x"}}`+"\n"+`not json`+"\n"), 0o600)
	a, _ := export(t, s, Cursor{}, 1<<20)
	b, _ := export(t, s, Cursor{}, 1<<20)
	if len(a) != 2 || a[0]["id"] == nil || a[0]["id"] != b[0]["id"] || a[1]["id"] != b[1]["id"] {
		t.Fatalf("raw ids not stable: %v vs %v", a, b)
	}
	if a[0]["event"] != "Stop" || a[1]["event"] != "unparsable" {
		t.Fatalf("raw records mangled: %v", a)
	}
	if !strings.HasPrefix(a[0]["id"].(string), "raw-") {
		t.Fatalf("raw id %v", a[0]["id"])
	}
}

func TestConcurrentAppendsNeverInterleave(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	big := strings.Repeat("x", 100_000) // far over PIPE_BUF
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := s.Append(map[string]any{"id": fmt.Sprint(w, "-", i), "pad": big}); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	lines, _ := export(t, s, Cursor{}, 1<<30)
	if len(lines) != 160 {
		t.Fatalf("got %d records, want 160", len(lines))
	}
}

func TestAckDeletesOnlyOldFullyAckedSegments(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	os.MkdirAll(s.Dir, 0o700)
	for _, name := range []string{"seg-2026010100.jsonl", "seg-2026010101.jsonl", "seg-2026010102.jsonl"} {
		p := filepath.Join(s.Dir, name)
		os.WriteFile(p, []byte(`{"id":"`+name+`"}`+"\n"), 0o600)
		past := time.Now().Add(-96 * time.Hour)
		os.Chtimes(p, past, past)
	}
	if err := s.Ack(Cursor{Seg: "seg-2026010101.jsonl", Off: 5}, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	segs, _ := s.Segments()
	if strings.Join(segs, ",") != "seg-2026010101.jsonl,seg-2026010102.jsonl" {
		t.Fatalf("after ack: %v", segs)
	}
	if got := s.Acked(); got.Seg != "seg-2026010101.jsonl" || got.Off != 5 {
		t.Fatalf("acked = %+v", got)
	}
}

func TestParseCursor(t *testing.T) {
	for _, bad := range []string{"x", "seg-1.jsonl:-1", "../etc:3", "seg-1.jsonl:abc"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) accepted", bad)
		}
	}
	c, err := ParseCursor("seg-2026110818.jsonl:42")
	if err != nil || c.Off != 42 || c.String() != "seg-2026110818.jsonl:42" {
		t.Fatalf("round trip: %+v %v", c, err)
	}
}

func TestTail(t *testing.T) {
	s := Spool{Dir: t.TempDir()}
	for i := 0; i < 5; i++ {
		s.Append(rec{ID: fmt.Sprint(i)})
	}
	got, _ := s.Tail(2)
	if len(got) != 2 || !bytes.Contains(got[0], []byte(`"3"`)) || !bytes.Contains(got[1], []byte(`"4"`)) {
		t.Fatalf("tail = %q", got)
	}
}
