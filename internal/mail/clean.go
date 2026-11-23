package mail

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Clean reduces an agent-written HTML fragment to the formatting the
// draft-email skill allows: p, br, strong, ul/ol/li, table/tr/th/td and links.
// Every other tag is dropped (with its content when it can hide or load
// something: script, style, images, embeds) or unwrapped to its text. No
// attribute survives except a link's http, https or mailto address, so a
// draft can't carry white, tiny or hidden text, and invisible Unicode
// (zero-width, direction overrides, tag characters) is removed. docket adds
// all styling itself (Style), and the plain-text part is made from this same
// HTML (Text), so what the human previews is what gets sent.
//
// The notes say what was removed, so the human can see an agent tried.
func Clean(fragment string) (string, []string) {
	c := &cleaner{seen: map[string]bool{}}
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	if err != nil {
		// The HTML5 parser accepts any input; this is only an I/O failure.
		return html.EscapeString(stripText(fragment)), []string{"the draft couldn't be read as HTML, so it is shown as plain text"}
	}
	for _, n := range nodes {
		c.node(n, 0)
	}
	if c.hidden > 0 {
		c.note(fmt.Sprintf("removed %d invisible character%s", c.hidden, plural(c.hidden)))
	}
	out := c.b.String()
	// Text pushed far down with blank lines is text the reader won't see.
	if squeezed := emptyParas.ReplaceAllString(manyBreaks.ReplaceAllString(out, "<br><br>"), ""); squeezed != out {
		if strings.Count(out, "<br>")-strings.Count(squeezed, "<br>") > 2 || len(emptyParas.FindAllString(out, -1)) > 2 {
			c.note("removed runs of blank lines")
		}
		out = squeezed
	}
	return strings.TrimSpace(out), c.notes
}

var (
	manyBreaks = regexp.MustCompile(`(?:<br>\s*){3,}`)
	emptyParas = regexp.MustCompile(`<p>(?:\s|<br>)*</p>`)
)

// allowed tags, written back without attributes (links keep href).
var allowed = map[atom.Atom]bool{
	atom.P: true, atom.Br: true, atom.Strong: true, atom.Ul: true, atom.Ol: true, atom.Li: true,
	atom.Table: true, atom.Tr: true, atom.Th: true, atom.Td: true, atom.A: true,
}

// dropped tags lose their content too: they run code, load something, or
// hold text that isn't meant to be read as the message.
var dropped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Title: true, atom.Template: true,
	atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Svg: true, atom.Math: true,
	atom.Noscript: true, atom.Textarea: true, atom.Select: true, atom.Option: true, atom.Button: true,
	atom.Input: true, atom.Img: true, atom.Video: true, atom.Audio: true, atom.Canvas: true,
	atom.Form: true, atom.Link: true, atom.Meta: true, atom.Base: true, atom.Frame: true,
	atom.Frameset: true, atom.Applet: true, atom.Picture: true, atom.Source: true, atom.Track: true,
	atom.Map: true, atom.Area: true, atom.Dialog: true, atom.Details: true,
}

// blocks that are unwrapped get a line break after them, so their text
// doesn't run into the next block's.
var blocks = map[atom.Atom]bool{
	atom.Div: true, atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true,
	atom.Main: true, atom.Blockquote: true, atom.Pre: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Address: true,
	atom.Figure: true, atom.Figcaption: true, atom.Nav: true, atom.Aside: true, atom.Center: true,
}

type cleaner struct {
	b      strings.Builder
	notes  []string
	seen   map[string]bool
	hidden int
}

func (c *cleaner) note(s string) {
	if !c.seen[s] {
		c.seen[s] = true
		c.notes = append(c.notes, s)
	}
}

func (c *cleaner) node(n *html.Node, depth int) {
	if depth > 200 {
		return
	}
	switch n.Type {
	case html.TextNode:
		t, k := StripInvisible(n.Data)
		c.hidden += k
		c.b.WriteString(html.EscapeString(t))
		return
	case html.CommentNode:
		c.note("removed an HTML comment")
		return
	case html.DoctypeNode:
		return
	case html.DocumentNode:
		c.children(n, depth)
		return
	case html.ElementNode:
	default:
		return
	}
	a := n.DataAtom
	switch {
	case dropped[a]:
		c.note("removed " + describeTag(n))
		return
	case a == atom.B:
		a = atom.Strong
	case allowed[a]:
	default:
		if hasStyling(n) {
			c.note("removed styling (colors, sizes, fonts)")
		}
		c.children(n, depth)
		if blocks[a] && !c.endsBlock() {
			c.b.WriteString("<br>")
		}
		return
	}
	if a == atom.A {
		href, ok := safeHref(attr(n, "href"))
		if !ok {
			if attr(n, "href") != "" {
				c.note("removed a link that wasn't a web or email address")
			}
			c.children(n, depth)
			return
		}
		if m := linkMismatch(textOf(n), href); m != "" {
			c.note(m)
		}
		c.b.WriteString(`<a href="` + html.EscapeString(href) + `">`)
		c.children(n, depth)
		c.b.WriteString("</a>")
		return
	}
	for _, at := range n.Attr {
		if at.Key != "" {
			c.note("removed styling (colors, sizes, fonts)")
			break
		}
	}
	name := a.String()
	if a == atom.Br {
		c.b.WriteString("<br>")
		return
	}
	c.b.WriteString("<" + name + ">")
	c.children(n, depth)
	c.b.WriteString("</" + name + ">")
}

