package mail

import (
	"fmt"
	"html"
	"regexp"
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The inline styles docket adds to a cleaned fragment, so a reply looks
// right in Outlook desktop (Word engine), Outlook on the web and Gmail, none
// of which can be trusted with a <style> block. They match what Outlook
// writes (Aptos 12pt, black), as vtmail's do, so a docket reply looks like
// one typed by hand. Margins are top/bottom only: Outlook desktop clips
// bullets when a list's left margin is set.
const fontCSS = "font-family: Aptos, Arial, Helvetica, sans-serif; font-size: 12pt; color: #000000;"

var tagCSS = map[string]string{
	"p":     fontCSS + " margin: 0 0 16px 0;",
	"ul":    "margin-top: 0; margin-bottom: 12px;",
	"ol":    "margin-top: 0; margin-bottom: 12px;",
	"li":    fontCSS + " margin: 0 0 4px 0;",
	"table": "border-collapse: collapse; margin: 0 0 12px 0;",
	"th":    fontCSS + " border: 1px solid #c8ced4; background-color: #eef1f4; padding: 5px 10px; text-align: left; vertical-align: top; font-weight: bold;",
	"td":    fontCSS + " border: 1px solid #c8ced4; padding: 5px 10px; text-align: left; vertical-align: top;",
	"a":     "color: #0b57d0;",
}

// labelCSS is a paragraph that is only bold text: a section label, set
// tighter to the content it introduces.
const labelCSS = fontCSS + " margin: 16px 0 4px 0;"

func parseClean(clean string) []*xhtml.Node {
	nodes, _ := xhtml.ParseFragment(strings.NewReader(clean), &xhtml.Node{Type: xhtml.ElementNode, Data: "body", DataAtom: atom.Body})
	return nodes
}

// Style adds docket's inline styles to a fragment from Clean. Its input
// must come from Clean: it writes back only the tags Clean allows.
func Style(clean string) string {
	var b strings.Builder
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		switch n.Type {
		case xhtml.TextNode:
			b.WriteString(html.EscapeString(n.Data))
			return
		case xhtml.ElementNode:
		default:
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		name := n.DataAtom.String()
		if !allowed[n.DataAtom] {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		if n.DataAtom == atom.Br {
			b.WriteString("<br>")
			return
		}
		css := tagCSS[name]
		if n.DataAtom == atom.P && isLabel(n) {
			css = labelCSS
		}
		b.WriteString("<" + name)
		if n.DataAtom == atom.A {
			b.WriteString(` href="` + html.EscapeString(attr(n, "href")) + `"`)
		}
		if css != "" {
			b.WriteString(` style="` + css + `"`)
		}
		b.WriteString(">")
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		b.WriteString("</" + name + ">")
	}
	for _, n := range parseClean(clean) {
		walk(n)
	}
	return b.String()
}

func isLabel(p *xhtml.Node) bool {
	strong := 0
	for c := p.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == xhtml.TextNode && strings.TrimSpace(c.Data) == "":
		case c.Type == xhtml.ElementNode && c.DataAtom == atom.Strong:
			strong++
		default:
			return false
		}
	}
	return strong == 1
}

var spaces = regexp.MustCompile(`[ \t\r\n]+`)

