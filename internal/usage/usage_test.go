package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanCountsSubagentsTowardParentAndIsIncremental(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("DOCKET_HOME", t.TempDir())
	proj := filepath.Join(claude, "projects", "-w-site")
	os.MkdirAll(filepath.Join(proj, "s1", "subagents"), 0o700)
	main := filepath.Join(proj, "s1.jsonl")
	line := func(sid string, in, out int) string {
		return `{"type":"assistant","sessionId":"` + sid + `","cwd":"/w/site","timestamp":"2026-11-08T10:00:00Z","message":{"usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `,"cache_read_input_tokens":1000}}}` + "\n"
	}
	os.WriteFile(main, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"+line("s1", 10, 5)), 0o600)
	os.WriteFile(filepath.Join(proj, "s1", "subagents", "a1.jsonl"), []byte(line("agent-x", 3, 2)), 0o600)

	days, err := Scan(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil || len(days) != 1 {
		t.Fatalf("days = %+v %v", days, err)
	}
	d := days[0]
	if d.SessionID != "s1" || d.Input != 13 || d.Output != 7 || d.CacheRead != 2000 || d.Cwd != "/w/site" {
		t.Fatalf("day = %+v", d)
	}
	f, _ := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(line("s1", 100, 50))
	f.Close()
	days, _ = Scan(time.Now().Add(-30 * 24 * time.Hour))
	if len(days) != 1 || days[0].Input != 113 || days[0].Output != 57 {
		t.Fatalf("incremental = %+v", days)
	}
}

func TestQuotaPrefersNewest(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("DOCKET_HOME", t.TempDir())
	os.WriteFile(filepath.Join(claude, "nightly_rate_limits.json"), []byte(`{"rate_limits":{"seven_day":{"used_percentage":40,"resets_at":5}},"captured_at":100}`), 0o600)
	q, ok := Read()
	if !ok || q.Windows["seven_day"].UsedPct != 40 {
		t.Fatalf("q = %+v", q)
	}
	Record([]byte(`{"rate_limits":{"five_hour":{"used_percentage":12,"resets_at":9}}}`))
	q, _ = Read()
	if q.Windows["five_hour"].UsedPct != 12 {
		t.Fatalf("q = %+v", q)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
