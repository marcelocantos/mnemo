// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"strings"
	"testing"
)

func TestStripPreviewSkip(t *testing.T) {
	in := "keep1\n<!-- preview-skip -->\ndrop me\n<!-- /preview-skip -->\nkeep2\n"
	got := StripPreviewSkip(in)
	if strings.Contains(got, "drop me") {
		t.Errorf("preview-skip region not stripped: %q", got)
	}
	if !strings.Contains(got, "keep1") || !strings.Contains(got, "keep2") {
		t.Errorf("non-skip content lost: %q", got)
	}
}

func TestStripPreviewSkipMultiple(t *testing.T) {
	in := "a<!-- preview-skip -->X<!-- /preview-skip -->b<!-- preview-skip -->Y<!-- /preview-skip -->c"
	got := StripPreviewSkip(in)
	if got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}

func TestStripPreviewSkipUnterminated(t *testing.T) {
	in := "keep<!-- preview-skip -->droppedforever"
	got := StripPreviewSkip(in)
	if got != "keep" {
		t.Errorf("got %q, want keep", got)
	}
}

func TestGFMFeatures(t *testing.T) {
	md := "~~struck~~ and a list:\n\n- [x] done\n- [ ] todo\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	html, err := HTML(md, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<del>", "<table>", "type=\"checkbox\"", "<td>1</td>"} {
		if !strings.Contains(html, want) {
			t.Errorf("GFM output missing %q in:\n%s", want, html)
		}
	}
}

func TestRawHTMLOmitted(t *testing.T) {
	// Tag-filtering: with Unsafe off, raw HTML must not pass through.
	md := "before\n\n<script>alert('xss')</script>\n\nafter\n"
	html, err := HTML(md, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<script>") {
		t.Errorf("raw <script> leaked into output:\n%s", html)
	}
}

func TestSyntheticH1(t *testing.T) {
	html, err := HTML("body text", Options{SyntheticH1: "project-alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "<h1>project-alpha</h1>") {
		t.Errorf("synthetic H1 missing:\n%s", html)
	}
}

func TestStandaloneWrapsWithTheme(t *testing.T) {
	html, err := HTML("# hi", Options{Standalone: true, Theme: ThemeLight})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(html, "<!DOCTYPE html>") {
		t.Errorf("standalone output not a full document: %.40q", html)
	}
	if !strings.Contains(html, palettes[ThemeLight].bg) {
		t.Errorf("light palette not applied")
	}
	// Fragment mode is the default.
	frag, _ := HTML("# hi", Options{})
	if strings.Contains(frag, "<!DOCTYPE") {
		t.Errorf("fragment mode should not wrap in a document")
	}
}
