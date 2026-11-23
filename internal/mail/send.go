package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Outgoing is a reply (or new message) to send. HTML must come from Clean.
type Outgoing struct {
	To         []Addr
	Cc         []Addr
	Subject    string
	HTML       string // a fragment from Clean
	InReplyTo  string // the Message-ID replied to, without brackets
	References []string
	Quote      *Quote
}

// Built is a message ready for SMTP or an IMAP append.
type Built struct {
	Raw        []byte
	MessageID  string // without brackets
	Recipients []string
	HTML       string // the HTML part as sent (for the preview)
	Text       string // the plain-text part as sent
}

// Signature is an account's signature: its HTML (images swapped for cid:
// references) and the inline images.
type Signature struct {
	HTML   string
	Text   string
	Images []InlineImage
}

// InlineImage is a signature image sent as a related part.
type InlineImage struct {
	CID  string
	Type string
	Name string
	Data []byte
}

var srcAttr = regexp.MustCompile(`(?i)(\bsrc\s*=\s*)"([^"]+)"`)

// LoadSignature reads the account's signature files. Images the HTML
// references by a relative path are read from beside it and sent inline.
// With preview, they become data: URIs instead, for showing in a browser.
func (a Account) LoadSignature(preview bool) (Signature, error) {
	var s Signature
	if a.SignatureText != "" {
		b, err := os.ReadFile(a.SignatureText)
		if err != nil {
			return s, fmt.Errorf("%s: signature_text: %w", a.Name, err)
		}
		s.Text = strings.TrimSpace(string(b))
	}
	if a.SignatureHTML == "" {
		return s, nil
	}
	b, err := os.ReadFile(a.SignatureHTML)
	if err != nil {
		return s, fmt.Errorf("%s: signature_html: %w", a.Name, err)
	}
	html := regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(string(b), "")
	dir := filepath.Dir(a.SignatureHTML)
	domain := domainOf(a.Address)
	var ferr error
	html = srcAttr.ReplaceAllStringFunc(html, func(m string) string {
		sub := srcAttr.FindStringSubmatch(m)
		src := sub[2]
		if regexp.MustCompile(`(?i)^(cid:|https?:|data:)`).MatchString(src) {
			return m
		}
		p := filepath.Join(dir, filepath.Clean("/" + src)[1:])
		data, err := os.ReadFile(p)
		if err != nil {
			ferr = fmt.Errorf("%s: signature image %s: %w", a.Name, src, err)
			return m
		}
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if preview {
			return sub[1] + `"data:` + ct + ";base64," + base64.StdEncoding.EncodeToString(data) + `"`
		}
		cid := "sig-" + randHex(8) + "@" + domain
		s.Images = append(s.Images, InlineImage{CID: cid, Type: ct, Name: filepath.Base(p), Data: data})
		return sub[1] + `"cid:` + cid + `"`
	})
	if ferr != nil {
		return Signature{}, ferr
	}
	s.HTML = strings.TrimSpace(html)
	return s, nil
}

// Build makes the MIME message: multipart/alternative with a plain-text
// part and an HTML part (multipart/related when the signature has images).
// Both parts come from the same cleaned HTML.
func (a Account) Build(o Outgoing, sig Signature, now time.Time) (Built, error) {
	if len(o.To) == 0 {
		return Built{}, errors.New("no recipients")
	}
	clean, _ := Clean(o.HTML) // already clean; cleaning again is a no-op safety net
	subject, _ := StripInvisible(strings.TrimSpace(o.Subject))
	subject = strings.Join(strings.Fields(subject), " ")
	if subject == "" {
		return Built{}, errors.New("no subject")
	}
	htmlDoc := Document(clean, sig.HTML, sig.Text, o.Quote)
	text := PlainDocument(clean, sig.Text, o.Quote)
	id := randHex(16) + "@" + domainOf(a.Address)

	var buf bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	h("From", a.From().String())
	h("To", joinAddrs(o.To))
	if len(o.Cc) > 0 {
		h("Cc", joinAddrs(o.Cc))
	}
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+id+">")
	if o.InReplyTo != "" {
		h("In-Reply-To", "<"+o.InReplyTo+">")
		refs := append([]string{}, o.References...)
		if len(refs) == 0 || refs[len(refs)-1] != o.InReplyTo {
			refs = append(refs, o.InReplyTo)
		}
		h("References", "<"+strings.Join(refs, "> <")+">")
	}
	h("MIME-Version", "1.0")
	alt := multipart.NewWriter(&buf)
	h("Content-Type", `multipart/alternative; boundary="`+alt.Boundary()+`"`)
	buf.WriteString("\r\n")

	tp, _ := alt.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	writeQP(tp, text)

	if len(sig.Images) == 0 {
		hp, _ := alt.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/html; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		writeQP(hp, htmlDoc)
	} else {
		var rel bytes.Buffer
		rw := multipart.NewWriter(&rel)
		rp, _ := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {`multipart/related; type="text/html"; boundary="` + rw.Boundary() + `"`}})
		hp, _ := rw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/html; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		writeQP(hp, htmlDoc)
		for _, img := range sig.Images {
			ip, _ := rw.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {img.Type},
				"Content-Transfer-Encoding": {"base64"},
				"Content-ID":                {"<" + img.CID + ">"},
				"Content-Disposition":       {mime.FormatMediaType("inline", map[string]string{"filename": img.Name})},
			})
			writeB64(ip, img.Data)
		}
		rw.Close()
		rp.Write(rel.Bytes())
	}
	alt.Close()

	var rcpt []string
	seen := map[string]bool{}
	for _, x := range append(append([]Addr{}, o.To...), o.Cc...) {
		e := strings.ToLower(x.Email)
		if e != "" && !seen[e] {
			seen[e] = true
			rcpt = append(rcpt, e)
		}
	}
	return Built{Raw: buf.Bytes(), MessageID: id, Recipients: rcpt, HTML: htmlDoc, Text: text}, nil
}

