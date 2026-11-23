package mail

import (
	"bytes"
	"io"
	"mime"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 mail
	gomail "github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

// Message is one email as docket keeps it.
type Message struct {
	Folder      string
	UID         uint32
	UIDValidity uint32
	MessageID   string   // without angle brackets
	InReplyTo   []string // without angle brackets
	References  []string
	From        Addr
	ReplyTo     []Addr
	To          []Addr
	Cc          []Addr
	Subject     string
	Date        time.Time
	Text        string // readable text: the text/plain part, else the HTML's text
	HTML        string // the HTML part as sent (shown only in a sandbox)
	Attachments []Attachment
	ListID      string
	Bulk        bool // mailing list, bulk or automatic mail
	Seen        bool
	Size        int
}

// Attachment is a file's name, type and size; docket doesn't keep the file.
type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size"`
}

const (
	maxText  = 100_000
	maxHTML  = 500_000
	maxParts = 64 // nested multiparts docket will read
)

// Parse reads a raw RFC 5322 message. It never fails outright on a broken
// message: whatever headers and text it could read are returned.
func Parse(raw []byte) (*Message, error) {
	m := &Message{Size: len(raw)}
	// Each level of nesting wraps every byte in another reader, so parsing
	// time grows with depth squared: a hostile message nested thousands deep
	// would take minutes. Read only the headers of one nested past maxParts.
	tooDeep := bytes.Count(bytes.ToLower(raw), []byte("multipart/")) > maxParts
	if tooDeep {
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			raw = append(stripCT(raw[:i+2]), "Content-Type: text/plain; charset=utf-8\r\n\r\n"...)
		} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
			raw = append(stripCT([]byte(strings.ReplaceAll(string(raw[:i+1]), "\n", "\r\n"))), "Content-Type: text/plain; charset=utf-8\r\n\r\n"...)
		}
	}
	r, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && r == nil {
		return m, err
	}
	h := r.Header
	if list, err := h.AddressList("From"); err == nil && len(list) > 0 {
		m.From = Addr{Name: list[0].Name, Email: strings.ToLower(list[0].Address)}
	} else if v := h.Get("From"); v != "" {
		if a := ParseAddrs(v); len(a) > 0 {
			m.From = a[0]
		}
	}
	m.ReplyTo = addrs(h, "Reply-To")
	m.To = addrs(h, "To")
	m.Cc = addrs(h, "Cc")
	if s, err := h.Subject(); err == nil {
		m.Subject = s
	} else {
		m.Subject = h.Get("Subject")
	}
	m.Subject, _ = StripInvisible(strings.TrimSpace(m.Subject))
	m.Date, _ = h.Date()
	if id, err := h.MessageID(); err == nil {
		m.MessageID = id
	} else {
		m.MessageID = strings.Trim(strings.TrimSpace(h.Get("Message-Id")), "<>")
	}
	m.InReplyTo, _ = h.MsgIDList("In-Reply-To")
	m.References, _ = h.MsgIDList("References")
	// An id that couldn't go back into a header safely is dropped.
	if !msgIDOK(m.MessageID) {
		m.MessageID = ""
	}
	m.InReplyTo, m.References = validIDs(m.InReplyTo), validIDs(m.References)
	m.ListID = strings.TrimSpace(h.Get("List-Id"))
	prec := strings.ToLower(h.Get("Precedence"))
	auto := strings.ToLower(h.Get("Auto-Submitted"))
	m.Bulk = m.ListID != "" || h.Get("List-Unsubscribe") != "" || prec == "bulk" || prec == "list" || prec == "junk" ||
		(auto != "" && auto != "no")

	var text, htmlBody strings.Builder
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if p == nil {
				break
			}
		}
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := ph.ContentType()
			b, _ := io.ReadAll(io.LimitReader(p.Body, maxHTML+1))
			switch ct {
			case "text/plain", "":
				if text.Len() < maxText {
					text.Write(b)
				}
			case "text/html":
				if htmlBody.Len() < maxHTML {
					htmlBody.Write(b)
				}
			default:
				if name := partName(ph.Header.Get("Content-Disposition"), ph.Header.Get("Content-Type")); name != "" {
					m.Attachments = append(m.Attachments, Attachment{Name: name, Type: ct, Size: len(b)})
				}
			}
		case *gomail.AttachmentHeader:
			name, _ := ph.Filename()
			ct, _, _ := ph.ContentType()
			n, _ := io.Copy(io.Discard, io.LimitReader(p.Body, 50<<20))
			if name == "" {
				name = "attachment"
			}
			m.Attachments = append(m.Attachments, Attachment{Name: name, Type: ct, Size: int(n)})
		}
	}
	m.HTML = clip(htmlBody.String(), maxHTML)
	m.Text = strings.TrimSpace(text.String())
	if tooDeep {
		m.Text = "(This message has an unusual structure, so docket shows only its headers; open it in your mail.)"
	}
	if m.Text == "" && m.HTML != "" {
		m.Text = HTMLText(m.HTML)
	}
	m.Text, _ = StripInvisible(clip(m.Text, maxText))
	return m, nil
}

