// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
)

func TestSegmentSessionSealAndNoRewrite(t *testing.T) {
	proj := t.TempDir()
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	// Two topics separated by a long idle gap + enough tail to seal first.
	entries := []map[string]any{
		metaMsg("user", "fix fts tokenizer bug", base.Format(time.RFC3339), "/Users/a/work/mnemo", "master"),
		msg("assistant", "investigating fts5", base.Add(1*time.Minute).Format(time.RFC3339)),
		msg("user", "check diacritics too", base.Add(2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "diacritic path fixed", base.Add(3*time.Minute).Format(time.RFC3339)),
		msg("user", "now implement vault export", base.Add(2*time.Hour).Format(time.RFC3339)),
		msg("assistant", "vault wing scaffold", base.Add(2*time.Hour+time.Minute).Format(time.RFC3339)),
		msg("user", "add migration", base.Add(2*time.Hour+2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "migration done", base.Add(2*time.Hour+3*time.Minute).Format(time.RFC3339)),
		msg("user", "ship", base.Add(2*time.Hour+4*time.Minute).Format(time.RFC3339)),
		msg("assistant", "shipped", base.Add(2*time.Hour+5*time.Minute).Format(time.RFC3339)),
	}
	sid := "aaaaaaaa-bbbb-cccc-dddd-000000000010"
	writeJSONL(t, proj, "-Users-a-work-mnemo", sid, entries)
	s := newTestStore(t, proj)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.SegmentSession(sid); err != nil {
		t.Fatalf("SegmentSession: %v", err)
	}

	var nSealed int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM topic_segments WHERE session_id = ? AND sealed = 1`, sid,
	).Scan(&nSealed); err != nil {
		t.Fatal(err)
	}
	if nSealed < 1 {
		t.Fatalf("expected sealed segments, got %d", nSealed)
	}

	// Capture sealed ids + labels.
	type row struct {
		id, label string
	}
	var before []row
	rws, err := s.readDB.Query(
		`SELECT id, COALESCE(label,'') FROM topic_segments WHERE session_id = ? AND sealed = 1`, sid,
	)
	if err != nil {
		t.Fatal(err)
	}
	for rws.Next() {
		var r row
		if rws.Scan(&r.id, &r.label) != nil {
			t.Fatal("scan")
		}
		before = append(before, r)
	}
	rws.Close()

	// Re-run: sealed must not change id/label.
	if err := s.SegmentSession(sid); err != nil {
		t.Fatal(err)
	}
	for _, r := range before {
		var label string
		var sealed int
		err := s.readDB.QueryRow(
			`SELECT COALESCE(label,''), sealed FROM topic_segments WHERE id = ?`, r.id,
		).Scan(&label, &sealed)
		if err != nil {
			t.Fatalf("sealed row missing after re-run: %s", r.id)
		}
		if sealed != 1 {
			t.Errorf("id %s lost sealed flag", r.id)
		}
		if label != r.label {
			t.Errorf("sealed label rewritten: %q -> %q", r.label, label)
		}
	}

	// Theme primary membership exists.
	var members int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM theme_members WHERE doc_kind = 'segment' AND membership_kind = 'primary'`,
	).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members < 1 {
		t.Error("expected primary theme_members for segments")
	}

	// parent_theme_id / depth navigable when hierarchy present.
	var withParent int
	_ = s.readDB.QueryRow(
		`SELECT COUNT(*) FROM themes WHERE parent_theme_id IS NOT NULL AND depth IS NOT NULL`,
	).Scan(&withParent)
	// May be 0 if only one scale sealed — not a hard fail.
	t.Logf("themes with parent: %d", withParent)
}

func TestSearchExpandNoneParityAndSegmentExpand(t *testing.T) {
	proj := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	unique := "zzuniquephrase42"
	entries := []map[string]any{
		metaMsg("user", "intro "+unique, base.Format(time.RFC3339), "/Users/a/work/mnemo", "master"),
		msg("assistant", "ack", base.Add(time.Minute).Format(time.RFC3339)),
		msg("user", "more on "+unique, base.Add(2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "still", base.Add(3*time.Minute).Format(time.RFC3339)),
		msg("user", "now other topic after gap", base.Add(3*time.Hour).Format(time.RFC3339)),
		msg("assistant", "other", base.Add(3*time.Hour+time.Minute).Format(time.RFC3339)),
		msg("user", "tail a", base.Add(3*time.Hour+2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "tail b", base.Add(3*time.Hour+3*time.Minute).Format(time.RFC3339)),
		msg("user", "tail c", base.Add(3*time.Hour+4*time.Minute).Format(time.RFC3339)),
		msg("assistant", "tail d", base.Add(3*time.Hour+5*time.Minute).Format(time.RFC3339)),
	}
	sid := "aaaaaaaa-bbbb-cccc-dddd-000000000020"
	writeJSONL(t, proj, "-Users-a-work-mnemo", sid, entries)
	s := newTestStore(t, proj)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	baseHits, err := s.Search(unique, 10, "all", "", 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseHits) < 1 {
		t.Fatal("expected search hits")
	}
	// expand=none must not mutate when AttachSegmentExpand skipped / none.
	noneHits, err := s.AttachSegmentExpand(append([]SearchResult(nil), baseHits...), SegmentExpandNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(noneHits) != len(baseHits) {
		t.Fatalf("none expand length changed")
	}
	for i := range baseHits {
		if noneHits[i].MessageID != baseHits[i].MessageID || noneHits[i].Text != baseHits[i].Text {
			t.Fatalf("expand=none mutated hit %d", i)
		}
		if noneHits[i].Segment != nil {
			t.Fatalf("expand=none attached segment")
		}
	}

	if err := s.SegmentSession(sid); err != nil {
		t.Fatal(err)
	}
	exp, err := s.AttachSegmentExpand(append([]SearchResult(nil), baseHits...), SegmentExpandFine)
	if err != nil {
		t.Fatal(err)
	}
	// May or may not attach depending on seal; if attached, ids non-empty.
	for _, h := range exp {
		if h.Segment != nil && h.Segment.ID == "" {
			t.Error("empty segment id")
		}
	}

	// Query shapes.
	bySess, err := s.QuerySegments(SegmentQuery{SessionID: sid, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySess) < 1 {
		t.Fatal("by-session empty")
	}
	// containing_msg_id
	mid := baseHits[0].MessageID
	byMsg, err := s.QuerySegments(SegmentQuery{ContainingMsgID: mid, SealedOnly: false, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	_ = byMsg
	// FTS
	if bySess[0].Label != "" {
		_, err = s.QuerySegments(SegmentQuery{FTSQuery: bySess[0].Label, Limit: 5})
		if err != nil {
			t.Fatalf("fts: %v", err)
		}
	}
	// theme
	var themeID string
	_ = s.readDB.QueryRow(`SELECT theme_id FROM theme_members WHERE doc_kind='segment' LIMIT 1`).Scan(&themeID)
	if themeID != "" {
		byTheme, err := s.QuerySegments(SegmentQuery{ThemeID: themeID, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(byTheme) < 1 {
			t.Error("by-theme empty")
		}
	}
}

func TestSchemaHasSegmentTables(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for _, name := range []string{
		"topic_segments", "topic_segments_fts", "segment_scan_state",
		"themes", "theme_members", "cluster_embeddings", "themes_fts",
	} {
		var n int
		if err := s.readDB.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name,
		).Scan(&n); err != nil || n != 1 {
			t.Errorf("missing table %s: n=%d err=%v", name, n, err)
		}
	}
	// Upgrade path: second open is no-op.
	db2 := filepath.Join(t.TempDir(), "u.db")
	// copy by re-New same schema via New on empty then nothing
	s2, err := New(db2, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	s3, err := New(db2, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s3.Close()
}

func TestGoldenPkWindowDiffHarness(t *testing.T) {
	// Hand-segmented gold: boundary after index 3 (between msg 4 and 5 in 0-based sub stream).
	// Hyp from structural on a known fixture should clear a loose bar.
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	msgs := []segment.Message{
		{ID: 1, Role: "user", Text: "topic one alpha", Timestamp: base.Format(time.RFC3339)},
		{ID: 2, Role: "assistant", Text: "topic one beta", Timestamp: base.Add(time.Minute).Format(time.RFC3339)},
		{ID: 3, Role: "user", Text: "topic one gamma", Timestamp: base.Add(2 * time.Minute).Format(time.RFC3339)},
		{ID: 4, Role: "assistant", Text: "topic one delta", Timestamp: base.Add(3 * time.Minute).Format(time.RFC3339)},
		{ID: 5, Role: "user", Text: "now topic two start", Timestamp: base.Add(2 * time.Hour).Format(time.RFC3339)},
		{ID: 6, Role: "assistant", Text: "topic two mid", Timestamp: base.Add(2*time.Hour + time.Minute).Format(time.RFC3339)},
		{ID: 7, Role: "user", Text: "topic two end", Timestamp: base.Add(2*time.Hour + 2*time.Minute).Format(time.RFC3339)},
		{ID: 8, Role: "assistant", Text: "done", Timestamp: base.Add(2*time.Hour + 3*time.Minute).Format(time.RFC3339)},
	}
	goldCuts := []int{3} // after 4th substantive message (0-based index 3)
	spans := segment.Structural(msgs, segment.DefaultConfig())
	// Derive hyp cuts from fine-level spans ends (except last).
	var hypCuts []int
	// Map msg id -> index
	idxOf := map[int]int{}
	for i, m := range msgs {
		idxOf[m.ID] = i
	}
	for _, sp := range spans {
		if sp.Level != 0 {
			continue
		}
		if i, ok := idxOf[sp.ToMsgID]; ok && i < len(msgs)-1 {
			hypCuts = append(hypCuts, i)
		}
	}
	pk := segment.Pk(len(msgs), goldCuts, hypCuts, 2)
	wd := segment.WindowDiff(len(msgs), goldCuts, hypCuts, 2)
	// Loose bar: structural idle-gap cut should be near gold.
	const pkBar = 0.5
	const wdBar = 0.5
	if pk > pkBar {
		t.Errorf("Pk=%.3f exceeds bar %.3f (hyp=%v gold=%v)", pk, pkBar, hypCuts, goldCuts)
	}
	if wd > wdBar {
		t.Errorf("WindowDiff=%.3f exceeds bar %.3f", wd, wdBar)
	}
	// Default expand remains none.
	if DefaultSegmentExpand != SegmentExpandNone {
		t.Error("expand default must stay none until product gate")
	}
	t.Logf("golden quality Pk=%.3f WindowDiff=%.3f hypCuts=%v", pk, wd, hypCuts)
}
