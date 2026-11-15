package server

import (
	"bytes"
	"html/template"

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
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(externalLinks{}, 500))),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// externalLinks opens links in a new tab, without handing over the opener.
type externalLinks struct{}

func (externalLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if l, ok := n.(*ast.Link); ok && entering {
			l.SetAttributeString("target", []byte("_blank"))
			l.SetAttributeString("rel", []byte("noopener noreferrer"))
		}
		return ast.WalkContinue, nil
	})
}

// renderMarkdown turns an item body into HTML for the page.
func renderMarkdown(s string) template.HTML {
	var b bytes.Buffer
	if err := md.Convert([]byte(s), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(b.String()) // goldmark escapes raw HTML (no WithUnsafe)
}
