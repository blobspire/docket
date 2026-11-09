// Package record defines the spool record: one line per hook event or CLI
// filing, as written on each machine and pulled by the server.
package record

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/config"
	"github.com/blobspire/docket/internal/ulid"
)

// Version of the record format.
const Version = 1

// MaxString caps any single string inside a hook payload (for example
// last_assistant_message or a long prompt).
const MaxString = 32 << 10

type Record struct {
	V              int               `json:"v"`
	ID             string            `json:"id"`
	TS             string            `json:"ts"`
	Machine        string            `json:"machine"`
	User           string            `json:"user,omitempty"`
	Agent          string            `json:"agent"`
	Event          string            `json:"event"`
	SessionID      string            `json:"session_id,omitempty"`
	Lead           string            `json:"lead,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	TranscriptPath string            `json:"transcript_path,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvNames       []string          `json:"env_names,omitempty"`
	Payload        json.RawMessage   `json:"payload,omitempty"`
	Item           *Item             `json:"item,omitempty"`
	Done           *Done             `json:"done,omitempty"`
}

// Item is something an agent filed for the human.
type Item struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"` // ask | review | human_task | issue | fyi
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	Options   []string   `json:"options,omitempty"`
	Recommend string     `json:"recommend,omitempty"`
	Blocking  bool       `json:"blocking,omitempty"`
	Blocks    string     `json:"blocks,omitempty"`
	Physical  bool       `json:"physical,omitempty"`
	Due       string     `json:"due,omitempty"`
	Key       string     `json:"key,omitempty"`
	Evidence  []Evidence `json:"evidence,omitempty"`
}

type Evidence struct {
	SHA256 string `json:"sha256"`
	Name   string `json:"name"`
	Mime   string `json:"mime,omitempty"`
	Size   int64  `json:"size"`
}

// Done marks a previously filed item resolved by the agent.
type Done struct {
	ItemID string `json:"item_id"`
	Note   string `json:"note,omitempty"`
}

// New starts a record for event with the fields every record shares.
func New(event string, getenv func(string) string) Record {
	now := time.Now()
	r := Record{
		V:       Version,
		ID:      ulid.New(now),
		TS:      now.UTC().Format(time.RFC3339Nano),
		Machine: config.Machine(),
		User:    getenv("USER"),
		Agent:   "claude",
		Event:   event,
		Lead:    getenv("DOCKET_LEAD"),
	}
	r.Env, r.EnvNames = captureEnv(getenv)
	return r
}

// Environment values worth keeping: where the session runs (tmux), which
// lead it is, and Claude Code's own non-secret session metadata.
var keepEnv = []string{"TMUX", "TMUX_PANE", "DOCKET_LEAD", "DOCKET_SESSION", "TERM_PROGRAM"}

var secretish = regexp.MustCompile(`(?i)TOKEN|KEY|SECRET|AUTH|PASS|CRED|COOKIE`)

func captureEnv(getenv func(string) string) (map[string]string, []string) {
	env := map[string]string{}
	for _, k := range keepEnv {
		if v := getenv(k); v != "" {
			env[k] = v
		}
	}
	// Record which CLAUDE* variables exist (names only, plus short values of
	// clearly non-secret ones) so session metadata such as a Remote Control
	// id can be found without ever storing credentials.
	var names []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "CLAUDE") {
			continue
		}
		names = append(names, k)
		if !secretish.MatchString(k) && len(v) <= 200 && getenv(k) == v {
			env[k] = v
		}
	}
	sort.Strings(names)
	if len(env) == 0 {
		env = nil
	}
	return env, names
}

// CapPayload re-encodes a hook payload with every long string truncated, so
// one huge message can't bloat the spool. Input that isn't JSON is kept as a
// string under "unparsable".
func CapPayload(raw []byte) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		s := string(raw)
		if len(s) > MaxString {
			s = s[:MaxString] + fmt.Sprintf("…[truncated %d bytes]", len(raw)-MaxString)
		}
		b, _ := json.Marshal(map[string]any{"unparsable": s})
		return b
	}
	b, err := json.Marshal(capStrings(v))
	if err != nil {
		return nil
	}
	return b
}

func capStrings(v any) any {
	switch t := v.(type) {
	case string:
		if len(t) > MaxString {
			cut := MaxString
			for cut > 0 && (t[cut]&0xC0) == 0x80 { // don't split a UTF-8 character
				cut--
			}
			return t[:cut] + fmt.Sprintf("…[truncated %d bytes]", len(t)-cut)
		}
		return t
	case []any:
		for i := range t {
			t[i] = capStrings(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = capStrings(t[k])
		}
		return t
	default:
		return v
	}
}
