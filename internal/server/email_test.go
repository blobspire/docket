package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/mail/mailtest"
)

// emailEnv is a server with one mailbox on a throwaway IMAP/SMTP pair.
func emailEnv(t *testing.T, extra string) (*env, string, *mailtest.SMTP) {
	t.Helper()
	imapAddr := mailtest.IMAP(t)
	smtp := mailtest.NewSMTP(t)
	pw := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(pw, []byte("password"), 0o600)
	e := newEnv(t, `person = "Cole"
[[machine]]
name = "box"
mode = "local"
[email]
`+extra+`
[[email.account]]
name = "test"
address = "me@example.com"
user = "username"
imap = "`+imapAddr+`"
smtp = "`+smtp.Addr+`"
security = "plain"
password_file = "`+pw+`"
`)
	return e, imapAddr, smtp
}

func msg(id, from, to, subject, body string, extra ...string) string {
	return "From: " + from + "\nTo: " + to + "\nSubject: " + subject + "\nDate: " + time.Now().Format(time.RFC1123Z) +
		"\nMessage-ID: <" + id + ">\n" + strings.Join(extra, "\n") + "\nContent-Type: text/plain; charset=utf-8\n\n" + body + "\n"
}

func TestReadAccountStoresMailAndLearnsWhoYouWriteTo(t *testing.T) {
	e, addr, _ := emailEnv(t, "")
	now := time.Now()
	mailtest.Append(t, addr, "Sent", msg("s1@x", "Cole <me@example.com>", "Nancy <nancy@example.org>", "Site", "hi"), now.AddDate(0, -2, 0))
	mailtest.Append(t, addr, "INBOX", msg("n1@x", "Nancy <nancy@example.org>", "me@example.com", "Undo?", "Can we undo a removal?"), now)
	a := e.s.cfg.Email.Accounts[0]
	r, err := e.s.readAccount(e.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var inbox int
	for _, m := range r.New {
		if m.Folder == "INBOX" {
			inbox++
		}
	}
	if inbox < 1 {
		t.Fatalf("new: %+v", r.New)
	}
	if !e.s.st.Known("test", "nancy@example.org") {
		t.Fatal("two-month-old Sent mail should make Nancy a known sender")
	}
	if e.s.st.Known("test", "me@example.com") {
		t.Fatal("you aren't your own contact")
	}
	st := e.s.st.EmailAccountState("test")
	if st.SentFolder != "Sent" || st.ContactsAt == 0 || st.Error != "" || st.OKAt == 0 {
		t.Fatalf("state: %+v", st)
	}
	// Nothing new on a second read; a new message is picked up.
	r2, err := e.s.readAccount(e.ctx, a)
	if err != nil || len(r2.New) != 0 {
		t.Fatalf("second read: %d %v", len(r2.New), err)
	}
	mailtest.Append(t, addr, "INBOX", msg("n2@x", "Nancy <nancy@example.org>", "me@example.com", "Re: Undo?", "Also...", "In-Reply-To: <n1@x>", "References: <n1@x>"), now)
	r3, _ := e.s.readAccount(e.ctx, a)
	if len(r3.New) != 1 || r3.New[0].Thread != "n1@x" {
		t.Fatalf("third read: %+v", r3.New)
	}
}

func TestReadAccountRecordsAFailure(t *testing.T) {
	e, _, _ := emailEnv(t, "")
	a := e.s.cfg.Email.Accounts[0]
	a.IMAP = "127.0.0.1:1"
	if _, err := e.s.readAccount(e.ctx, a); err == nil {
		t.Fatal("expected a connect failure")
	}
	if st := e.s.st.EmailAccountState("test"); st.Error == "" || st.ErrorAt == 0 {
		t.Fatalf("failure not recorded: %+v", st)
	}
}

func TestEmailCheckSendsNothing(t *testing.T) {
	e, _, smtp := emailEnv(t, "")
	var out bytes.Buffer
	if err := e.s.EmailCheck(context.Background(), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "read: ok") || !strings.Contains(out.String(), "send: ok (logged in, nothing sent)") {
		t.Fatalf("output:\n%s", out.String())
	}
	if len(smtp.Messages()) != 0 {
		t.Fatal("the check sent mail")
	}
}

func TestEmailConfigDefaultsAndChecks(t *testing.T) {
	e, _, _ := emailEnv(t, "")
	c := e.s.cfg.Email
	if len(c.Schedule) != 3 || c.TriageModel != "claude-sonnet-5-5" || c.DraftModel != "claude-opus-5-5" || c.DigestDays != 3 ||
		c.SendHold.Duration != 30*time.Second || c.MaxRunning != 4 {
		t.Fatalf("defaults: %+v", c)
	}
	path := filepath.Join(t.TempDir(), "bad.toml")
	os.WriteFile(path, []byte(`[[email.account]]
name = "gmail"
address = "me@gmail.com"
imap = "imap.gmail.com:143"
smtp = "smtp.gmail.com:587"
security = "plain"
password_file = "/x"
`), 0o600)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "only allowed for a gateway") {
		t.Fatalf("plain text to Gmail must be refused: %v", err)
	}
}

// One folder that can't be read doesn't stop the others.
func TestAFailingFolderDoesntBlockTheOthers(t *testing.T) {
	e, addr, _ := emailEnv(t, "")
	mailtest.Append(t, addr, "INBOX", msg("n1@x", "Nancy <nancy@example.org>", "me@example.com", "Hi", "hi"), time.Now())
	a := e.s.cfg.Email.Accounts[0]
	a.Folders = []string{"Missing", "INBOX"}
	r, err := e.s.readAccount(e.ctx, a)
	if err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Fatalf("the missing folder should be reported: %v", err)
	}
	if len(r.New) != 1 || r.New[0].MessageID != "n1@x" {
		t.Fatalf("INBOX should still be read: %+v", r.New)
	}
	if c := e.s.st.EmailCursor("test", "INBOX"); c.LastUID == 0 {
		t.Fatal("INBOX's cursor should advance")
	}
}
