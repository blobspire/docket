package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/mailbox"
	"github.com/blobspire/docket/internal/sessions"
	"github.com/blobspire/docket/internal/spool"
	"github.com/blobspire/docket/internal/store"
)

const exportBudget = "4000000"

// remote runs `docket <args>` on a machine: directly for the server's own
// machine, over the system ssh (so ~/.ssh/config, VPN routes and keys all
// apply) for pull machines.
func (s *Server) remote(ctx context.Context, m Machine, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	switch m.Mode {
	case "local":
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return localRun(stdin, args...)
	case "pull":
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = shellQuote(a)
		}
		// The docket path is left unquoted so a leading ~ expands remotely.
		cmd = exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=10",
			m.SSH, m.Docket+" "+strings.Join(quoted, " "))
	default:
		return nil, fmt.Errorf("machine %s is a %s machine; the server can't reach it", m.Name, m.Mode)
	}
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(clipErr(msg))
	}
	return out.Bytes(), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func clipErr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// parseExport splits `docket spool export` output into records and trailer.
func parseExport(r io.Reader) ([]store.RawEvent, spool.Trailer, error) {
	var evs []store.RawEvent
	var tr spool.Trailer
	gotTrailer := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var head struct {
			ID        string  `json:"id"`
			TS        string  `json:"ts"`
			Event     string  `json:"event"`
			SessionID string  `json:"session_id"`
			Cursor    *string `json:"_cursor"`
			More      bool    `json:"_more"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			continue
		}
		if head.Cursor != nil {
			tr = spool.Trailer{Cursor: *head.Cursor, More: head.More}
			gotTrailer = true
			continue
		}
		if head.ID == "" {
			continue
		}
		evs = append(evs, store.RawEvent{ID: head.ID, TS: head.TS, Event: head.Event, SessionID: head.SessionID, Body: append([]byte(nil), line...)})
	}
	if err := sc.Err(); err != nil {
		return nil, tr, err
	}
	if !gotTrailer {
		return nil, tr, errors.New("export ended without a trailer (truncated?)")
	}
	return evs, tr, nil
}

// pull collects everything new from a local or pull machine.
func (s *Server) pull(ctx context.Context, m Machine) error {
	cur, err := s.st.Cursor(m.Name)
	if err != nil {
		return err
	}
	for page := 0; page < 100; page++ {
		out, err := s.remote(ctx, m, nil, "spool", "export", "--cursor", cur, "--max", exportBudget)
		if err != nil {
			s.st.MachineError(m.Name, err.Error())
			return err
		}
		evs, tr, err := parseExport(bytes.NewReader(out))
		if err != nil {
			s.st.MachineError(m.Name, err.Error())
			return err
		}
		n, err := s.st.IngestBatch(m.Name, evs, tr.Cursor)
		if err != nil {
			return err
		}
		if n > 0 {
			s.kickProcess()
		}
		if tr.Cursor != "" && tr.Cursor != cur {
			if _, err := s.remote(ctx, m, nil, "spool", "ack", tr.Cursor); err != nil {
				s.log.Printf("%s: ack: %v", m.Name, err)
			}
		}
		cur = tr.Cursor
		if !tr.More {
			break
		}
	}
	if out, err := s.remote(ctx, m, nil, "sessions", "--json"); err == nil {
		s.applySessions(m.Name, out)
	} else {
		s.log.Printf("%s: sessions: %v", m.Name, err)
	}
	s.fetchBlobs(ctx, m)
	s.collectHealth(ctx, m)
	return nil
}

// applySessions merges a machine's live-session registry into the store.
func (s *Server) applySessions(machine string, data []byte) {
	var live []sessions.Live
	if json.Unmarshal(data, &live) != nil {
		return
	}
	seen := map[string]bool{}
	for _, l := range live {
		seen[l.ID] = true
		state := l.Status
		if state == "" {
			state = "idle"
		}
		patch := store.SessionPatch{Machine: store.Str(machine), Cwd: store.Str(l.Cwd), Name: store.Str(l.Name), State: store.Str(state),
			PID: store.Int(int64(l.PID)), Kind: store.Str(l.Kind), WaitingFor: store.Str(l.Waiting)}
		prev, _, _ := s.st.GetSession(l.ID)
		switch {
		case state == "waiting" && prev.WaitingSince == 0:
			patch.WaitingSince = store.Int(time.Now().Unix())
		case state != "waiting" && prev.WaitingSince != 0:
			patch.WaitingSince = store.Int(0)
		}
		if l.Tmux != "" {
			patch.Tmux = store.Str(sessions.PaneOf(l.Tmux))
		}
		if l.BridgeID != "" {
			patch.BridgeID = store.Str(l.BridgeID)
		}
		if prev, ok, _ := s.st.GetSession(l.ID); !ok || prev.LastEventAt == 0 {
			patch.LastEventAt = store.Int(time.Now().Unix())
		}
		if err := s.st.PatchSession(l.ID, patch); err != nil {
			s.log.Printf("%s: session %s: %v", machine, short(l.ID), err)
		}
	}
	// Sessions of this machine that the registry no longer lists have exited.
	known, err := s.st.Sessions(false, 500)
	if err != nil {
		s.log.Printf("%s: sessions: %v", machine, err)
	}
	for _, k := range known {
		if k.Machine == machine && !seen[k.ID] && time.Since(time.Unix(k.LastEventAt, 0)) > 2*time.Minute {
			s.st.PatchSession(k.ID, store.SessionPatch{State: store.Str("ended"), EndedAt: store.Int(time.Now().Unix())})
		}
	}
	s.st.Bump()
}

func (s *Server) blobPath(sha string) string { return filepath.Join(s.cfg.DataDir, "blobs", sha) }

// fetchBlobs copies evidence files the server hasn't got yet.
func (s *Server) fetchBlobs(ctx context.Context, m Machine) {
	missing, err := s.st.MissingBlobs(m.Name)
	if err != nil {
		return
	}
	for _, sha := range missing {
		data, err := s.remote(ctx, m, nil, "spool", "blob", sha)
		if err != nil {
			s.log.Printf("%s: blob %s: %v", m.Name, sha[:12], err)
			continue
		}
		if err := s.saveBlob(sha, bytes.NewReader(data)); err != nil {
			s.log.Printf("%s: blob %s: %v", m.Name, sha[:12], err)
		}
	}
}

// saveBlob stores a blob after checking its hash.
func (s *Server) saveBlob(sha string, r io.Reader) error {
	if len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
		return errors.New("bad blob hash")
	}
	dir := filepath.Join(s.cfg.DataDir, "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".in-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return fmt.Errorf("blob hash mismatch (got %s)", got[:12])
	}
	if err := os.Rename(tmp.Name(), s.blobPath(sha)); err != nil {
		return err
	}
	return s.st.BlobFetched(sha)
}

// ---- push machines --------------------------------------------------------

// SyncReply is what the server returns to `docket sync`.
type SyncReply struct {
	Cursor     string             `json:"cursor"`
	Deliveries []mailbox.Delivery `json:"deliveries"`
	WantBlobs  []string           `json:"want_blobs"`
}

// Receive ingests a push machine's export (read from r) and its session
// registry, and hands back the answers waiting for its sessions.
func (s *Server) Receive(machine string, r io.Reader, sessionsJSON []byte) (SyncReply, error) {
	m, ok := s.cfg.Machine(machine)
	if !ok || m.Mode != "push" {
		return SyncReply{}, fmt.Errorf("machine %q isn't configured with mode = \"push\"", machine)
	}
	evs, tr, err := parseExport(r)
	if err != nil {
		return SyncReply{}, err
	}
	if _, err := s.st.IngestBatch(machine, evs, tr.Cursor); err != nil {
		return SyncReply{}, err
	}
	if len(sessionsJSON) > 0 {
		s.applySessions(machine, sessionsJSON)
	}
	reply := SyncReply{Cursor: tr.Cursor}
	pending, err := s.st.Deliveries("pending")
	if err != nil {
		return reply, err
	}
	for _, d := range pending {
		if d.Machine != machine {
			continue
		}
		// Hand it over even if the session has ended: resuming it later runs
		// the SessionStart hook, which delivers what is waiting.
		sess, _, _ := s.st.GetSession(d.SessionID)
		if sess.HasPlugin {
			reply.Deliveries = append(reply.Deliveries, mailbox.Delivery{ID: d.ID, ItemID: d.ItemID, SessionID: d.SessionID, Nonce: d.Nonce, Text: d.Text})
			// Writing a local mailbox file can't meaningfully fail; if it ever
			// does, the missing receipt makes the deliverer resend it, and the
			// mailbox ignores ids it has already delivered.
			s.st.DeliverySent(d.ID, "mailbox")
		}
	}
	reply.WantBlobs, _ = s.st.MissingBlobs(machine)
	s.st.Bump()
	return reply, nil
}

// PutBlob stores an evidence file pushed by `docket sync`.
func (s *Server) PutBlob(sha string, r io.Reader) error { return s.saveBlob(sha, r) }

// BackfillText is the one-time prompt asking a live session to file what it
// is already waiting on.
func (s *Server) BackfillText() string {
	return fmt.Sprintf("[docket] One-time setup: %[1]s now uses docket, a single queue for everything agents need from %[1]s. "+
		"Please file each thing you are currently waiting on %[1]s for (held approvals, blockers, decisions, human-only tasks, open issues), "+
		"one item each, with `~/.local/bin/docket ask \"<question>\" --options \"a|b\" --why \"<context>\" --key <short-stable-id>` "+
		"(or `docket task` / `docket issue`; run `~/.local/bin/docket help` for details), then carry on with your work. "+
		"Filing never blocks. If nothing is waiting on %[1]s, just say so and continue.", s.cfg.Person)
}

// BackfillPrompt sends BackfillText to every live, idle session the server
// can type into safely. It returns one line per session describing the result.
func (s *Server) BackfillPrompt(ctx context.Context, dryRun bool) []string {
	var report []string
	live, _ := s.st.Sessions(false, 500)
	for _, x := range live {
		m, ok := s.cfg.Machine(x.Machine)
		label := x.Machine + "/" + firstNonEmptyStr(x.Name, x.ID)
		switch {
		case !ok || m.Mode == "push":
			report = append(report, label+": skipped (the server can't reach it; it will file going forward)")
			continue
		case x.Tmux == "":
			report = append(report, label+": skipped (not in tmux)")
			continue
		case x.State != "idle" && x.State != "waiting":
			report = append(report, label+": skipped ("+x.State+")")
			continue
		}
		if dryRun {
			report = append(report, label+": would prompt")
			continue
		}
		if _, err := s.remote(ctx, m, strings.NewReader(s.BackfillText()), "session", "send", "--pane", x.Tmux); err != nil {
			report = append(report, label+": not sent ("+err.Error()+")")
			continue
		}
		report = append(report, label+": prompted")
	}
	return report
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
