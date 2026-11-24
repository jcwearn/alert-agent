package ui

import (
	"bytes"
	"context"
	"io"

	"github.com/a-h/templ"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
)

// markdown renders the agent's Markdown (reports, transcript text) as HTML.
//
// The agent's text can quote alert annotations and log lines, which are
// untrusted, so the renderer keeps goldmark's safe defaults: raw HTML is
// dropped and javascript:/data:/vbscript: links are emptied. Never add
// html.WithUnsafe() here.
func markdown(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		src := []byte(s)
		doc := parser.New(parser.WithExtensions(extension.NewGFMParser())).Parse(src)
		var buf bytes.Buffer
		if err := html.New(html.WithExtensions(extension.NewGFMHTMLRenderer())).Render(&buf, src, doc); err != nil {
			return err
		}
		_, err := w.Write(buf.Bytes())
		return err
	})
}
