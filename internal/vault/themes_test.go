// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

func sampleThemeView() store.ThemeView {
	return store.ThemeView{
		ID:          "theme_abc123",
		Label:       "Schema Migration",
		Slug:        "schema-migration",
		Summary:     "additive sqlite column changes keep old binaries safe",
		Weight:      4.2,
		Repos:       []string{"mnemo", "bullseye"},
		MemberCount: 3,
		FirstSeen:   "2026-07-01",
		LastTouched: "2026-07-10",
		ComputedAt:  "2026-07-10T00:00:00Z",
		Members: []store.ThemeMemberView{
			{Kind: "decision", EntityID: "2", Similarity: 0.95},
			{Kind: "compaction", EntityID: "3", Similarity: 0.80},
			{Kind: "pattern", EntityID: "pattern_x", Similarity: 0.55},
		},
	}
}

func TestThemePath(t *testing.T) {
	v := sampleThemeView()
	if got := themePath(v); got != "_mnemo/themes/schema-migration.md" {
		t.Errorf("themePath = %q", got)
	}
	// Empty slug falls back to the id.
	v.Slug, v.Label = "", ""
	if got := themePath(v); got != "_mnemo/themes/abc123.md" {
		t.Errorf("themePath fallback = %q", got)
	}
}

func TestRenderTheme(t *testing.T) {
	out := renderTheme(sampleThemeView())

	for _, want := range []string{
		"type: theme",
		"theme_id: theme_abc123",
		"label-source: bigram",
		"# Schema Migration",
		"## Summary",
		"additive sqlite column",
		// mixed kinds (pattern present) → "Underlying entities"
		"## Underlying entities",
		"decision · `2`",
		"pattern · `pattern_x`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered theme missing %q:\n%s", want, out)
		}
	}
}

func TestThemeProvenanceHeading(t *testing.T) {
	decisionsOnly := []store.ThemeMemberView{{Kind: "decision"}, {Kind: "compaction"}}
	if got := themeProvenanceHeading(decisionsOnly); got != "Underlying decisions" {
		t.Errorf("decisions/compactions heading = %q", got)
	}
	mixed := []store.ThemeMemberView{{Kind: "decision"}, {Kind: "pattern"}}
	if got := themeProvenanceHeading(mixed); got != "Underlying entities" {
		t.Errorf("mixed heading = %q", got)
	}
}

func TestRenderThemesIndex(t *testing.T) {
	views := []store.ThemeView{sampleThemeView()}
	out := renderThemesIndex(views)
	if !strings.Contains(out, "type: theme-index") || !strings.Contains(out, "themes: 1") {
		t.Errorf("index frontmatter wrong:\n%s", out)
	}
	if !strings.Contains(out, "[[_mnemo/themes/schema-migration|Schema Migration]]") {
		t.Errorf("index link missing:\n%s", out)
	}

	empty := renderThemesIndex(nil)
	if !strings.Contains(empty, "No theme currently clears that bar") {
		t.Errorf("empty index missing sentinel:\n%s", empty)
	}
}
