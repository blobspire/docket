package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/blobspire/docket/internal/ingest"
	"github.com/blobspire/docket/internal/mail"
	"github.com/blobspire/docket/internal/store"
	"github.com/blobspire/docket/internal/ulid"
)

// Email triage, server side. docket itself reads and sends mail; the
// models it runs never get a mail tool (see docs/email.md and SECURITY.md).

// emailRead is what one read of an account found.
type emailRead struct {
	New     []store.EmailMessage // messages docket hadn't stored
	Skipped int                  // matched the account's skip search (Gmail tabs)
	Dropped int                  // over max_per_pass; the oldest were left
}

// readAccount reads an account's triage folders and its Sent folder. A
// folder that fails is skipped (and reported in the error): the messages it
// returns are stored either way, so callers must use them even on error. The
// first read of Sent learns who you write to (a year of envelopes), which is
// how docket tells known senders from first-time ones.
func (s *Server) readAccount(ctx context.Context, a mail.Account) (emailRead, error) {
	var out emailRead
	err := func() error {
		c, err := a.Dial(ctx)
		if err != nil {
			return err
		}
		defer c.Close()
		sent, drafts, _, err := c.Folders()
		if err != nil {
			return err
		}
		s.st.SetEmailFolders(a.Name, sent, drafts)
		now := time.Now()
		back, _ := parseEvery(s.cfg.Email.Backfill)
		since := now.Add(-back)
		st := s.st.EmailAccountState(a.Name)
		if sent != "" && st.ContactsAt == 0 {
			// Best effort: who you've written to only decides whether a
			// sender counts as known, so a failure here is logged, retried
			// on the next read, and never holds up triage.
			rcpt, cur, err := c.Recipients(sent, now.AddDate(-1, 0, 0))
			s.st.AddContacts(a.Name, rcpt)
			if err != nil {
				s.log.Printf("email %s: reading who you've written to: %v (retrying next time)", a.Name, err)
			} else {
				s.st.SetContactsScanned(a.Name)
			}
			// Later reads of Sent start a few days back, for threads you
			// answered recently, then follow the cursor.
			if s.st.EmailCursor(a.Name, sent).UIDValidity == 0 {
				s.st.SetEmailCursor(a.Name, sent, mail.Cursor{UIDValidity: cur.UIDValidity})
			}
		}
		// Sent first: a reply to your reply then finds your message already
		// stored and joins its conversation.
		var folders []string
		if sent != "" {
			folders = append(folders, sent)
		}
		for _, f := range a.Folders {
			if !strings.EqualFold(f, sent) {
				folders = append(folders, f)
			}
		}
		// A folder that fails is retried next time; the others are still read.
		var folderErrs []string
		for _, f := range folders {
			isSent := strings.EqualFold(f, sent)
			got, err := c.Fetch(f, s.st.EmailCursor(a.Name, f), since, !isSent, s.cfg.Email.MaxPerPass)
			if err != nil {
				folderErrs = append(folderErrs, err.Error())
				continue
			}
			added, err := s.st.AddEmail(a.Name, got.Messages, a.Me)
			if err != nil {
				folderErrs = append(folderErrs, a.Name+": store "+f+": "+err.Error())
				continue
			}
			if isSent {
				rcpt := map[string]int64{}
				for _, m := range added {
					for _, x := range append(append([]mail.Addr{}, m.To...), m.Cc...) {
						if !a.Me(x.Email) {
							rcpt[x.Email] = m.Date
						}
					}
				}
				s.st.AddContacts(a.Name, rcpt)
			}
			if err := s.st.SetEmailCursor(a.Name, f, got.Cursor); err != nil {
				return err
			}
			out.New = append(out.New, added...)
			out.Skipped += got.Skipped
			out.Dropped += got.Dropped
		}
		// A later folder's message may have moved an earlier one's thread.
		for i := range out.New {
			s.st.DB.QueryRow(`SELECT thread FROM email_messages WHERE id=?`, out.New[i].ID).Scan(&out.New[i].Thread)
		}
		if len(folderErrs) > 0 {
			return errors.New(strings.Join(folderErrs, "; "))
		}
		return nil
	}()
	s.st.EmailAccountResult(a.Name, err)
	return out, err
}

