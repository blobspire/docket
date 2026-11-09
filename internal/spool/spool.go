// Package spool is docket's per-machine, append-only event log.
//
// Hooks and the CLI append one JSON object per line to hourly segment files
// (seg-YYYYMMDDHH.jsonl, UTC). The server pulls with Export, which returns
// only complete lines after a cursor and ends with a trailer naming the next
// cursor; after storing what it received, the server calls Ack, and segments
// that are fully acknowledged and older than the retention period are
// deleted. Delivery is at-least-once: the server de-duplicates by record id.
package spool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	prefix    = "seg-"
	suffix    = ".jsonl"
	ackedFile = "acked"
	// A partial last line in an older segment is treated as abandoned once
	// the file has been quiet this long (a writer crashed mid-line).
	abandonAfter = time.Minute
)

type Spool struct{ Dir string }

// SegmentName is the segment a record written at t belongs in.
func SegmentName(t time.Time) string {
	return prefix + t.UTC().Format("2006010215") + suffix
}

// Append writes v as one JSON line to the current segment.
func (s Spool) Append(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.AppendLine(line)
}

// AppendLine writes one line (a newline is added) under an exclusive lock,
// so concurrent writers never interleave.
func (s Spool) AppendLine(line []byte) error {
	if bytes.IndexByte(line, '\n') >= 0 {
		return errors.New("spool: line contains a newline")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, SegmentName(time.Now())), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_, err = f.Write(append(line, '\n'))
	return err
}

// Cursor is a position in the spool: a segment name and a byte offset in it.
type Cursor struct {
	Seg string
	Off int64
}

func (c Cursor) String() string {
	if c.Seg == "" {
		return ""
	}
	return c.Seg + ":" + strconv.FormatInt(c.Off, 10)
}

// ParseCursor parses "seg-YYYYMMDDHH.jsonl:123". The empty string is the start.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return Cursor{}, fmt.Errorf("spool: bad cursor %q", s)
	}
	off, err := strconv.ParseInt(s[i+1:], 10, 64)
	if err != nil || off < 0 || !isSegment(s[:i]) {
		return Cursor{}, fmt.Errorf("spool: bad cursor %q", s)
	}
	return Cursor{Seg: s[:i], Off: off}, nil
}

func isSegment(name string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && !strings.ContainsRune(name, '/')
}

// Segments lists segment file names, oldest first.
func (s Spool) Segments() ([]string, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var segs []string
	for _, e := range ents {
		if !e.IsDir() && isSegment(e.Name()) {
			segs = append(segs, e.Name())
		}
	}
	sort.Strings(segs)
	return segs, nil
}

// Trailer is the last line of an export.
type Trailer struct {
	Cursor string `json:"_cursor"`
	More   bool   `json:"_more"`
}

// Export writes the complete lines after `from` to w, stopping once about
// max bytes have been written (always at least one line), then writes a
// Trailer line. Records without an "id" (raw fallback lines written by the
// plugin shim when the binary is missing) get a stable one derived from
// their position, so re-exports de-duplicate.
func (s Spool) Export(w io.Writer, from Cursor, max int64) (Trailer, error) {
	segs, err := s.Segments()
	if err != nil {
		return Trailer{}, err
	}
	bw := bufio.NewWriter(w)
	next := from
	var written int64
	more := false
	for i, seg := range segs {
		if from.Seg != "" && seg < from.Seg {
			continue
		}
		start := int64(0)
		if seg == from.Seg {
			start = from.Off
		}
		next = Cursor{Seg: seg, Off: start}
		path := filepath.Join(s.Dir, seg)
		data, info, err := readFrom(path, start)
		if err != nil {
			return Trailer{}, err
		}
		complete := data
		if j := bytes.LastIndexByte(data, '\n'); j >= 0 {
			complete = data[:j+1]
		} else {
			complete = nil
		}
		partial := int64(len(data) - len(complete))
		off := start
		for len(complete) > 0 {
			if written > 0 && written >= max {
				more = true
				break
			}
			j := bytes.IndexByte(complete, '\n')
			line := complete[:j]
			if len(bytes.TrimSpace(line)) > 0 {
				out := withID(line, seg, off)
				if _, err := bw.Write(out); err != nil {
					return Trailer{}, err
				}
				if err := bw.WriteByte('\n'); err != nil {
					return Trailer{}, err
				}
				written += int64(len(out) + 1)
			}
			off += int64(j + 1)
			complete = complete[j+1:]
		}
		next = Cursor{Seg: seg, Off: off}
		if more {
			break
		}
		last := i == len(segs)-1
		if partial > 0 {
			if last || time.Since(info.ModTime()) < abandonAfter {
				// A writer may still be finishing this line; resume here next time.
				more = !last
				break
			}
			next.Off += partial // abandoned partial line from a crashed writer
		}
	}
	t := Trailer{Cursor: next.String(), More: more}
	tb, _ := json.Marshal(t)
	if _, err := bw.Write(append(tb, '\n')); err != nil {
		return Trailer{}, err
	}
	return t, bw.Flush()
}