// Text is the plain-text version of a fragment from Clean: paragraphs, real
// line breaks, "- " list items, "a | b" table rows, and links written as
// "text (address)" so nothing the HTML links to is hidden in the text part.
func Text(clean string) string {
	type list struct {
		ordered bool
		n       int
	}
	var lists []*list
	var walk func(b *strings.Builder, n *xhtml.Node)
	children := func(b *strings.Builder, n *xhtml.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(b, c)
		}
	}
	walk = func(b *strings.Builder, n *xhtml.Node) {
		if n.Type == xhtml.TextNode {
			b.WriteString(spaces.ReplaceAllString(n.Data, " "))
			return
		}
		if n.Type != xhtml.ElementNode {
			children(b, n)
			return
		}
		switch n.DataAtom {
		case atom.Br:
			b.WriteString("\n")
		case atom.P:
			children(b, n)
			b.WriteString("\n\n")
		case atom.Ul, atom.Ol:
			lists = append(lists, &list{ordered: n.DataAtom == atom.Ol})
			children(b, n)
			lists = lists[:len(lists)-1]
			b.WriteString("\n")
		case atom.Li:
			indent := ""
			if len(lists) > 1 {
				indent = strings.Repeat("  ", len(lists)-1)
			}
			mark := "- "
			if len(lists) > 0 && lists[len(lists)-1].ordered {
				l := lists[len(lists)-1]
				l.n++
				mark = fmt.Sprintf("%d. ", l.n)
			}
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteString("\n")
			}
			b.WriteString(indent + mark)
			children(b, n)
			b.WriteString("\n")
		case atom.Table:
			children(b, n)
			b.WriteString("\n")
		case atom.Tr:
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type != xhtml.ElementNode {
					continue
				}
				var cell strings.Builder
				children(&cell, c)
				cells = append(cells, strings.TrimSpace(spaces.ReplaceAllString(cell.String(), " ")))
			}
			b.WriteString(strings.Join(cells, " | ") + "\n")
		case atom.A:
			var t strings.Builder
			children(&t, n)
			text := t.String()
			b.WriteString(text)
			href := attr(n, "href")
			shown := strings.TrimPrefix(href, "mailto:")
			if tt := strings.TrimSpace(text); href != "" && tt != href && tt != shown {
				b.WriteString(" (" + shown + ")")
			}
		default:
			children(b, n)
		}
	}
	var b strings.Builder
	for _, n := range parseClean(clean) {
		walk(&b, n)
	}
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
		if strings.HasPrefix(l, "  ") && (strings.Contains(l, "- ") || strings.Contains(l, ". ")) {
			lines[i] = strings.TrimRight(l, " ") // keep a nested item's indent
		}
	}
	out := strings.Join(lines, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}

// Quote is the original message shown under a reply, Outlook style.
type Quote struct {
	From    string
	Sent    string
	To      string
	Cc      string
	Subject string
	Text    string
}

func (q *Quote) header() string {
	lines := []string{"________________________________", "From: " + q.From, "Sent: " + q.Sent, "To: " + q.To}
	if q.Cc != "" {
		lines = append(lines, "Cc: "+q.Cc)
	}
	lines = append(lines, "Subject: "+q.Subject)
	return strings.Join(lines, "\n")
}

func textToHTML(t string) string {
	lines := strings.Split(t, "\n")
	for i, l := range lines {
		lines[i] = html.EscapeString(l)
	}
	return strings.Join(lines, "<br>\n")
}

// Document is the HTML part of a reply: the styled fragment, the account's
// signature and the quoted original. Every non-ASCII character is written
// as a character reference, because Exchange re-encodes stored mail (it has
// saved drafts as big5) and references survive any charset.
func Document(clean, signatureHTML, signatureText string, q *Quote) string {
	parts := []string{Style(clean)}
	switch {
	case signatureHTML != "":
		parts = append(parts, signatureHTML) // the account's own markup, already styled
	case signatureText != "":
		parts = append(parts, `<p style="`+tagCSS["p"]+`">`+textToHTML(signatureText)+"</p>")
	}
	if q != nil {
		parts = append(parts,
			`<hr style="border: none; border-top: 1px solid #b5b5b5; margin: 16px 0 8px 0;">`,
			`<p style="`+tagCSS["p"]+`">`+textToHTML(q.header())+"</p>",
			`<p style="`+tagCSS["p"]+`">`+textToHTML(q.Text)+"</p>")
	}
	doc := `<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"></head>` + "\n" +
		`<body><div style="` + fontCSS + `">` + "\n" + strings.Join(parts, "\n") + "\n</div></body></html>\n"
	return asciiRefs(doc)
}

// PlainDocument is the text part: the reply, the signature and the quote.
func PlainDocument(clean, signatureText string, q *Quote) string {
	t := Text(clean)
	if signatureText != "" {
		t += "\n\n" + strings.TrimSpace(signatureText)
	}
	if q != nil {
		t += "\n\n" + q.header() + "\n\n" + q.Text
	}
	return t + "\n"
}

func asciiRefs(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "&#%d;", r)
		}
	}
	return b.String()
}
