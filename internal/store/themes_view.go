// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"time"
)

// Cluster-render defaults (docs/design/vault-clustering.md § config).
// Config wiring (vault_clustering.*) lands in a later phase; these are
// the design defaults used until then.
const (
	// DefaultMinClusterWeight is the weight floor for *rendering* a theme
	// as a page. Themes below it stay in the table but get no page.
	DefaultMinClusterWeight = 3.0
	// DefaultClusterInterval is the recompute cadence: clustering output
	// is noisy on short windows and the pass is non-trivial, so it runs
	// at most this often.
	DefaultClusterInterval = 24 * time.Hour
)

// ThemeMemberView is one member of a theme, for the renderer's evidence
// and provenance sections.
type ThemeMemberView struct {
	Kind       string  `json:"kind"`
	EntityID   string  `json:"entity_id"`
	Similarity float64 `json:"similarity"`
}

// ThemeView is a rendered theme: the themes row plus its members and a
// derived slug. Slug is computed from the label rather than stored (no
// slug column yet); rendered themes are few and multi-member, so
// label-collisions are unlikely.
type ThemeView struct {
	ID          string            `json:"id"`
	Label       string            `json:"label"`
	Slug        string            `json:"slug"`
	Summary     string            `json:"summary"`
	Weight      float64           `json:"weight"`
	Repos       []string          `json:"repos"`
	MemberCount int               `json:"member_count"`
	FirstSeen   string            `json:"first_seen"`
	LastTouched string            `json:"last_touched"`
	ComputedAt  string            `json:"computed_at"`
	Pinned      bool              `json:"pinned"`
	Members     []ThemeMemberView `json:"members"`
}

// ThemesForRender returns heuristic-engine themes at or above minWeight,
// heaviest first, each with its members. limit caps the count (the
// max_themes render cap); limit <= 0 means no cap. Segment-clusterer
// themes (doc_kind='segment') are excluded — this view is the
// four-stream corpus engine's output.
func (s *Store) ThemesForRender(minWeight float64, limit int) ([]ThemeView, error) {
	q := `
		SELECT t.id, t.label, t.summary, t.weight, t.repos,
		       t.first_seen, t.last_touched, t.computed_at,
		       CASE WHEN p.theme_id IS NULL THEN 0 ELSE 1 END AS pinned
		FROM themes t
		LEFT JOIN theme_pins p ON p.theme_id = t.id
		WHERE t.weight >= ?
		  AND EXISTS (
		    SELECT 1 FROM theme_members m
		    WHERE m.theme_id = t.id
		      AND m.doc_kind IN ('decision','compaction','pattern','vault_user')
		  )
		ORDER BY t.weight DESC, t.id`
	args := []any{minWeight}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ThemeView
	for rows.Next() {
		var v ThemeView
		var reposJSON string
		var pinned int
		if err := rows.Scan(&v.ID, &v.Label, &v.Summary, &v.Weight, &reposJSON,
			&v.FirstSeen, &v.LastTouched, &v.ComputedAt, &pinned); err != nil {
			return nil, err
		}
		v.Pinned = pinned == 1
		_ = json.Unmarshal([]byte(reposJSON), &v.Repos)
		v.Slug = slugify(v.Label)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Members per theme, ordered by similarity so the representative
	// leads the evidence section.
	for i := range out {
		mrows, err := s.readDB.Query(`
			SELECT doc_kind, entity_id, COALESCE(similarity, 0)
			FROM theme_members
			WHERE theme_id = ?
			  AND doc_kind IN ('decision','compaction','pattern','vault_user')
			ORDER BY similarity DESC, entity_id
		`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for mrows.Next() {
			var m ThemeMemberView
			if err := mrows.Scan(&m.Kind, &m.EntityID, &m.Similarity); err != nil {
				mrows.Close()
				return nil, err
			}
			out[i].Members = append(out[i].Members, m)
		}
		mrows.Close()
		out[i].MemberCount = len(out[i].Members)
	}
	return out, nil
}

// ThemeInspect is the full detail for one theme (mnemo_vault_themes_inspect):
// the view plus how it was labelled and whether it is pinned.
type ThemeInspect struct {
	ThemeView
	LabelSource string `json:"label_source"` // "bigram" | "llm" | "vault_user"
	Pinned      bool   `json:"pinned"`
	PinReason   string `json:"pin_reason,omitempty"`
}

// InspectTheme resolves a theme by exact id or by slug (derived from the
// label) and returns its full detail, ignoring the render weight floor
// so any theme is inspectable. Returns nil when nothing matches.
func (s *Store) InspectTheme(ref string) (*ThemeInspect, error) {
	// Load all non-segment themes (small set) and match by id or slug.
	// Matching slug in Go avoids duplicating slugify in SQL.
	views, err := s.ThemesForRender(0, 0)
	if err != nil {
		return nil, err
	}
	var match *ThemeView
	for i := range views {
		if views[i].ID == ref || views[i].Slug == ref {
			match = &views[i]
			break
		}
	}
	if match == nil {
		return nil, nil
	}

	insp := &ThemeInspect{ThemeView: *match, LabelSource: "bigram"}
	var pinnedAt, reason string
	err = s.readDB.QueryRow(
		`SELECT COALESCE(pinned_at,''), COALESCE(reason,'') FROM theme_pins WHERE theme_id = ?`,
		match.ID).Scan(&pinnedAt, &reason)
	if err == nil && pinnedAt != "" {
		insp.Pinned = true
		insp.PinReason = reason
	}
	return insp, nil
}

// SetThemePin pins or unpins a theme (mnemo_vault_themes_pin). A pinned
// theme is exempt from retire_after auto-archival. Idempotent in both
// directions.
func (s *Store) SetThemePin(themeID, reason string, unpin bool) error {
	if unpin {
		_, err := s.writeDB.Exec(`DELETE FROM theme_pins WHERE theme_id = ?`, themeID)
		return err
	}
	_, err := s.writeDB.Exec(`
		INSERT INTO theme_pins (theme_id, pinned_at, reason)
		VALUES (?, ?, ?)
		ON CONFLICT(theme_id) DO UPDATE SET reason = excluded.reason
	`, themeID, time.Now().UTC().Format(time.RFC3339), reason)
	return err
}

// MaybeRecomputeThemes runs a clustering pass only if the last one is
// older than p.RecomputeInterval (or none exists). Called from the vault
// sync loop so the recompute cadence rides the existing timer without a
// dedicated goroutine: however often sync fires, clustering runs at most
// once per interval. Returns the run when one happened, nil when skipped.
func (s *Store) MaybeRecomputeThemes(vaultRoot, trigger string, p ClusterParams) (*ClusterRun, error) {
	interval := p.RecomputeInterval
	if interval <= 0 {
		interval = DefaultClusterInterval
	}
	var last string
	// A missing row scans to '' → zero time → always due.
	_ = s.readDB.QueryRow(`SELECT COALESCE(MAX(started_at), '') FROM cluster_runs`).Scan(&last)
	if last != "" {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			if time.Since(t) < interval {
				return nil, nil
			}
		}
	}
	return s.RecomputeThemes(vaultRoot, trigger, p)
}