func readFrom(path string, off int64) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if off > info.Size() {
		return nil, info, fmt.Errorf("spool: cursor offset %d is past the end of %s (%d bytes)", off, filepath.Base(path), info.Size())
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	return data, info, err
}

func withID(line []byte, seg string, off int64) []byte {
	var probe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(line, &probe) == nil && probe.ID != "" {
		return line
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		// Not a JSON object; wrap it so the server can still see it.
		b, _ := json.Marshal(map[string]any{"id": rawID(seg, off), "v": 0, "event": "unparsable", "raw": string(line)})
		return b
	}
	idField := []byte(`{"id":"` + rawID(seg, off) + `"`)
	rest := bytes.TrimSpace(trimmed[1:])
	if len(rest) > 0 && rest[0] != '}' {
		idField = append(idField, ',')
	}
	return append(idField, rest...)
}

func rawID(seg string, off int64) string {
	return "raw-" + strings.TrimSuffix(strings.TrimPrefix(seg, prefix), suffix) + "-" + strconv.FormatInt(off, 10)
}

// Ack records that everything before c has been stored by the server, then
// deletes segments that are fully acknowledged and older than retention.
func (s Spool) Ack(c Cursor, retention time.Duration) error {
	if c.Seg == "" {
		return nil
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, ackedFile+".tmp")
	if err := os.WriteFile(tmp, []byte(c.String()+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.Dir, ackedFile)); err != nil {
		return err
	}
	segs, err := s.Segments()
	if err != nil {
		return err
	}
	for _, seg := range segs {
		if seg >= c.Seg {
			break
		}
		p := filepath.Join(s.Dir, seg)
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) > retention {
			os.Remove(p)
		}
	}
	return nil
}

// Acked returns the last acknowledged cursor (the start if none).
func (s Spool) Acked() Cursor {
	b, err := os.ReadFile(filepath.Join(s.Dir, ackedFile))
	if err != nil {
		return Cursor{}
	}
	c, err := ParseCursor(strings.TrimSpace(string(b)))
	if err != nil {
		return Cursor{}
	}
	return c
}

// Tail returns up to n of the most recent complete lines.
func (s Spool) Tail(n int) ([][]byte, error) {
	segs, err := s.Segments()
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for i := len(segs) - 1; i >= 0 && len(out) < n; i-- {
		data, err := os.ReadFile(filepath.Join(s.Dir, segs[i]))
		if err != nil {
			return nil, err
		}
		if j := bytes.LastIndexByte(data, '\n'); j >= 0 {
			data = data[:j]
		} else {
			continue
		}
		lines := bytes.Split(data, []byte{'\n'})
		for k := len(lines) - 1; k >= 0 && len(out) < n; k-- {
			if len(bytes.TrimSpace(lines[k])) > 0 {
				out = append(out, lines[k])
			}
		}
	}
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out, nil
}
