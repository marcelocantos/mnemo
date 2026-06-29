// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package render converts CLAUDE.md markdown to HTML for the Threads
// feature's preview paths (🎯T85.3, Integration §0.8). One renderer feeds
// both the CLI (`thread show`) and the menu-bar shim. It uses goldmark with
// the GFM extension (tables, strikethrough, autolinks, task lists); raw HTML
// is omitted (goldmark's Unsafe option stays off), which gives the proposal's
// "tag-filtering" requirement for free.
//
// The GUI path needs a self-contained document with inline CSS because the
// HTML lands in NSAttributedString(html:), not a web view that could load an
// external stylesheet. The palette values mirror the dashboard's light/dark
// tokens (ui/dashboard.html) for visual parity.
package render

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Theme selects the inline-CSS palette for standalone output.
type Theme string

const (
	ThemeLight Theme = "light"
	ThemeDark  Theme = "dark"
)

// Options parameterises a render.
type Options struct {
	// Theme picks the palette for standalone output. Empty → dark.
	Theme Theme
	// SyntheticH1, when non-empty, is prepended as a level-1 heading (the
	// thread's directory name) at render time — not written to the file.
	SyntheticH1 string
	// Standalone wraps the rendered fragment in a full HTML document with an
	// inline <style> block (for the GUI). When false, only the body fragment
	// is returned (for embedding, e.g. in the web dashboard).
	Standalone bool
}

var gfm = goldmark.New(goldmark.WithExtensions(extension.GFM))

// HTML renders markdown to HTML per opts. Preview-skip regions are stripped
// first; then the optional synthetic H1 is prepended; then goldmark converts.
func HTML(markdown string, opts Options) (string, error) {
	src := StripPreviewSkip(markdown)
	if opts.SyntheticH1 != "" {
		src = "# " + opts.SyntheticH1 + "\n\n" + src
	}
	var buf bytes.Buffer
	if err := gfm.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	if !opts.Standalone {
		return buf.String(), nil
	}
	return wrap(buf.String(), opts.Theme), nil
}

// previewSkipOpen and previewSkipClose bound regions excluded from the
// preview (used to keep boilerplate in the file for Claude's context while
// omitting it from the hover preview).
const (
	previewSkipOpen  = "<!-- preview-skip -->"
	previewSkipClose = "<!-- /preview-skip -->"
)

// StripPreviewSkip removes every region delimited by the preview-skip
// markers, inclusive. An unterminated open marker strips to end of input.
func StripPreviewSkip(s string) string {
	var b strings.Builder
	for {
		before, after, found := strings.Cut(s, previewSkipOpen)
		b.WriteString(before)
		if !found {
			break
		}
		_, rest, closed := strings.Cut(after, previewSkipClose)
		if !closed {
			// Unterminated open marker: drop the remainder.
			break
		}
		s = rest
	}
	return strings.TrimLeft(b.String(), "\n")
}

// palette holds the handful of colours the preview CSS needs.
type palette struct {
	bg, panel, border, text, dim, accent, code string
}

var palettes = map[Theme]palette{
	ThemeDark: {
		bg: "#0f1117", panel: "#1a1d27", border: "#2a2f45",
		text: "#c9d1e0", dim: "#8b93a7", accent: "#4f8ef7", code: "#1a1d27",
	},
	ThemeLight: {
		bg: "#ffffff", panel: "#f0f2f8", border: "#d0d4e0",
		text: "#333a50", dim: "#6b7280", accent: "#2563eb", code: "#f0f2f8",
	},
}

// wrap embeds the fragment in a self-contained HTML document with an inline
// stylesheet for the given theme.
func wrap(fragment string, theme Theme) string {
	p, ok := palettes[theme]
	if !ok {
		p = palettes[ThemeDark]
	}
	css := fmt.Sprintf(`
body{font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;color:%[4]s;background:%[1]s;margin:0;padding:14px}
h1,h2,h3,h4{color:%[4]s;line-height:1.25;margin:1.1em 0 .5em}
h1{font-size:1.5em;border-bottom:1px solid %[3]s;padding-bottom:.2em}
h2{font-size:1.25em;border-bottom:1px solid %[3]s;padding-bottom:.2em}
a{color:%[6]s;text-decoration:none}
a:hover{text-decoration:underline}
code{background:%[7]s;border:1px solid %[3]s;border-radius:3px;padding:1px 4px;font:12px/1.4 "SF Mono",Menlo,monospace}
pre{background:%[2]s;border:1px solid %[3]s;border-radius:6px;padding:10px;overflow-x:auto}
pre code{background:none;border:0;padding:0}
blockquote{margin:0;padding:0 1em;border-left:3px solid %[3]s;color:%[5]s}
table{border-collapse:collapse;width:100%%}
th,td{border:1px solid %[3]s;padding:5px 9px;text-align:left}
th{background:%[2]s}
del{color:%[5]s}
hr{border:0;border-top:1px solid %[3]s;margin:1.2em 0}
ul,ol{padding-left:1.4em}
`, p.bg, p.panel, p.border, p.text, p.dim, p.accent, p.code)

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>`)
	b.WriteString(css)
	b.WriteString(`</style></head><body>`)
	b.WriteString(fragment)
	b.WriteString(`</body></html>`)
	return b.String()
}