func addrs(h gomail.Header, key string) []Addr {
	list, err := h.AddressList(key)
	if err != nil {
		return ParseAddrs(h.Get(key))
	}
	out := make([]Addr, 0, len(list))
	for _, a := range list {
		out = append(out, Addr{Name: a.Name, Email: strings.ToLower(a.Address)})
	}
	return out
}

func partName(disp, ctype string) string {
	if _, p, err := mime.ParseMediaType(disp); err == nil && p["filename"] != "" {
		return p["filename"]
	}
	if _, p, err := mime.ParseMediaType(ctype); err == nil && p["name"] != "" {
		return p["name"]
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8Start(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	if len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// HTMLText is the readable text of an incoming HTML email: visible text only
// (no scripts, styles or hidden head), blocks on their own lines.
func HTMLText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(spaces.ReplaceAllString(n.Data, " "))
			return
		case html.ElementNode:
			switch n.Data {
			case "script", "style", "head", "title", "noscript", "template", "svg":
				return
			case "br":
				b.WriteString("\n")
				return
			}
			if hiddenStyle(n) {
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "h5", "h6", "table", "blockquote", "section", "article", "header", "footer":
				b.WriteString("\n")
			case "td", "th":
				b.WriteString(" ")
			}
		}
	}
	walk(doc)
	lines := strings.Split(b.String(), "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// hiddenStyle reports text a mail client wouldn't show. Triage reads the
// text a person would see, so preheader tricks and hidden instructions in
// an incoming email are left out of what the model is given.
func hiddenStyle(n *html.Node) bool {
	for _, a := range n.Attr {
		k := strings.ToLower(a.Key)
		if k == "hidden" || (k == "aria-hidden" && a.Val == "true") {
			return true
		}
		if k == "style" && hiddenCSS(a.Val) {
			return true
		}
	}
	return false
}

// hiddenCSS reads a style attribute's declarations and reports text that
// renders invisibly: display:none, visibility:hidden, mso-hide:all, a zero
// opacity, a zero max-height, or a font smaller than 2px. Values are read
// as numbers, so 0.9em or opacity .8 stay visible.
func hiddenCSS(style string) bool {
	for _, decl := range strings.Split(style, ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(importantRE.ReplaceAllString(strings.ToLower(v), ""))
		switch k {
		case "display":
			if v == "none" {
				return true
			}
		case "visibility":
			if v == "hidden" || v == "collapse" {
				return true
			}
		case "mso-hide":
			if v == "all" {
				return true
			}
		case "opacity":
			if n, ok := cssNumber(v); ok && n <= 0.01 {
				return true
			}
		case "max-height", "height":
			if n, ok := cssNumber(v); ok && n == 0 && k == "max-height" {
				return true
			}
		case "font-size":
			if n, unit, ok := cssLength(v); ok {
				px := n
				switch unit {
				case "em", "rem":
					px = n * 16
				case "%":
					px = n * 16 / 100
				case "pt":
					px = n * 4 / 3
				}
				if px < 2 {
					return true
				}
			}
		}
	}
	return false
}

var importantRE = regexp.MustCompile(`!\s*important\s*$`)

func cssNumber(v string) (float64, bool) {
	n, _, ok := cssLength(v)
	return n, ok
}

func cssLength(v string) (float64, string, bool) {
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.' || v[i] == '-' || v[i] == '+') {
		i++
	}
	if i == 0 {
		return 0, "", false
	}
	n, err := strconv.ParseFloat(v[:i], 64)
	if err != nil {
		return 0, "", false
	}
	return n, strings.TrimSpace(v[i:]), true
}

// maxRefs is how many References docket keeps: the root and the latest,
// which is all threading needs. A message listing thousands (anyone can
// send one) would otherwise be costly or worse to store.
const maxRefs = 20

func validIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		if msgIDOK(id) {
			out = append(out, id)
		}
	}
	if len(out) > maxRefs {
		out = append(out[:1], out[len(out)-(maxRefs-1):]...)
	}
	return out
}
