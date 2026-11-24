package server

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/store"
)

// The nightly backup is a gzipped, consistent copy of the database that
// opens as a docket store; it runs once a day after 4:00 and keeps a week.
func TestNightlyBackup(t *testing.T) {
	e := newEnv(t, "")
	e.s.st.UpsertItem(store.Item{ID: "dk-backup01", Kind: "review", Title: "LIBERO-plus results", Machine: "box"})
	dir := e.s.backupDir()
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 30, 0, 0, time.Local) }

	e.s.backupDaily(day(24, 3)) // before 4:00: not yet
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("backed up before 4:00 (%v)", err)
	}
	// A week of older backups, plus one more: the oldest two go.
	os.MkdirAll(dir, 0o755)
	for d := 16; d <= 23; d++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("docket-2026-11-%02d.db.gz", d)), []byte("old"), 0o600)
	}
	e.s.backupDaily(day(24, 4))
	path := filepath.Join(dir, "docket-2026-11-24.db.gz")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v, want 0600", info.Mode().Perm())
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Fatalf("backups folder mode %v, want 0700", d.Mode().Perm())
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 7 || left[0] != filepath.Join(dir, "docket-2026-11-18.db.gz") {
		t.Fatalf("kept %v, want the newest 7 (from Nov 18) and nothing else", left)
	}

	// It's a working database with the item in it.
	f, _ := os.Open(path)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "docket.db")
	out, _ := os.Create(restored)
	io.Copy(out, zr)
	out.Close()
	st, err := store.Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if it, ok, _ := st.GetItem("dk-backup01"); !ok || it.Title != "LIBERO-plus results" {
		t.Fatalf("restored backup lacks the item: %+v %v", it, ok)
	}

	// Once a day: a second tick the same day doesn't redo it.
	os.Remove(path)
	e.s.backupDaily(day(24, 22))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("backed up twice in one day")
	}
	e.s.backupDaily(day(25, 5))
	if _, err := os.Stat(filepath.Join(dir, "docket-2026-11-25.db.gz")); err != nil {
		t.Fatalf("no backup the next day: %v", err)
	}

	// The Health page says when the last one ran.
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if b := rec.Body.String(); !strings.Contains(b, "Last backup") || !strings.Contains(b, "keeps 7 nightly copies") {
		t.Fatalf("health page:\n%s", b)
	}
}

func TestBackupsOff(t *testing.T) {
	e := newEnv(t, "backup_days = 0\nperson = \"Cole\"\n[[machine]]\nname = \"box\"\nmode = \"local\"\n")
	e.s.backupDaily(time.Date(2026, 11, 24, 5, 0, 0, 0, time.Local))
	if _, err := os.Stat(e.s.backupDir()); !os.IsNotExist(err) {
		t.Fatal("backed up with backup_days = 0")
	}
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if !strings.Contains(rec.Body.String(), "Nightly backups are off") {
		t.Fatal("health page doesn't say backups are off")
	}
}

// A failed backup says why on the Health page and isn't retried every tick.
func TestBackupFailureShows(t *testing.T) {
	e := newEnv(t, "")
	os.WriteFile(e.s.backupDir(), []byte("a file where the folder goes"), 0o600)
	e.s.backupDaily(time.Date(2026, 11, 24, 5, 0, 0, 0, time.Local))
	if b := e.s.backupStatus(); b.Error == "" || b.At != 0 {
		t.Fatalf("status after a failure: %+v", b)
	}
	rec := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if !strings.Contains(rec.Body.String(), "The last backup failed") {
		t.Fatal("health page hides the failure")
	}
}
