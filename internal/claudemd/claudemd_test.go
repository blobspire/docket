package claudemd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallIsIdempotentAndUninstallRestores(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	orig := "# My rules\n\nAlways run tests.\n"
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(orig), 0o644)

	if changed, err := Install("Cole"); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	b, _ := os.ReadFile(Path())
	if !strings.HasPrefix(string(b), orig) || !strings.Contains(string(b), "how to reach Cole") || !Installed() {
		t.Fatalf("after install:\n%s", b)
	}
	if changed, _ := Install("Cole"); changed {
		t.Fatal("second install changed the file")
	}
	// A newer block text replaces the old one in place.
	if changed, _ := Install("Sam"); !changed {
		t.Fatal("refresh did not change the file")
	}
	b, _ = os.ReadFile(Path())
	if strings.Count(string(b), "BEGIN docket") != 1 || !strings.Contains(string(b), "how to reach Sam") {
		t.Fatalf("refresh:\n%s", b)
	}
	if changed, err := Uninstall(); err != nil || !changed {
		t.Fatalf("uninstall: %v %v", changed, err)
	}
	b, _ = os.ReadFile(Path())
	if string(b) != orig {
		t.Fatalf("uninstall left:\n%q\nwant\n%q", b, orig)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "CLAUDE.md.docket-backup-*"))
	if len(backups) == 0 {
		t.Fatal("no backup written")
	}
}

func TestInstallIntoMissingFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := Install(""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(Path())
	if !strings.Contains(string(b), "how to reach the user") {
		t.Fatalf("%s", b)
	}
	Uninstall()
	b, _ = os.ReadFile(Path())
	if len(b) != 0 {
		t.Fatalf("expected empty file after uninstall, got %q", b)
	}
}
