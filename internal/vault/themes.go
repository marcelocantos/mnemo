// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// syncThemes materialises active and archived theme pages under the
// library wing (🎯T64.8). Active: _mnemo/themes/<slug>.md
// Archived: _mnemo/themes/_archive/<slug>.md
func (e *Exporter) syncThemes(ctx context.Context, layout string) error {
	if layout == store.VaultLayoutV1 {
		return nil
	}

	themes, err := e.backend.ListThemes(true)
	if err != nil {
		return fmt.Errorf("vault: list themes: %w", err)
	}

	written, skipped := 0, 0
	var active []store.ThemeSummary
	for _, th := range themes {
		if ctx.Err() != nil {
			break
		}
		if !th.Archived {
			active = append(active, th)
		}
		slug := th.Slug
		if slug == "" {
			slug = th.ID
		}
		relPath := themePath(slug, th.Archived)
		absPath := filepath.Join(e.path, relPath)
		// Opposite path: when a theme archives (or returns), remove the
		// stale location so there is never a live + archive pair for one id.
		other := themePath(slug, !th.Archived)
		otherAbs := filepath.Join(e.path, other)
		if _, err := os.Stat(otherAbs); err == nil {
			if rmErr := os.Remove(otherAbs); rmErr != nil {
				slog.Warn("vault: remove stale theme path failed", "path", otherAbs, "err", rmErr)
			} else {
				// Drop manifest row for the moved path if present.
				_ = e.backend.RemoveVaultManifestRow(other)
			}
		}
		if !needsUpdate(absPath, th.ComputedAt) {
			skipped++
			continue
		}
		content := renderTheme(th)
		if err := writeNote(absPath, content, th.ComputedAt); err != nil {
			slog.Warn("vault: write theme note failed", "path", absPath, "err", err)
			continue
		}
		e.recordOutput(relPath, "theme", th.ID, content)
		written++
	}

	idxPath := themesIndexPath()
	idxContent := renderThemesIndex(active)
	if err := writeNote(filepath.Join(e.path, idxPath), idxContent, ""); err != nil {
		slog.Warn("vault: write themes index failed", "path", idxPath, "err", err)
	} else {
		e.recordOutput(idxPath, "theme_index", "themes", idxContent)
	}

	slog.Info("vault: themes synced", "written", written, "skipped", skipped, "layout", layout)
	return nil
}

func themePath(slug string, archived bool) string {
	if archived {
		return filepath.ToSlash(filepath.Join("_mnemo", "themes", "_archive", slug+".md"))
	}
	return filepath.ToSlash(filepath.Join("_mnemo", "themes", slug+".md"))
}

func themesIndexPath() string {
	return filepath.ToSlash(filepath.Join("_mnemo", "themes", "index.md"))
}

func renderTheme(th store.ThemeSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "id: %q\n", th.ID)
	fmt.Fprintf(&b, "slug: %q\n", th.Slug)
	fmt.Fprintf(&b, "engine: %q\n", th.SourceEngine)
	fmt.Fprintf(&b, "weight: %g\n", th.Weight)
	fmt.Fprintf(&b, "member_count: %d\n", th.MemberCount)
	fmt.Fprintf(&b, "archived: %v\n", th.Archived)
	fmt.Fprintf(&b, "---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", th.Label)
	if th.CentroidText != "" {
		fmt.Fprintf(&b, "%s\n\n", th.CentroidText)
	}
	if len(th.Repos) > 0 {
		fmt.Fprintf(&b, "## Repos\n\n")
		for _, r := range th.Repos {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Theme id: `%s` · engine `%s` · weight %g · %d members\n",
		th.ID, th.SourceEngine, th.Weight, th.MemberCount)
	return b.String()
}

func renderThemesIndex(themes []store.ThemeSummary) string {
	var b strings.Builder
	b.WriteString("# Themes\n\n")
	b.WriteString("Document-level topical clusters from mnemo's vault clustering engine.\n\n")
	if len(themes) == 0 {
		b.WriteString("_No active themes yet. Trigger `mnemo_vault_recluster` after the index has decisions, compactions, or patterns._\n")
		return b.String()
	}
	for _, th := range themes {
		slug := th.Slug
		if slug == "" {
			slug = th.ID
		}
		fmt.Fprintf(&b, "- [[%s|%s]] (weight %.2f, %d members)\n", slug, th.Label, th.Weight, th.MemberCount)
	}
	return b.String()
}
