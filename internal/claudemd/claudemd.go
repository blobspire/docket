// Package claudemd manages docket's block in the global ~/.claude/CLAUDE.md:
// the short instruction that tells every agent on a machine to file what it
// needs from the human with `docket` instead of only mentioning it in chat.
package claudemd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	begin = "<!-- BEGIN docket (managed by `docket init --agent`; remove with `docket uninstall`) -->"
	end   = "<!-- END docket -->"
)

// Path is the global CLAUDE.md (CLAUDE_CONFIG_DIR is honoured).
func Path() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, "CLAUDE.md")
}

// Block is the managed text. person is how agents should refer to the human.
func Block(person string) string {
	if person == "" {
		person = "the user"
	}
	return begin + `
## docket: how to reach ` + person + `
` + person + ` does not read back through chat for things only they can do. When you need something from them, file it with ` + "`docket`" + ` and keep working (filing never blocks):
- Holding an action for their approval, or need their decision: ` + "`docket ask \"<question>\" --options \"a|b\" --recommend a`" + `
- Blocked on something only they can do (login, credentials, hardware, money, physical work): ` + "`docket task \"<what>\" --blocks \"<work it blocks>\"`" + `
- Work ready for their review: ` + "`docket review \"<what>\" --evidence <screenshot, video or report>`" + `
- A problem you noticed and are not fixing now: ` + "`docket issue \"<problem>\"`" + `
Do not use AskUserQuestion for these. Answers arrive as messages tagged ` + "`[docket dk-…]`" + `; when an item is resolved, run ` + "`docket done <id>`" + `. ` + "`docket help`" + ` has the details.
Preferences ` + person + ` has approved for all agents: @~/.docket/rules.md
` + end
}

// Installed reports whether the managed block is present.
func Installed() bool {
	b, err := os.ReadFile(Path())
	return err == nil && strings.Contains(string(b), begin) && strings.Contains(string(b), end)
}

// Install adds or refreshes the block, backing the file up first.
func Install(person string) (changed bool, err error) {
	path := Path()
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	next := upsert(string(old), Block(person))
	if next == string(old) {
		return false, nil
	}
	if err := backup(path, old); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	return true, writeAtomic(path, next)
}

// Uninstall removes the block, leaving everything else as it was.
func Uninstall() (changed bool, err error) {
	path := Path()
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	next := remove(string(old))
	if next == string(old) {
		return false, nil
	}
	if err := backup(path, old); err != nil {
		return false, err
	}
	return true, writeAtomic(path, next)
}

func upsert(doc, block string) string {
	if i, j, ok := span(doc); ok {
		if doc[j-1] == '\n' {
			block += "\n" // span consumed the block's trailing newline
		}
		return doc[:i] + block + doc[j:]
	}
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	if doc != "" {
		doc += "\n"
	}
	return doc + block + "\n"
}

func remove(doc string) string {
	i, j, ok := span(doc)
	if !ok {
		return doc
	}
	out := strings.TrimRight(doc[:i], "\n") + doc[j:]
	if strings.TrimSpace(out) == "" {
		return ""
	}
	return strings.TrimRight(out, "\n") + "\n"
}

func span(doc string) (int, int, bool) {
	i := strings.Index(doc, begin)
	if i < 0 {
		return 0, 0, false
	}
	j := strings.Index(doc[i:], end)
	if j < 0 {
		return 0, 0, false
	}
	j = i + j + len(end)
	if j < len(doc) && doc[j] == '\n' {
		j++
	}
	return i, j, true
}

// backup saves the file before docket changes it, keeping the newest 3.
func backup(path string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := os.WriteFile(fmt.Sprintf("%s.docket-backup-%s", path, time.Now().Format("20060102-150405")), data, 0o600); err != nil {
		return err
	}
	old, _ := filepath.Glob(path + ".docket-backup-*")
	sort.Strings(old)
	for len(old) > 3 {
		os.Remove(old[0])
		old = old[1:]
	}
	return nil
}

func writeAtomic(path, content string) error {
	tmp := path + ".docket-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
