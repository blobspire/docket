package mail

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/blobspire/docket/internal/mail/mailtest"
)

// A big backlog must not be downloaded whole: the newest max are chosen
// from dates and sizes first, and only their bodies are fetched, in
// batches (each its own command, with its own deadline).
func TestFetchChoosesTheNewestBeforeDownloading(t *testing.T) {
	addr := mailtest.IMAP(t)
	a := testAccount(t, addr, "127.0.0.1:1")
	base := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 60; i++ {
		raw := fmt.Sprintf("From: x@example.org\nTo: me@example.com\nSubject: m%02d\nMessage-ID: <m%02d@x>\n\nbody %d\n", i, i, i)
		mailtest.Append(t, addr, "INBOX", raw, base.Add(time.Duration(i)*time.Minute))
	}
	c, err := a.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wire bytes.Buffer
	c.c.SetDebug(&wire)
	f, err := c.Fetch("INBOX", Cursor{}, time.Time{}, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Messages) != 5 || f.Dropped != 55 || f.Messages[0].Subject != "m55" || f.Messages[4].Subject != "m59" {
		var got []string
		for _, m := range f.Messages {
			got = append(got, m.Subject)
		}
		t.Fatalf("got %v dropped %d", got, f.Dropped)
	}
	bodyFetches := regexp.MustCompile(`UID FETCH (\S+) \(UID BODY\.PEEK\[\]\)`).FindAllStringSubmatch(wire.String(), -1)
	if len(bodyFetches) != 1 || strings.Count(bodyFetches[0][1], ",")+strings.Count(bodyFetches[0][1], ":") > 4 {
		t.Fatalf("bodies should be fetched for the 5 chosen only: %v", bodyFetches)
	}
	// Without a limit, 60 bodies come in batches of at most 25.
	wire.Reset()
	f, err = c.Fetch("INBOX", Cursor{}, time.Time{}, false, 0)
	if err != nil || len(f.Messages) != 60 {
		t.Fatalf("all: %d %v", len(f.Messages), err)
	}
	if n := len(regexp.MustCompile(`UID BODY\.PEEK\[\]\)`).FindAllString(wire.String(), -1)); n != 3 {
		t.Fatalf("expected 3 body batches, got %d", n)
	}
}

// A message too big to download whole shows its real first text part,
// decoded, and lists its attachments.
func TestFetchReadsTheTextOfAHugeMessage(t *testing.T) {
	addr := mailtest.IMAP(t)
	a := testAccount(t, addr, "127.0.0.1:1")
	blob := strings.Repeat("QUJDRA==\r\n", maxBody/10+1000)
	raw := "From: Nancy <nancy@example.org>\r\nTo: me@example.com\r\nSubject: big one\r\nMessage-ID: <big@x>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"mix\"\r\n\r\n--mix\r\nContent-Type: multipart/alternative; boundary=\"alt\"\r\n\r\n" +
		"--alt\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHi Cole, caf=C3=A9 plan attached, soft wr=\r\napped.\r\n" +
		"--alt\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hi Cole</p>\r\n--alt--\r\n" +
		"--mix\r\nContent-Type: application/pdf; name=\"plan.pdf\"\r\nContent-Disposition: attachment; filename=\"plan.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + blob + "--mix--\r\n"
	mailtest.Append(t, addr, "INBOX", raw, time.Now())
	c, err := a.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wire bytes.Buffer
	c.c.SetDebug(&wire)
	f, err := c.Fetch("INBOX", Cursor{}, time.Time{}, false, 10)
	if err != nil || len(f.Messages) != 1 {
		t.Fatalf("%d %v", len(f.Messages), err)
	}
	m := f.Messages[0]
	if m.Text != "Hi Cole, café plan attached, soft wrapped." || m.Subject != "big one" || m.From.Email != "nancy@example.org" {
		t.Fatalf("message: %q %+v", m.Text, m)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "plan.pdf" {
		t.Fatalf("attachments: %+v", m.Attachments)
	}
	if strings.Contains(wire.String(), "BODY[") && !strings.Contains(wire.String(), "BODY.PEEK[1.1]") {
		t.Fatalf("must peek at the text part only:\n%.2000s", wire.String())
	}
	if regexp.MustCompile(`FETCH \S+ \([^)]*BODY\[`).MatchString(wire.String()) {
		t.Fatal("a non-peek fetch would mark the message read")
	}
}

// A hostile message nested thousands of levels deep is read in no time,
// headers only.
func TestParseCapsDeepNesting(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: x@example.org\r\nSubject: deep\r\nMessage-ID: <d@x>\r\nContent-Type: multipart/mixed; boundary=\"b0\"\r\n\r\n")
	const depth = 4000
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "--b%d\r\nContent-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n", i, i+1)
	}
	b.WriteString("--b4000\r\nContent-Type: text/plain\r\n\r\nhi\r\n")
	t0 := time.Now()
	m, _ := Parse([]byte(b.String()))
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("took %s", d)
	}
	if m.Subject != "deep" || !strings.Contains(m.Text, "unusual structure") {
		t.Fatalf("got %+v", m)
	}
}

func TestHTMLTextKeepsSmallButVisibleText(t *testing.T) {
	for in, want := range map[string]string{
		`<div style="font-size:0.9em">Call me about the grant</div>`:          "Call me about the grant",
		`<p style="opacity:0.8">Meeting moved to 3pm</p>`:                     "Meeting moved to 3pm",
		`<p style="font-size: 11px; opacity: .95">fine print</p>`:             "fine print",
		`<span style="font-size:1px">tiny</span><p>ok</p>`:                    "ok",
		`<span style="opacity:0">gone</span><p>ok</p>`:                        "ok",
		`<div style="display: none !important">imp</div><p>ok</p>`:            "ok",
		`<div style="max-height:0;overflow:hidden">pre</div><p>ok</p>`:        "ok",
		`<div style="visibility:hidden">x</div><p style="font-size:0">y</p>z`: "z",
	} {
		if got := HTMLText(in); got != want {
			t.Errorf("%s => %q, want %q", in, got, want)
		}
	}
}
