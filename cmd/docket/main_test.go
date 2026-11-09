package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHookAlwaysExitsZero(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	// Point DOCKET_HOME at a file, so creating the spool directory fails.
	f := t.TempDir() + "/not-a-dir"
	if err := writeFile(f); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKET_HOME", f)
	var out, errb bytes.Buffer
	code := run([]string{"hook", "Stop"}, strings.NewReader(`{"session_id":"s"}`), &out, &errb)
	if code != 0 {
		t.Fatalf("hook exit code %d (would disrupt the session); stderr=%s", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("hook wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errb.String(), "docket hook:") {
		t.Fatalf("expected the failure on stderr, got %q", errb.String())
	}
}

func TestFileThenTailThenExport(t *testing.T) {
	t.Setenv("DOCKET_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"ask", "Push to main?", "--options", "yes|no"}, nil, &out, &errb); code != 0 {
		t.Fatalf("ask failed: %s", errb.String())
	}
	out.Reset()
	if code := run([]string{"spool", "tail", "-n", "5"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "Push to main?") {
		t.Fatalf("tail: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"spool", "export"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), `"_cursor"`) {
		t.Fatalf("export: code=%d out=%q", code, out.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"bogus"}, nil, &out, &errb); code != 1 {
		t.Fatalf("code = %d", code)
	}
}