// Send delivers a built message over SMTP. With security "tls" it uses
// implicit TLS on port 465 and requires STARTTLS otherwise; "plain" is only
// accepted for a loopback gateway (Check enforces that).
func (a Account) Send(ctx context.Context, b Built) error {
	pw, err := a.Password()
	if err != nil {
		return err
	}
	host, port, _ := net.SplitHostPort(a.SMTP)
	deadline := time.Now().Add(2 * time.Minute)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	d := &net.Dialer{Timeout: 30 * time.Second, Deadline: deadline}
	tcfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if a.Security == "tls" && port == "465" {
		conn, err = tls.DialWithDialer(d, "tcp", a.SMTP, tcfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", a.SMTP)
	}
	if err != nil {
		return fmt.Errorf("%s: connect %s: %w", a.Name, a.SMTP, err)
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: smtp: %w", a.Name, err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("%s: smtp hello: %w", a.Name, err)
	}
	if a.Security == "tls" && port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s: %s doesn't offer STARTTLS; refusing to send in plain text", a.Name, a.SMTP)
		}
		if err := c.StartTLS(tcfg); err != nil {
			return fmt.Errorf("%s: starttls: %w", a.Name, err)
		}
	}
	if ok, _ := c.Extension("AUTH"); ok {
		if err := c.Auth(smtp.PlainAuth("", a.User, pw, host)); err != nil {
			return fmt.Errorf("%s: smtp login: %w", a.Name, err)
		}
	}
	if err := c.Mail(a.Address); err != nil {
		return fmt.Errorf("%s: smtp from: %w", a.Name, err)
	}
	for _, r := range b.Recipients {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("%s: smtp to %s: %w", a.Name, r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("%s: smtp data: %w", a.Name, err)
	}
	if _, err := w.Write(b.Raw); err != nil {
		return fmt.Errorf("%s: smtp write: %w", a.Name, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%s: smtp send: %w", a.Name, err)
	}
	c.Quit()
	return nil
}

// CheckSMTP connects and logs in without sending anything.
func (a Account) CheckSMTP(ctx context.Context) error {
	pw, err := a.Password()
	if err != nil {
		return err
	}
	host, port, _ := net.SplitHostPort(a.SMTP)
	d := &net.Dialer{Timeout: 30 * time.Second, Deadline: time.Now().Add(time.Minute)}
	tcfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if a.Security == "tls" && port == "465" {
		conn, err = tls.DialWithDialer(d, "tcp", a.SMTP, tcfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", a.SMTP)
	}
	if err != nil {
		return fmt.Errorf("%s: connect %s: %w", a.Name, a.SMTP, err)
	}
	conn.SetDeadline(time.Now().Add(time.Minute))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	c.Hello("localhost")
	if a.Security == "tls" && port != "465" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s: no STARTTLS", a.Name)
		}
		if err := c.StartTLS(tcfg); err != nil {
			return err
		}
	}
	if ok, _ := c.Extension("AUTH"); ok {
		if err := c.Auth(smtp.PlainAuth("", a.User, pw, host)); err != nil {
			return fmt.Errorf("%s: smtp login: %w", a.Name, err)
		}
	}
	return c.Quit()
}

func joinAddrs(as []Addr) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
}

func writeQP(w interface{ Write([]byte) (int, error) }, s string) {
	q := quotedprintable.NewWriter(w)
	q.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")))
	q.Close()
}

func writeB64(w interface{ Write([]byte) (int, error) }, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		w.Write([]byte(enc[:76] + "\r\n"))
		enc = enc[76:]
	}
	w.Write([]byte(enc + "\r\n"))
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return "docket.local"
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ReplySubject adds "Re: " unless the subject already has it.
func ReplySubject(s string) string {
	t := strings.TrimSpace(s)
	if regexp.MustCompile(`(?i)^(re|aw|sv)\s*:`).MatchString(t) {
		return t
	}
	return "Re: " + t
}
