// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
)

func TestSegmentSessionSealAndNoRewrite(t *testing.T) {
	proj := t.TempDir()
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
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
	if err := s.ClusterSealedSegments(); err != nil {
		t.Fatal(err)
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
}

func TestClusterMultiMemberPrimarySecondaryDendrogram(t *testing.T) {
	// Drive ClusterSealedSegments on controlled sealed rows (not via
	// structural segmenter), so multi-member cut + dendrogram parents
	// are deterministic.
	s := newTestStore(t, t.TempDir())
	now := time.Now().UTC().Format(time.RFC3339)
	// Two near-duplicate FTS segments (should merge at cut) + two near-
	// duplicate vault segments (second leaf) → ≥2 leaf themes → phase-2
	// parent edge.
	segs := []struct {
		id, label, summary string
	}{
		{"seg_fts_aaaa01", "fts tokenizer diacritic handling", "fix fts5 tokenizer diacritic unicode"},
		{"seg_fts_bbbb02", "fts tokenizer diacritic handling again", "fix fts5 tokenizer diacritic unicode path"},
		{"seg_vault_cc03", "vault export migration documentation", "obsidian vault export migration wing pages"},
		{"seg_vault_dd04", "vault export migration docs", "obsidian vault export migration wing documentation"},
	}
	for i, sg := range segs {
		_, err := s.writeDB.Exec(`
			INSERT INTO topic_segments (
				id, session_id, from_msg_id, to_msg_id, level, parent_id,
				method, confidence, sealed, label, summary, repo,
				first_ts, last_ts, computed_at
			) VALUES (?, ?, ?, ?, 0, NULL, 'structural', 0.9, 1, ?, ?, 'mnemo', ?, ?, ?)
		`, sg.id, fmt.Sprintf("sess-%d", i), 10*i+1, 10*i+5, sg.label, sg.summary, now, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClusterSealedSegments(); err != nil {
		t.Fatal(err)
	}

	// Exactly one primary per sealed segment.
	rows, err := s.readDB.Query(`
		SELECT s.id, COUNT(CASE WHEN m.membership_kind = 'primary' THEN 1 END)
		FROM topic_segments s
		LEFT JOIN theme_members m ON m.entity_id = s.id AND m.doc_kind = 'segment'
		WHERE s.sealed = 1
		GROUP BY s.id
	`)
	if err != nil {
		t.Fatal(err)
	}
	var sealedCount int
	for rows.Next() {
		var id string
		var primaries int
		if rows.Scan(&id, &primaries) != nil {
			t.Fatal("scan")
		}
		sealedCount++
		if primaries != 1 {
			t.Errorf("segment %s has %d primaries, want 1", id, primaries)
		}
	}
	rows.Close()
	if sealedCount != 4 {
		t.Fatalf("sealedCount=%d want 4", sealedCount)
	}

	// Multi-member leaf themes (FTS pair and/or vault pair).
	var multi int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT theme_id FROM theme_members
			WHERE doc_kind = 'segment' AND membership_kind = 'primary'
			GROUP BY theme_id HAVING COUNT(*) >= 2
		)
	`).Scan(&multi); err != nil {
		t.Fatal(err)
	}
	if multi < 1 {
		t.Fatalf("expected multi-member theme; dump: %s", dumpThemes(t, s))
	}

	// ≥2 leaf themes so dendrogram phase has work.
	var leafThemes int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(DISTINCT theme_id) FROM theme_members
		WHERE doc_kind = 'segment' AND membership_kind = 'primary'
	`).Scan(&leafThemes); err != nil {
		t.Fatal(err)
	}
	if leafThemes < 2 {
		t.Fatalf("need ≥2 leaf themes for dendrogram, got %d: %s", leafThemes, dumpThemes(t, s))
	}

	// Secondary only ≥ threshold.
	secRows, err := s.readDB.Query(`
		SELECT similarity FROM theme_members
		WHERE doc_kind = 'segment' AND membership_kind = 'secondary'
	`)
	if err != nil {
		t.Fatal(err)
	}
	for secRows.Next() {
		var sim float64
		if secRows.Scan(&sim) != nil {
			t.Fatal("scan sim")
		}
		if sim < SecondaryThemeThreshold {
			t.Errorf("secondary similarity %.3f < threshold %.3f", sim, SecondaryThemeThreshold)
		}
	}
	secRows.Close()

	// Dendrogram required: parent_theme_id edges + ThemeAncestors.
	var withParent int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM themes
		WHERE parent_theme_id IS NOT NULL AND parent_theme_id != ''
	`).Scan(&withParent); err != nil {
		t.Fatal(err)
	}
	if withParent < 1 {
		t.Fatalf("expected dendrogram parent_theme_id edges, got 0: %s", dumpThemes(t, s))
	}
	var child, parent string
	if err := s.readDB.QueryRow(`
		SELECT id, parent_theme_id FROM themes
		WHERE parent_theme_id IS NOT NULL AND parent_theme_id != ''
		LIMIT 1
	`).Scan(&child, &parent); err != nil {
		t.Fatal(err)
	}
	chain, err := s.ThemeAncestors(child)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) < 2 || chain[0] != child {
		t.Fatalf("ThemeAncestors=%v want [child, …parent…]", chain)
	}
	foundParent := false
	for _, id := range chain {
		if id == parent {
			foundParent = true
		}
	}
	if !foundParent {
		t.Errorf("parent %s not in ancestors %v", parent, chain)
	}
}

func dumpThemes(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.readDB.Query(`
		SELECT t.id, t.label, COALESCE(t.parent_theme_id,''), COALESCE(t.depth,0),
		       (SELECT COUNT(*) FROM theme_members m WHERE m.theme_id = t.id AND m.membership_kind='primary')
		FROM themes t
	`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b string
	for rows.Next() {
		var id, label, parent string
		var depth, n int
		_ = rows.Scan(&id, &label, &parent, &depth, &n)
		b += fmt.Sprintf("%s n=%d d=%d p=%s %q; ", id, n, depth, parent, label)
	}
	return b
}

func TestWatermarkSkipsFullySegmentedSessions(t *testing.T) {
	proj := t.TempDir()
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	entries := []map[string]any{
		metaMsg("user", "alpha topic start", base.Format(time.RFC3339), "/Users/a/work/mnemo", "master"),
		msg("assistant", "alpha mid", base.Add(time.Minute).Format(time.RFC3339)),
		msg("user", "alpha end", base.Add(2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "alpha done", base.Add(3*time.Minute).Format(time.RFC3339)),
		msg("user", "now beta topic", base.Add(2*time.Hour).Format(time.RFC3339)),
		msg("assistant", "beta mid", base.Add(2*time.Hour+time.Minute).Format(time.RFC3339)),
		msg("user", "beta end", base.Add(2*time.Hour+2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "beta done", base.Add(2*time.Hour+3*time.Minute).Format(time.RFC3339)),
		msg("user", "tail1", base.Add(2*time.Hour+4*time.Minute).Format(time.RFC3339)),
		msg("assistant", "tail2", base.Add(2*time.Hour+5*time.Minute).Format(time.RFC3339)),
	}
	sid := "aaaaaaaa-bbbb-cccc-dddd-000000000201"
	writeJSONL(t, proj, "-Users-a-work-mnemo", sid, entries)
	s := newTestStore(t, proj)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.SegmentSession(sid); err != nil {
		t.Fatal(err)
	}
	var through int
	if err := s.readDB.QueryRow(
		`SELECT segmented_through_id FROM segment_scan_state WHERE session_id = ?`, sid,
	).Scan(&through); err != nil {
		t.Fatal(err)
	}
	if through <= 0 {
		t.Fatal("watermark not advanced")
	}
	// Force watermark to cover all messages so SegmentSession no-ops.
	var maxID int
	if err := s.readDB.QueryRow(`SELECT MAX(id) FROM messages WHERE session_id = ?`, sid).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`UPDATE segment_scan_state SET segmented_through_id = ? WHERE session_id = ?`,
		maxID, sid,
	); err != nil {
		t.Fatal(err)
	}
	// Delete unsealed only; set a marker on scanned_at then ensure SegmentSession
	// does not change scanned_at when skipped.
	marker := "2000-01-01T00:00:00Z"
	if _, err := s.writeDB.Exec(
		`UPDATE segment_scan_state SET scanned_at = ? WHERE session_id = ?`, marker, sid,
	); err != nil {
		t.Fatal(err)
	}
	if err := s.SegmentSession(sid); err != nil {
		t.Fatal(err)
	}
	var scanned string
	if err := s.readDB.QueryRow(
		`SELECT scanned_at FROM segment_scan_state WHERE session_id = ?`, sid,
	).Scan(&scanned); err != nil {
		t.Fatal(err)
	}
	if scanned != marker {
		t.Fatalf("SegmentSession ran despite watermark: scanned_at=%q want %q", scanned, marker)
	}

	// SegmentAllSessions dirty filter: session fully watermarked should not
	// appear as needing work (count of dirty queries).
	var dirty int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*)
		FROM session_summary ss
		LEFT JOIN segment_scan_state st ON st.session_id = ss.session_id
		WHERE COALESCE(
			(SELECT MAX(m.id) FROM messages m WHERE m.session_id = ss.session_id), 0
		) > COALESCE(st.segmented_through_id, 0)
	`).Scan(&dirty); err != nil {
		t.Fatal(err)
	}
	if dirty != 0 {
		t.Fatalf("expected 0 dirty sessions, got %d", dirty)
	}
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
	if err := s.ClusterSealedSegments(); err != nil {
		t.Fatal(err)
	}
	exp, err := s.AttachSegmentExpand(append([]SearchResult(nil), baseHits...), SegmentExpandFine)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range exp {
		if h.Segment != nil && h.Segment.ID == "" {
			t.Error("empty segment id")
		}
	}

	bySess, err := s.QuerySegments(SegmentQuery{SessionID: sid, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySess) < 1 {
		t.Fatal("by-session empty")
	}
	mid := baseHits[0].MessageID
	if _, err := s.QuerySegments(SegmentQuery{ContainingMsgID: mid, SealedOnly: false, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	var themeID string
	_ = s.readDB.QueryRow(`SELECT theme_id FROM theme_members WHERE doc_kind='segment' LIMIT 1`).Scan(&themeID)
	if themeID != "" {
		if _, err := s.QuerySegments(SegmentQuery{ThemeID: themeID, Limit: 10}); err != nil {
			t.Fatal(err)
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
	db2 := filepath.Join(t.TempDir(), "u.db")
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
	goldCuts := []int{3}
	spans := segment.Structural(msgs, segment.DefaultConfig())
	var hypCuts []int
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
	const pkBar = 0.5
	const wdBar = 0.5
	if pk > pkBar {
		t.Errorf("Pk=%.3f exceeds bar %.3f (hyp=%v gold=%v)", pk, pkBar, hypCuts, goldCuts)
	}
	if wd > wdBar {
		t.Errorf("WindowDiff=%.3f exceeds bar %.3f", wd, wdBar)
	}
	if DefaultSegmentExpand != SegmentExpandNone {
		t.Error("expand default must stay none until product gate")
	}
	t.Logf("golden quality Pk=%.3f WindowDiff=%.3f hypCuts=%v", pk, wd, hypCuts)
}
