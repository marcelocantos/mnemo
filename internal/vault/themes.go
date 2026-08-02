// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// themePath returns the library-wing path for one theme page. The slug
// is derived from the label; rendered themes clear the weight floor and
// are multi-member, so label collisions are unlikely. A theme whose
// label degrades to an empty slug falls back to its stable id.
func themePath(v store.ThemeView) string {
	slug := v.Slug
	if slug == "" || slug == "untitled" {
		slug = slugify(v.Label)
	}
	if slug == "" || slug == "untitled" {
		slug = slugify(strings.TrimPrefix(v.ID, "theme_"))
	}
	return path.Join(mnemoWingDir, "themes", slug+".md")
}

// themeArchivePath returns the retired-theme path under themes/_archive/
// (docs/design/vault-clustering.md § retirement). A theme whose newest
// member is older than retire_after fades here unless pinned.
func themeArchivePath(v store.ThemeView) string {
	base := path.Base(themePath(v))
	return path.Join(mnemoWingDir, "themes", "_archive", base)
}

// themesIndexPath returns the library-wing themes collection index.
func themesIndexPath() string {
	return path.Join(mnemoWingDir, "themes", "_index.md")
}

// themeRetired reports whether a theme should be archived: its newest
// member (last_touched) is older than retireAfter. A pinned theme is
// exempt (checked by the caller). retireAfter <= 0 disables retirement;
// an empty or unparseable timestamp is treated as not retired, so a
// theme is never archived on a parse failure alone.
func themeRetired(lastTouched string, retireAfter time.Duration, now time.Time) bool {
	if retireAfter <= 0 || lastTouched == "" {
		return false
	}
	t := parseThemeTS(lastTouched)
	if t.IsZero() {
		return false
	}
	return now.Sub(t) > retireAfter
}

func parseThemeTS(ts string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// renderTheme produces one _mnemo/themes/<slug>.md page. label-source is
// "bigram" for the heuristic default; the user-anchored and LLM label
// paths (and per-member evidence enrichment) land in later phases.
func renderTheme(v store.ThemeView) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "type", "theme")
	writeYAML(&b, "theme_id", v.ID)
	writeYAML(&b, "aliases", `["`+v.Label+`"]`)
	fmt.Fprintf(&b, "weight: %.1f\n", v.Weight)
	fmt.Fprintf(&b, "member_count: %d\n", v.MemberCount)
	writeYAML(&b, "first-seen", dateOf(v.FirstSeen))
	writeYAML(&b, "last-touched", dateOf(v.LastTouched))
	writeYAML(&b, "computed_at", v.ComputedAt)
	labelSource := v.LabelSource
	if labelSource == "" {
		labelSource = "bigram"
	}
	writeYAML(&b, "label-source", labelSource)
	b.WriteString("tags:\n")
	b.WriteString("  - mnemo\n")
	b.WriteString("  - mnemo/theme\n")
	b.WriteString("  - theme\n")
	for _, repo := range v.Repos {
		if r := shortProjectName(repo); r != "" && r != "untitled" {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# %s\n\n", v.Label)
	fmt.Fprintf(&b, "*weight %.1f · %s", v.Weight, pluralize(v.MemberCount, "member"))
	if len(v.Repos) > 0 {
		fmt.Fprintf(&b, " · %s", strings.Join(v.Repos, ", "))
	}
	b.WriteString("*\n\n")

	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString("## Summary\n\n")
		b.WriteString(summarize(s, 600))
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "## %s\n\n", themeProvenanceHeading(v.Members))
	for _, m := range v.Members {
		fmt.Fprintf(&b, "- %s · `%s`\n", m.Kind, m.EntityID)
	}
	b.WriteString("\n")

	return b.String()
}

// themeProvenanceHeading names the members section per the parent
// design's per-entity heading table: exclusively decisions/compactions
// reads as "Underlying decisions"; any other mix is "Underlying
// entities".
func themeProvenanceHeading(members []store.ThemeMemberView) string {
	for _, m := range members {
		if m.Kind != "decision" && m.Kind != "compaction" {
			return "Underlying entities"
		}
	}
	return "Underlying decisions"
}

// renderThemesIndex produces _mnemo/themes/_index.md — the collection
// hub, themes listed heaviest first.
func renderThemesIndex(views []store.ThemeView) string {
	var b strings.Builder

	b.WriteString("---\n")
	writeYAML(&b, "type", "theme-index")
	fmt.Fprintf(&b, "themes: %d\n", len(views))
	b.WriteString("tags:\n")
	b.WriteString("  - mnemo\n")
	b.WriteString("  - mnemo/theme\n")
	b.WriteString("  - index\n")
	b.WriteString("---\n\n")

	b.WriteString("# Themes\n\n")
	b.WriteString("Topically-grouped work, clustered from decisions, compaction\n")
	b.WriteString("summaries, patterns, and your own vault notes. A theme earns a\n")
	fmt.Fprintf(&b, "page at weight %.0f or more.\n\n", store.DefaultMinClusterWeight)

	if len(views) == 0 {
		b.WriteString("*No theme currently clears that bar.*\n\n")
		return b.String()
	}

	for _, v := range views {
		link := strings.TrimSuffix(themePath(v), ".md")
		fmt.Fprintf(&b, "- [[%s|%s]] — weight %.1f · %s\n",
			link, v.Label, v.Weight, pluralize(v.MemberCount, "member"))
	}
	b.WriteString("\n")

	return b.String()
}
