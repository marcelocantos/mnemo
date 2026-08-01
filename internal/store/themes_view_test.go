// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func insertTheme(t *testing.T, s *Store, id, label string, weight float64, repos string) {
	t.Helper()
	if _, err := s.writeDB.Exec(
		`INSERT INTO themes (id, label, summary, weight, repos, depth, first_seen, last_touched, computed_at)
		 VALUES (?, ?, 'sum', ?, ?, 0, '2026-07-01', '2026-07-02', '2026-07-02T00:00:00Z')`,
		id, label, weight, repos); err != nil {
		t.Fatal(err)
	}
}

func insertThemeMember(t *testing.T, s *Store, themeID, kind, entity string, sim float64) {
	t.Helper()
	if _, err := s.writeDB.Exec(
		`INSERT INTO theme_members (theme_id, doc_kind, entity_id, membership_kind, similarity)
		 VALUES (?, ?, ?, 'primary', ?)`,
		themeID, kind, entity, sim); err != nil {
		t.Fatal(err)
	}
}

func TestThemesForRenderWeightFloor(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	insertTheme(t, s, "theme_light", "Light", 2.0, `["mnemo"]`)
	insertThemeMember(t, s, "theme_light", "decision", "1", 0.9)

	insertTheme(t, s, "theme_heavy", "Schema Migration", 5.0, `["mnemo","bullseye"]`)
	insertThemeMember(t, s, "theme_heavy", "decision", "2", 0.95)
	insertThemeMember(t, s, "theme_heavy", "compaction", "3", 0.80)

	// A dormant segment theme must never surface in this view.
	insertTheme(t, s, "theme_seg", "Seg", 9.0, `[]`)
	insertThemeMember(t, s, "theme_seg", "segment", "seg1", 1.0)

	views, err := s.ThemesForRender(3.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 view (weight>=3, non-segment), got %d: %+v", len(views), views)
	}
	v := views[0]
	if v.ID != "theme_heavy" || v.Slug != "schema-migration" {
		t.Errorf("unexpected view id/slug: %+v", v)
	}
	if v.MemberCount != 2 || len(v.Members) != 2 {
		t.Errorf("want 2 members, got %d", v.MemberCount)
	}
	// Members ordered by similarity desc → representative first.
	if v.Members[0].EntityID != "2" {
		t.Errorf("members not ordered by similarity: %+v", v.Members)
	}
	if len(v.Repos) != 2 || v.Repos[0] != "mnemo" {
		t.Errorf("repos not parsed: %+v", v.Repos)
	}
}

func TestInspectThemeByIDAndSlug(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	insertTheme(t, s, "theme_heavy", "Schema Migration", 5.0, `["mnemo"]`)
	insertThemeMember(t, s, "theme_heavy", "decision", "2", 0.95)
	insertThemeMember(t, s, "theme_heavy", "pattern", "pattern_x", 0.60)

	byID, err := s.InspectTheme("theme_heavy")
	if err != nil {
		t.Fatal(err)
	}
	if byID == nil || byID.ID != "theme_heavy" {
		t.Fatalf("inspect by id failed: %+v", byID)
	}
	if byID.LabelSource != "bigram" || byID.Pinned {
		t.Errorf("unexpected label-source/pin: %+v", byID)
	}
	if len(byID.Members) != 2 {
		t.Errorf("want 2 members, got %d", len(byID.Members))
	}

	bySlug, err := s.InspectTheme("schema-migration")
	if err != nil {
		t.Fatal(err)
	}
	if bySlug == nil || bySlug.ID != "theme_heavy" {
		t.Errorf("inspect by slug failed: %+v", bySlug)
	}

	miss, err := s.InspectTheme("nope")
	if err != nil {
		t.Fatal(err)
	}
	if miss != nil {
		t.Errorf("want nil for unknown ref, got %+v", miss)
	}
}

func TestSetThemePinRoundTrip(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	insertTheme(t, s, "theme_heavy", "Schema Migration", 5.0, `["mnemo"]`)
	insertThemeMember(t, s, "theme_heavy", "decision", "2", 0.95)

	if err := s.SetThemePin("theme_heavy", "keep for the retro", false); err != nil {
		t.Fatal(err)
	}
	insp, _ := s.InspectTheme("theme_heavy")
	if insp == nil || !insp.Pinned || insp.PinReason != "keep for the retro" {
		t.Fatalf("pin not reflected: %+v", insp)
	}

	// Idempotent re-pin updates the reason, no error.
	if err := s.SetThemePin("theme_heavy", "still keeping", false); err != nil {
		t.Fatal(err)
	}
	if got := countScalar(t, s, "SELECT COUNT(*) FROM theme_pins"); got != 1 {
		t.Errorf("re-pin should not duplicate rows, got %d", got)
	}

	if err := s.SetThemePin("theme_heavy", "", true); err != nil {
		t.Fatal(err)
	}
	insp2, _ := s.InspectTheme("theme_heavy")
	if insp2 == nil || insp2.Pinned {
		t.Errorf("unpin not reflected: %+v", insp2)
	}
}

func TestMaybeRecomputeThemesGating(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedClusterCorpus(t, s)

	// First call: no prior run → executes.
	run, err := s.MaybeRecomputeThemes("", time.Hour, "interval")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil {
		t.Fatal("first MaybeRecomputeThemes should run, got nil")
	}

	// Immediate second call within the interval → skipped.
	run2, err := s.MaybeRecomputeThemes("", time.Hour, "interval")
	if err != nil {
		t.Fatal(err)
	}
	if run2 != nil {
		t.Errorf("second call within interval should skip, got run %+v", run2)
	}

	// A zero interval forces a run every time.
	run3, err := s.MaybeRecomputeThemes("", 0, "interval")
	if err != nil {
		t.Fatal(err)
	}
	// 0 resolves to DefaultClusterInterval, so still skipped — assert the
	// documented behaviour rather than a re-run.
	if run3 != nil {
		t.Errorf("zero interval resolves to default and should skip, got %+v", run3)
	}
}
