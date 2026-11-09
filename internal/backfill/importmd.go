package backfill

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// Task is one open task found in a markdown file.
type Task struct {
	Text string
	Line int
}

var (
	heading   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	unchecked = regexp.MustCompile(`^\s*[-*]\s+\[ \]\s+(.+)$`)
	checked   = regexp.MustCompile(`^\s*[-*]\s+\[[xX]\]\s+`)
	bullet    = regexp.MustCompile(`^\s*[-*]\s+(.+)$`)
)

// ImportMarkdown returns open tasks from a markdown file. With section set,
// only items under headings containing it (case-insensitive) are taken:
// unchecked "- [ ]" boxes and plain bullets. Without it, every unchecked box.
func ImportMarkdown(path, section string) ([]Task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Task
	inSection, sectionLevel := false, 0
	section = strings.ToLower(strings.TrimSpace(section))
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		l := sc.Text()
		if m := heading.FindStringSubmatch(l); m != nil {
			level := len(m[1])
			if section != "" && strings.Contains(strings.ToLower(m[2]), section) {
				inSection, sectionLevel = true, level
			} else if inSection && level <= sectionLevel {
				inSection = false
			}
			continue
		}
		if m := unchecked.FindStringSubmatch(l); m != nil {
			if section == "" || inSection {
				out = append(out, Task{Text: strings.TrimSpace(m[1]), Line: n})
			}
			continue
		}
		if checked.MatchString(l) {
			continue
		}
		if inSection {
			if m := bullet.FindStringSubmatch(l); m != nil {
				if t := strings.TrimSpace(m[1]); t != "" && !strings.HasPrefix(t, "~~") {
					out = append(out, Task{Text: t, Line: n})
				}
			}
		}
	}
	return out, sc.Err()
}
