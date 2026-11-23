package mail

import (
	"context"
	"mime"
	"net"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/blobspire/docket/internal/mail/mailtest"
)

// Every safety guard on the way out, each pinned so reverting it fails.

func TestCleanSqueezesEveryKindOfBlankRun(t *testing.T) {
	cases := map[string]string{
		"nbsp paras":       strings.Repeat("<p> </p>", 80),
		"nbsp entity":      strings.Repeat("<p>&nbsp;</p>", 80),
		"ideographic":      strings.Repeat("<p>　</p>", 80),
		"br strong":        strings.Repeat("<br><strong></strong>", 80),
		"br nbsp":          strings.Repeat("<br> ", 80),
		"empty link":       strings.Repeat(`<br><a href="https://x.test"></a>`, 80),
		"headings":         strings.Repeat("<h1> </h1>", 80),
		"empty strong p":   strings.Repeat("<p><strong></strong></p>", 80),
		"order":            strings.Repeat("<br><br><p></p>", 40),
		"empty list items": "<ul>" + strings.Repeat("<li> </li>", 80) + "</ul>",
		"space run":        "<p>a" + strings.Repeat(" ", 400) + "b</p>",
	}
	for name, mid := range cases {
		out, notes := Clean("<p>Thanks, Cole</p>" + mid + "<p>key=SECRET</p>")
		nodes, err := html.ParseFragment(strings.NewReader(out), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
		if err != nil || len(nodes) == 0 {
			t.Fatalf("%s: couldn't parse the output: %v", name, err)
		}
		var brRun, maxRun, blanks int
		var walk func(n *html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "br" {
				brRun++
				if brRun > maxRun {
					maxRun = brRun
				}
			} else if n.Type == html.TextNode && strings.TrimSpace(strings.Map(func(r rune) rune {
				if r == ' ' || r == '　' {
					return ' '
				}
				return r
			}, n.Data)) == "" {
			} else {
				brRun = 0
			}
			if n.Type == html.ElementNode && (n.Data == "p" || n.Data == "li") && blank(n) {
				blanks++
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		for _, n := range nodes {
			walk(n)
		}
		if maxRun > 2 || blanks > 0 || !strings.Contains(out, "key=SECRET") || strings.Count(out, " ") > 2 {
			t.Errorf("%s: %d breaks in a row, %d blank blocks: %.200q", name, maxRun, blanks, out)
		}
		if name != "space run" && len(notes) == 0 {
			t.Errorf("%s: no note", name)
		}
		if again, _ := Clean(out); again != out {
			t.Errorf("%s: cleaning twice changed it", name)
		}
	}
}

func TestStripInvisibleUsesUnicodesOwnLists(t *testing.T) {
	var bits strings.Builder
	for _, c := range "SECRET" {
		for i := 7; i >= 0; i-- {
			if c>>i&1 == 1 {
				bits.WriteRune('᠌')
			} else {
				bits.WriteRune('᠋')
			}
		}
	}
	in := "<p>Sounds good" + bits.String() + "͏឴឵᠍᠏⁥￰\U000e0080\U000e0fffㅤᅟ⠀" + "!</p>"
	out, notes := Clean(in)
	if out != "<p>Sounds good!</p>" {
		t.Fatalf("survivors: %+q", out)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "invisible") {
		t.Fatalf("notes: %v", notes)
	}
	// Ordinary text in any script stays.
	if got, n := StripInvisible("Café naïve 日本語 Ünïcödé — ✓"); n != 0 || got != "Café naïve 日本語 Ünïcödé — ✓" {
		t.Fatalf("stripped visible text: %q (%d)", got, n)
	}
}

func TestCleanEscapesTextAndLinks(t *testing.T) {
	out, _ := Clean(`<p>&lt;img src=x onerror=alert(1)&gt;</p><p><a href='https://x.test/?q="onmouseover="alert(1)'>x</a></p><p><a href="https:///nohost">h</a></p>` +
		`<p>&lt;a href="javascript:alert(1)"&gt;click&lt;/a&gt;</p>`)
	for _, styled := range []string{out, Style(out), Document(out, "", "", nil)} {
		nodes, err := html.ParseFragment(strings.NewReader(styled), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
		if err != nil || len(nodes) == 0 || !strings.Contains(styled, "x.test") {
			t.Fatalf("couldn't parse %q: %v", styled, err)
		}
		var walk func(n *html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode {
				if n.Data == "img" || n.Data == "script" {
					t.Errorf("escaped text became a tag: %s", styled)
				}
				for _, a := range n.Attr {
					if strings.HasPrefix(a.Key, "on") {
						t.Errorf("attribute breakout: %s", styled)
					}
					if a.Key == "href" && (strings.Contains(a.Val, "nohost") || !strings.HasPrefix(a.Val, "https://")) {
						t.Errorf("an unsafe link survived: %s", styled)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		for _, n := range nodes {
			walk(n)
		}
	}
}

func TestBuildReCleansAndRefusesBadHeaders(t *testing.T) {
	a := Account{Name: "x", Address: "me@example.com"}
	b, err := a.Build(Outgoing{To: []Addr{{Email: "nancy@example.org"}}, Subject: "Re: hi​\r\nBcc: evil@evil.test =?utf-8?q?=E2=80=AE?=",
		HTML: "<script>alert(1)</script><title>t</title><p style=\"color:#fff\">hi​</p>"}, Signature{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b.Raw)
	for _, bad := range []string{"script", "alert", "<title>", "color:#fff", "​", "=?utf-8?q?=E2=80=AE?="} {
		if strings.Contains(raw, bad) {
			t.Errorf("%q reached the message", bad)
		}
	}
	msg, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || msg.Header.Get("Bcc") != "" || strings.ContainsAny(subj, "\r\n\u200b\u202e") || !strings.HasPrefix(subj, "Re: hi Bcc: evil@evil.test") {
		t.Fatalf("subject: %q (%v), headers %q", subj, err, msg.Header)
	}
	for _, bad := range []Addr{{Email: "nancy@example.org\nX-Evil: 1"}, {Email: "evil@evil.test> <nancy@example.org"},
		{Email: "nancy@example.org, evil@evil.test"}, {Email: "evil"}, {Email: "Nancy <nancy@example.org>"}} {
		if _, err := a.Build(Outgoing{To: []Addr{bad}, Subject: "s", HTML: "<p>x</p>"}, Signature{}, time.Now()); err == nil {
			t.Errorf("address %q must be refused", bad.Email)
		}
		if _, err := a.Build(Outgoing{To: []Addr{{Email: "ok@example.org"}}, Cc: []Addr{bad}, Subject: "s", HTML: "<p>x</p>"}, Signature{}, time.Now()); err == nil {
			t.Errorf("cc %q must be refused", bad.Email)
		}
	}
	// Bad message ids are dropped, never written into a header.
	b, err = a.Build(Outgoing{To: []Addr{{Email: "n@example.org"}}, Subject: "s", HTML: "<p>x</p>",
		InReplyTo: "a@b>\r\nBcc: evil@evil.test\r\nX-Evil: <c", References: []string{"r@b>\r\nX-Evil: 1\r\n\r\n--injected <", "ok@x"}}, Signature{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b.Raw), "X-Evil") || strings.Contains(string(b.Raw), "evil.test") || strings.Contains(string(b.Raw), "In-Reply-To") {
		t.Fatalf("message ids leaked into headers:\n%.600s", b.Raw)
	}
	// A long thread keeps References short enough for the line limit.
	var refs []string
	for i := 0; i < 40; i++ {
		refs = append(refs, "CAJ"+strings.Repeat("x", 40)+"@mail.gmail.com")
	}
	b, _ = a.Build(Outgoing{To: []Addr{{Email: "n@example.org"}}, Subject: "s", HTML: "<p>x</p>", InReplyTo: "last@x", References: refs}, Signature{}, time.Now())
	for _, line := range strings.Split(string(b.Raw), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("header line over 998 octets: %.80s…", line)
		}
	}
}

func TestEnvelopeIncludesCcAndOnlyValidatedAddresses(t *testing.T) {
	f := mailtest.NewSMTP(t)
	a := testAccount(t, "127.0.0.1:1", f.Addr)
	b, err := a.Build(Outgoing{To: []Addr{{Email: "Nancy@Example.org"}}, Cc: []Addr{{Name: "Sam\r\nBcc: x@y", Email: "sam@example.org"}}, Subject: "s", HTML: "<p>x</p>"}, Signature{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Send(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	got := f.Messages()
	if len(got) != 1 || strings.Join(got[0].To, ",") != "nancy@example.org,sam@example.org" {
		t.Fatalf("envelope: %+v", got)
	}
	if strings.Contains(got[0].Data, "\r\nBcc:") {
		t.Fatal("a display name injected a header")
	}
}

func TestTLSIsVerifiedAndNeverSkipped(t *testing.T) {
	// A server offering STARTTLS with a certificate nobody vouches for.
	f := mailtest.NewSMTPStartTLS(t)
	_, port, _ := net.SplitHostPort(f.Addr)
	a := testAccount(t, "127.0.0.1:1", f.Addr)
	a.Security, a.SMTP = "tls", "127.0.0.1:"+port
	b, _ := a.Build(Outgoing{To: []Addr{{Email: "x@example.org"}}, Subject: "s", HTML: "<p>x</p>"}, Signature{}, time.Now())
	if err := a.Send(context.Background(), b); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("an unverifiable certificate must be refused: %v", err)
	}
	if len(f.Messages()) != 0 {
		t.Fatal("sent over an unverified connection")
	}
	if err := a.CheckSMTP(context.Background()); err == nil {
		t.Fatal("the check must refuse it too")
	}
	// No STARTTLS at all: the check refuses to log in in plain text.
	plain := mailtest.NewSMTP(t)
	_, port, _ = net.SplitHostPort(plain.Addr)
	a.SMTP = "127.0.0.1:" + port
	if err := a.CheckSMTP(context.Background()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("CheckSMTP without STARTTLS: %v", err)
	}
}

func TestIMAPRefusesPlainTextLoginForATLSAccount(t *testing.T) {
	addr := mailtest.IMAP(t) // offers no STARTTLS
	a := testAccount(t, addr, "127.0.0.1:1")
	a.Security = "tls"
	if _, err := a.Dial(context.Background()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("must refuse to log in in plain text: %v", err)
	}
}

func TestPlainIsOnlyForLoopbackOnBothSides(t *testing.T) {
	for _, a := range []Account{
		{Name: "x", Address: "me@example.com", IMAP: "127.0.0.1:1143", SMTP: "smtp.example.com:25", Security: "plain", PasswordFile: "/x"},
		{Name: "x", Address: "me@example.com", IMAP: "imap.example.com:143", SMTP: "127.0.0.1:1025", Security: "plain", PasswordFile: "/x"},
	} {
		if err := a.Check(); err == nil {
			t.Errorf("plain with %s / %s must be refused", a.IMAP, a.SMTP)
		}
	}
}

func TestPasswordFileMustNotBeGroupReadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(p, []byte("pw"), 0o640)
	os.Chmod(p, 0o640)
	if _, err := (Account{Name: "x", PasswordFile: p}).Password(); err == nil {
		t.Fatal("a group-readable password file must be refused")
	}
}

func TestSignatureImagesStayInTheSignatureFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sig")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(root, "secret.png"), []byte("SECRET-BYTES"), 0o644)
	for _, src := range []string{"../secret.png", filepath.Join(root, "secret.png"), "sub/../../secret.png"} {
		os.WriteFile(filepath.Join(dir, "sig.html"), []byte(`<p>Cole</p><img src="`+src+`">`), 0o644)
		a := Account{Name: "x", Address: "me@example.com", SignatureHTML: filepath.Join(dir, "sig.html")}
		sig, err := a.LoadSignature(false)
		for _, img := range sig.Images {
			if string(img.Data) == "SECRET-BYTES" {
				t.Errorf("%s: read a file outside the signature folder", src)
			}
		}
		if err == nil {
			t.Errorf("%s: expected an error (no such image inside the folder)", src)
		}
	}
}

func TestParseDropsMessageIDsThatCouldBreakAHeader(t *testing.T) {
	m, _ := Parse([]byte("From: x@example.org\r\nMessage-ID: not valid> Bcc: evil@evil.test\r\nSubject: s\r\n\r\nbody\r\n"))
	if m.MessageID != "" {
		t.Fatalf("kept %q", m.MessageID)
	}
}
