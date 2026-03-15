package web

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM, // GitHub-Flavoured Markdown (tables, strikethrough, etc.)
	),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
	),
	goldmark.WithRendererOptions(
		// Do NOT pass html.WithUnsafe() so raw HTML is stripped.
		html.WithHardWraps(),
	),
)

// renderMarkdown converts markdown text to an HTML-safe template.HTML.
// Raw HTML in the source is sanitised away by goldmark's default renderer.
func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		// Fallback: return escaped plain text.
		return template.HTML(template.HTMLEscapeString(src))
	}
	// goldmark's output is already safe (no raw HTML) so we can trust it.
	return template.HTML(buf.String())
}
