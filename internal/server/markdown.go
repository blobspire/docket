package server

import (
	"bytes"
	"html/template"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Item bodies are markdown written by agents. goldmark's defaults keep that
// safe: raw HTML is dropped and javascript:/data: style links aren't rendered.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(safeLinks{}, 500))),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// mdNoTables is md without tables: a table pads every row out to its
// header's width, so a few kilobytes with thousands of columns would render
// to hundreds of megabytes.
var mdNoTables = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Linkify, extension.TaskList),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(safeLinks{}, 500))),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// maxTableColumns is the widest table rendered as one; maxTableCells caps a
// rough count of cells (widest row × lines) a text may make tables of; and
// only the first mdBudget bytes render as markdown, the rest as plain text.
const (
	maxTableColumns = 64
	maxTableCells   = 20000
	mdBudget        = 32 << 10
)

// safeLinks opens links in a new tab without handing over the opener, and
// turns images into plain links: an agent-written body must not make the
// dashboard fetch from any host it names on every load.
type safeLinks struct{}

func (safeLinks) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var images []*ast.Image
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch l := n.(type) {
		case *ast.Link:
			external(l)
		case *ast.AutoLink:
			external(l)
		case *ast.Image:
			images = append(images, l)
		}
		return ast.WalkContinue, nil
	})
	for _, img := range images {
		// Inside a link already: keep just its text, so the outer link works.
		inLink := false
		for p := img.Parent(); p != nil; p = p.Parent() {
			if _, ok := p.(*ast.Link); ok {
				inLink = true
			}
		}
		if inLink {
			parent := img.Parent()
			if img.FirstChild() == nil {
				img.AppendChild(img, ast.NewString([]byte("image")))
			}
			for c := img.FirstChild(); c != nil; {
				next := c.NextSibling()
				parent.InsertBefore(parent, img, c)
				c = next
			}
			parent.RemoveChild(parent, img)
			continue
		}
		link := ast.NewLink()
		link.Destination, link.Title = img.Destination, img.Title
		for c := img.FirstChild(); c != nil; {
			next := c.NextSibling()
			link.AppendChild(link, c)
			c = next
		}
		if link.FirstChild() == nil {
			link.AppendChild(link, ast.NewString([]byte("image")))
		}
		external(link)
		img.Parent().ReplaceChild(img.Parent(), img, link)
	}
}

func external(n ast.Node) {
	n.SetAttributeString("target", []byte("_blank"))
	n.SetAttributeString("rel", []byte("noopener noreferrer"))
}

var (
	delimRow = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
	quotes   = regexp.MustCompile(`^[ \t]*(>[ \t]?)*`) // blockquote markers before a table
)

// blank is a blank line as markdown sees it: ASCII whitespace only (a line
// of non-breaking spaces is text, and a table runs on through it).
func blank(line string) bool { return strings.Trim(line, " \t\v\f\r") == "" }

// tableCells estimates the cells the tables in s would make: each header's
// columns times its rows (a table runs to the next blank line), or
// effectively infinite for a header wider than maxTableColumns.
func tableCells(s string) int {
	lines := strings.Split(s, "\n")
	cells := 0
	for i := 0; i+1 < len(lines); i++ {
		next := quotes.ReplaceAllString(lines[i+1], "")
		if !strings.Contains(lines[i], "|") || !strings.Contains(next, "-") || !delimRow.MatchString(next) {
			continue
		}
		cols := strings.Count(lines[i], "|") + 1
		if cols > maxTableColumns+2 { // n columns: up to n+1 pipes
			return math.MaxInt32
		}
		j := i + 2
		for j < len(lines) && !blank(lines[j]) {
			j++
		}
		cells += cols * (j - i)
		i = j
	}
	return cells
}

// maxTableFloor bounds widest-line × line-count for the whole text: a
// backstop if some table shape slips past tableCells (it stays well above
// any ordinary report, so it doesn't cost one its tables).
const maxTableFloor = 300000

func tableFloor(s string) int {
	widest, lines := 0, 1
	for _, line := range strings.Split(s, "\n") {
		widest = max(widest, strings.Count(line, "|"))
		lines++
	}
	return (widest + 1) * lines
}

// renderMarkdown turns an item body into HTML for the page.
func renderMarkdown(s string) template.HTML {
	rest := ""
	if len(s) > mdBudget {
		cut := strings.LastIndexByte(s[:mdBudget], '\n')
		if cut < mdBudget/2 {
			cut = mdBudget
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		s, rest = s[:cut], s[cut:]
	}
	// A table pads every row to its header's width: a wide header or a long
	// run of rows under a wide one makes a node per cell.
	conv := md
	if tableCells(s) > maxTableCells || tableFloor(s) > maxTableFloor {
		conv = mdNoTables
	}
	var b bytes.Buffer
	out := template.HTML("")
	if err := conv.Convert([]byte(s), &b); err != nil || b.Len() > 40*len(s)+(1<<20) {
		out = template.HTML("<pre>" + template.HTMLEscapeString(s) + "</pre>")
	} else {
		out = template.HTML(b.String()) // goldmark escapes raw HTML (no WithUnsafe)
	}
	if rest != "" {
		out += template.HTML("<pre>" + template.HTMLEscapeString(rest) + "</pre>")
	}
	return out
}

// shortBody is how much of a body a dashboard row carries; the item's own
// page shows all of it.
const shortBody = 6000

// renderMarkdownShort renders the start of a long body, with a link to the
// rest, so a huge body doesn't weigh down every dashboard load.
func renderMarkdownShort(s, id string) template.HTML {
	if len(s) <= shortBody {
		return renderMarkdown(s)
	}
	cut := shortBody
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return renderMarkdown(s[:cut]) + template.HTML(`<p><a href="/item/`+template.HTMLEscapeString(id)+`">The rest of it →</a></p>`)
}
