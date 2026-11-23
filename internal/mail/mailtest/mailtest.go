// Package mailtest runs throwaway IMAP and SMTP servers on loopback for
// docket's tests. The IMAP server is go-imap's in-memory backend (user
// "username", password "password"); the SMTP server records what it gets.
package mailtest

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
)

// IMAP starts an in-memory IMAP server and returns its host:port.
func IMAP(t testing.TB) string {
	t.Helper()
	s := server.New(memory.New())
	s.AllowInsecureAuth = true
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
	// The in-memory backend starts with a sample message in INBOX: remove
	// it, so a test's mailbox holds only what the test put there.
	c, err := client.Dial(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	all := new(imap.SeqSet)
	all.AddRange(1, 0)
	if err := c.Store(all, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge(nil); err != nil {
		t.Fatal(err)
	}
	return l.Addr().String()
}

// Append puts a raw message (LF line ends are fine) into a folder,
// creating the folder if needed.
func Append(t testing.TB, addr, folder, raw string, date time.Time) {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	c.Create(folder)
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n")
	if err := c.Append(folder, nil, date, bytes.NewBufferString(raw)); err != nil {
		t.Fatal(err)
	}
}

// SMTP is a fake SMTP server that offers AUTH PLAIN and records messages.
type SMTP struct {
	Addr string
	mu   sync.Mutex
	sent []Sent
	tls  *tls.Config // offers STARTTLS with this (a self-signed certificate)
}

// NewSMTPStartTLS is a fake SMTP server that offers STARTTLS with a
// self-signed certificate, which a client verifying certificates refuses.
func NewSMTPStartTLS(t testing.TB) *SMTP {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f := NewSMTP(t)
	f.mu.Lock()
	f.tls = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	f.mu.Unlock()
	return f
}

// Sent is one message the fake server accepted.
type Sent struct {
	From string
	To   []string
	Data string
	Auth string
}

// NewSMTP starts the fake server.
func NewSMTP(t testing.TB) *SMTP {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &SMTP{Addr: l.Addr().String()}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

// Messages returns what was sent so far.
func (f *SMTP) Messages() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent{}, f.sent...)
}

func (f *SMTP) serve(conn net.Conn) {
	defer func() { conn.Close() }()
	r := bufio.NewReader(conn)
	w := func(s string) { conn.Write([]byte(s + "\r\n")) }
	f.mu.Lock()
	tcfg := f.tls
	f.mu.Unlock()
	w("220 fake")
	var cur Sent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			w("250-fake")
			if tcfg != nil {
				w("250-STARTTLS")
			}
			w("250 AUTH PLAIN")
		case up == "STARTTLS" && tcfg != nil:
			w("220 go ahead")
			tc := tls.Server(conn, tcfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn = tc
			r = bufio.NewReader(conn)
		case strings.HasPrefix(up, "AUTH"):
			cur.Auth = cmd
			w("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur.From = strings.Trim(cmd[10:], "<> ")
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.To = append(cur.To, strings.Trim(cmd[8:], "<> "))
			w("250 ok")
		case up == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, "."))
			}
			cur.Data = b.String()
			f.mu.Lock()
			f.sent = append(f.sent, cur)
			f.mu.Unlock()
			cur = Sent{Auth: cur.Auth}
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}
