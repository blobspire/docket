package server

import (
	"bytes"
	"html/template"
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

// renderMarkdown turns an item body into HTML for the page.
func renderMarkdown(s string) template.HTML {
	var b bytes.Buffer
	if err := md.Convert([]byte(s), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(b.String()) // goldmark escapes raw HTML (no WithUnsafe)
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