func (c *cleaner) children(n *html.Node, depth int) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.node(ch, depth+1)
	}
}

func (c *cleaner) endsBlock() bool {
	s := strings.TrimRight(c.b.String(), " \n\t")
	if s == "" {
		return true
	}
	for _, t := range []string{"<br>", "</p>", "</ul>", "</ol>", "</li>", "</table>", "</tr>", "</td>", "</th>"} {
		if strings.HasSuffix(s, t) {
			return true
		}
	}
	return false
}

func describeTag(n *html.Node) string {
	switch n.DataAtom {
	case atom.Img, atom.Picture, atom.Svg, atom.Canvas:
		return "an image"
	case atom.Script, atom.Noscript, atom.Template:
		return "a script"
	case atom.Style:
		return "a style block"
	case atom.Iframe, atom.Object, atom.Embed, atom.Frame, atom.Frameset, atom.Applet:
		return "an embedded page"
	case atom.Form, atom.Input, atom.Button, atom.Select, atom.Option, atom.Textarea:
		return "a form"
	case atom.Video, atom.Audio, atom.Source, atom.Track:
		return "a video or sound"
	default:
		return "a <" + n.Data + "> block"
	}
}

func hasStyling(n *html.Node) bool {
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "style", "class", "color", "face", "size", "bgcolor", "hidden", "width", "height":
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	t, _ := StripInvisible(b.String())
	return strings.TrimSpace(t)
}

// safeHref keeps http, https and mailto addresses only.
func safeHref(h string) (string, bool) {
	h, _ = StripInvisible(strings.TrimSpace(h))
	if h == "" {
		return "", false
	}
	u, err := url.Parse(h)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return "", false
		}
	case "mailto":
	default:
		return "", false
	}
	return u.String(), true
}

// linkMismatch notes a link whose text looks like an address but goes
// somewhere else ("example.com" that opens evil.test).
func linkMismatch(text, href string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" || strings.ContainsAny(t, " \t\n") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "mailto") {
		if strings.Contains(t, "@") && !strings.EqualFold(strings.TrimPrefix(t, "mailto:"), u.Opaque) {
			return fmt.Sprintf("a link shows %s but writes to %s", text, u.Opaque)
		}
		return ""
	}
	if !strings.Contains(t, ".") {
		return ""
	}
	tt := t
	if !strings.Contains(tt, "://") {
		tt = "http://" + tt
	}
	tu, err := url.Parse(tt)
	if err != nil || tu.Host == "" {
		return ""
	}
	if host(tu.Host) != host(u.Host) {
		return fmt.Sprintf("a link shows %s but opens %s", text, u.Host)
	}
	return ""
}

func host(h string) string {
	h = strings.ToLower(h)
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimPrefix(h, "www.")
}

// StripInvisible removes characters that render as nothing but can carry
// text: format characters (zero-width spaces and joiners, direction
// overrides and isolates, soft hyphens, the Unicode tag block), variation
// selectors, control characters other than tab and newline, private-use
// characters and the Hangul fillers. It returns the cleaned text and how
// many characters it removed.
func StripInvisible(s string) (string, int) {
	n := 0
	out := strings.Map(func(r rune) rune {
		if invisible(r) {
			n++
			return -1
		}
		if r == ' ' || r == ' ' {
			return '\n'
		}
		return r
	}, s)
	return out, n
}

func invisible(r rune) bool {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return false
	case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r):
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF: // variation selectors
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tag characters
		return true
	case r == 0x115F, r == 0x1160, r == 0x3164, r == 0xFFA0, r == 0x2800: // fillers, blank braille
		return true
	case r == 0xFFFC: // object replacement
		return true
	}
	return false
}

func stripText(s string) string { t, _ := StripInvisible(s); return t }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