// EmailCheck connects to every account (reading and sending) without
// changing anything, for `docket server email-check`.
func (s *Server) EmailCheck(ctx context.Context, w io.Writer) error {
	if !s.cfg.Email.On() {
		return fmt.Errorf("no [[email.account]] in %s", ConfigPath())
	}
	failed := 0
	for _, a := range s.cfg.Email.Accounts {
		fmt.Fprintf(w, "%s (%s)\n", a.Name, a.Address)
		c, err := a.Dial(ctx)
		if err != nil {
			fmt.Fprintf(w, "  read: FAILED: %v\n", err)
			failed++
			continue
		}
		sent, drafts, all, err := c.Folders()
		if err != nil {
			fmt.Fprintf(w, "  read: FAILED: %v\n", err)
			failed++
			c.Close()
			continue
		}
		fmt.Fprintf(w, "  read: ok, %d folders; Sent = %q, Drafts = %q\n", len(all), sent, drafts)
		for _, f := range a.Folders {
			if !hasFold(all, f) {
				fmt.Fprintf(w, "  folder %q: not found\n", f)
				failed++
			}
		}
		if q := a.SkipQuery(); q != "" {
			fmt.Fprintf(w, "  skips: %s\n", q)
		}
		c.Close()
		if err := a.CheckSMTP(ctx); err != nil {
			fmt.Fprintf(w, "  send: FAILED: %v\n", err)
			failed++
		} else {
			fmt.Fprintf(w, "  send: ok (logged in, nothing sent)\n")
		}
		if a.SignatureHTML != "" || a.SignatureText != "" {
			if sig, err := a.LoadSignature(false); err != nil {
				fmt.Fprintf(w, "  signature: FAILED: %v\n", err)
				failed++
			} else {
				fmt.Fprintf(w, "  signature: ok (%d inline image%s)\n", len(sig.Images), map[bool]string{true: "", false: "s"}[len(sig.Images) == 1])
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d problem%s", failed, map[bool]string{true: "", false: "s"}[failed == 1])
	}
	return nil
}

// EmailReadNow reads every account once and prints what it found (counts
// only), for `docket server email-check --read`.
func (s *Server) EmailReadNow(ctx context.Context, w io.Writer) error {
	for _, a := range s.cfg.Email.Accounts {
		t0 := time.Now()
		// On an error, what was read from the other folders is still stored
		// and reported.
		r, err := s.readAccount(ctx, a)
		if err != nil {
			fmt.Fprintf(w, "%s: %v\n", a.Name, err)
		}
		in, sent := 0, 0
		for _, m := range r.New {
			if m.FromMe {
				sent++
			} else {
				in++
			}
		}
		var contacts int
		s.st.DB.QueryRow(`SELECT COUNT(*) FROM email_contacts WHERE account=?`, a.Name).Scan(&contacts)
		fmt.Fprintf(w, "%s: read %d new (%d to you, %d from you), skipped %d, left %d over the limit, %d people you've written to, in %s\n",
			a.Name, len(r.New), in, sent, r.Skipped, r.Dropped, contacts, time.Since(t0).Round(time.Second))
	}
	return nil
}

func hasFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// accountWatch files one item when a mailbox can't be read, and closes it
// when reading works again. For a gateway that signs in with a device code
// (DavMail), the item carries the newest code from the gateway's log.
func (s *Server) accountWatch(a mail.Account, err error) {
	key := "email-account:" + a.Name
	if err == nil {
		s.resolveKey(key, "your "+a.Label+" mail can be read again")
		return
	}
	body := fmt.Sprintf("docket couldn't read your %s mail, so triage is skipping it until this is fixed.\n\n%s", a.Label, clipErr(err.Error()))
	if url, code, at, ok := signinCode(a.SigninLog); ok {
		left := time.Until(at.Add(15 * time.Minute))
		if left > 0 {
			body = fmt.Sprintf("Sign in to your %s mail again: open %s and enter **%s** (it works for about %d more minutes).\n\nIf it has expired, tap **Check now** under Email for a fresh code.\n\n%s",
				a.Label, url, code, int(left.Minutes())+1, clipErr(err.Error()))
		} else {
			body += "\n\nIt probably needs you to sign in again: tap **Check now** under Email and a fresh sign-in code will appear here."
		}
	}
	s.watch(store.Item{ID: ulid.ItemID(), Ongoing: true, Kind: "issue", Source: ingest.SrcWatchdog, DedupeKey: key,
		Project: s.cfg.SystemProject, Urgency: "normal", Title: "Can't read your " + a.Label + " mail", Body: body})
}

var signinRE = regexp.MustCompile(`open the page (https://\S+) and enter the code (\S+?)\.? to authenticate`)

// signinCode finds the newest device sign-in code in a gateway's log (the
// last 64 KB), and when the log was last written.
func signinCode(path string) (url, code string, at time.Time, ok bool) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	off := fi.Size() - 64<<10
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return
	}
	m := signinRE.FindAllStringSubmatch(string(buf), -1)
	if len(m) == 0 {
		return
	}
	last := m[len(m)-1]
	return last[1], last[2], fi.ModTime(), true
}
