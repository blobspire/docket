package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportMarkdown(t *testing.T) {
	md := `# Status
## Human tasks (checklist)
- [ ] Approve SIM_VERSION 6
- [x] Install timers
- [ ] Insert the COLE drive
## Waiting on the human
- Review the R&D audit
- ~~old item~~
## Running now
- lane AB
`
	p := filepath.Join(t.TempDir(), "STATUS.md")
	os.WriteFile(p, []byte(md), 0o600)
	texts := func(section string) string {
		got, err := ImportMarkdown(p, section)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range got {
			out = append(out, g.Text)
		}
		return strings.Join(out, "|")
	}
	if got := texts("waiting on the human"); got != "Review the R&D audit" {
		t.Fatalf("section only: %q", got)
	}
	if got := texts("human tasks"); got != "Approve SIM_VERSION 6|Insert the COLE drive" {
		t.Fatalf("checklist section: %q", got)
	}
	if got := texts(""); got != "Approve SIM_VERSION 6|Insert the COLE drive" {
		t.Fatalf("all unchecked: %q", got)
	}
}
