// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNoClusteringOnIngestPath is the structural half of the "global
// clustering is off the hot path" oracle (🎯T64.11). The behavioural
// tests below prove the current code writes no themes; this one stops
// the call being reintroduced later, which would silently restore the
// super-quadratic rebuild that pegged the daemon.
//
// ClusterSealedSegments is retained as dormant, explicitly-invoked
// machinery (cross-session themes may return as an offline analytic),
// so the rule is "not called from production code", not "deleted".
func TestNoClusteringOnIngestPath(t *testing.T) {
	// Its own definition and tests may name it; nothing else may call it.
	allowed := map[string]bool{
		filepath.Join("internal", "store", "segment_cluster.go"): true,
	}
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || allowed[rel] {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, banned := range []string{"ClusterSealedSegments(", "scheduleClusterSealed("} {
			if strings.Contains(string(src), banned) {
				offenders = append(offenders, rel+" calls "+banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("clustering reintroduced on a production path (🎯T64.11 forbids this):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// segmentFixture ingests a two-topic session and returns the store and
// session id. Shared by the 🎯T64.11 tests, which all need a real
// session with real messages.id values to anchor spans to.
func segmentFixture(t *testing.T, sid string) (*Store, string) {
	t.Helper()
	proj := t.TempDir()
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	entries := []map[string]any{
		metaMsg("user", "fix the fts tokenizer bug", base.Format(time.RFC3339), "/Users/a/work/mnemo", "master"),
		msg("assistant", "investigating fts5 diacritics", base.Add(1*time.Minute).Format(time.RFC3339)),
		msg("user", "check the unicode path too", base.Add(2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "tokenizer patched", base.Add(3*time.Minute).Format(time.RFC3339)),
		msg("user", "now implement the vault exporter", base.Add(2*time.Hour).Format(time.RFC3339)),
		msg("assistant", "vault wing scaffolded", base.Add(2*time.Hour+time.Minute).Format(time.RFC3339)),
		msg("user", "add the migration", base.Add(2*time.Hour+2*time.Minute).Format(time.RFC3339)),
		msg("assistant", "migration landed", base.Add(2*time.Hour+3*time.Minute).Format(time.RFC3339)),
	}
	writeJSONL(t, proj, "-Users-a-work-mnemo", sid, entries)
	s := newTestStore(t, proj)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	return s, sid
}

// sessionMsgBounds returns the min/max messages.id for a session.
func sessionMsgBounds(t *testing.T, s *Store, sid string) (int64, int64) {
	t.Helper()
	var lo, hi int64
	if err := s.readDB.QueryRow(
		`SELECT COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM messages WHERE session_id = ?`, sid,
	).Scan(&lo, &hi); err != nil {
		t.Fatal(err)
	}
	if lo == 0 || hi <= lo {
		t.Fatalf("fixture has no usable message range: %d..%d", lo, hi)
	}
	return lo, hi
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPutCompactionSegmentsWritesWindowAndTopicSpans covers the core of
// 🎯T64.11: a compaction contributes a coarse window span carrying its
// own summary, plus the summariser's topic spans nested inside it.
func TestPutCompactionSegmentsWritesWindowAndTopicSpans(t *testing.T) {
	s, sid := segmentFixture(t, "aaaaaaaa-bbbb-cccc-dddd-000000000101")
	lo, hi := sessionMsgBounds(t, s, sid)
	mid := lo + (hi-lo)/2

	err := s.PutCompactionSegments(CompactionSegments{
		SessionID:     sid,
		CompactionID:  1,
		FromMsgID:     lo,
		ToMsgID:       hi,
		WindowSummary: "Fixed the FTS tokenizer, then built the vault exporter.",
		Spans: []CompactionSpan{
			{FromMsgID: lo, ToMsgID: mid, Label: "fts tokenizer bug", Summary: "diacritic handling in the fts5 tokenizer"},
			{FromMsgID: mid + 1, ToMsgID: hi, Label: "vault exporter", Summary: "scaffolded the vault wing and its migration"},
		},
	})
	if err != nil {
		t.Fatalf("PutCompactionSegments: %v", err)
	}

	// Window span: coarse level, compaction method, carries the summary.
	var level int
	var method, summary string
	var compactionID int64
	if err := s.readDB.QueryRow(`
		SELECT level, method, COALESCE(summary,''), COALESCE(compaction_id,0)
		FROM topic_segments
		WHERE session_id = ? AND method = ?`, sid, SegmentMethodCompaction,
	).Scan(&level, &method, &summary, &compactionID); err != nil {
		t.Fatalf("window span missing: %v", err)
	}
	if level != segmentLevelWindow {
		t.Errorf("window span level = %d, want %d", level, segmentLevelWindow)
	}
	if compactionID != 1 {
		t.Errorf("window span compaction_id = %d, want 1", compactionID)
	}
	if summary == "" {
		t.Error("window span lost the compaction summary")
	}

	// Topic spans: fine level, llm method, parented to the window.
	rows, err := s.readDB.Query(`
		SELECT COALESCE(label,''), level, COALESCE(parent_id,''), sealed
		FROM topic_segments
		WHERE session_id = ? AND method = ?
		ORDER BY from_msg_id`, sid, SegmentMethodLLM)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label, parent string
		var lvl, sealed int
		if err := rows.Scan(&label, &lvl, &parent, &sealed); err != nil {
			t.Fatal(err)
		}
		if lvl != segmentLevelTopic {
			t.Errorf("topic span level = %d, want %d", lvl, segmentLevelTopic)
		}
		if parent == "" {
			t.Errorf("topic span %q has no parent window", label)
		}
		if sealed != 1 {
			t.Errorf("topic span %q not sealed; a compacted window is behind the cursor", label)
		}
		labels = append(labels, label)
	}
	if len(labels) != 2 {
		t.Fatalf("got %d topic spans, want 2: %v", len(labels), labels)
	}

	// Re-running replaces rather than duplicates — the backfill and any
	// re-compaction must be safe to repeat.
	if err := s.PutCompactionSegments(CompactionSegments{
		SessionID: sid, CompactionID: 1, FromMsgID: lo, ToMsgID: hi,
		WindowSummary: "Fixed the FTS tokenizer, then built the vault exporter.",
		Spans: []CompactionSpan{
			{FromMsgID: lo, ToMsgID: mid, Label: "fts tokenizer bug", Summary: "diacritic handling"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM topic_segments WHERE compaction_id = 1`); n != 2 {
		t.Errorf("re-run left %d spans, want 2 (1 window + 1 topic)", n)
	}
}

// TestCompactionSpansSupersedeStructuralForExpand proves the
// supersession rule: where a compaction has contributed spans, search
// expansion resolves to those rather than the structural sketch, while
// structural spans remain in place as coverage.
func TestCompactionSpansSupersedeStructuralForExpand(t *testing.T) {
	s, sid := segmentFixture(t, "aaaaaaaa-bbbb-cccc-dddd-000000000102")
	if err := s.SegmentSession(sid); err != nil {
		t.Fatal(err)
	}
	structural := countRows(t, s,
		`SELECT COUNT(*) FROM topic_segments WHERE session_id = ? AND method = ?`,
		sid, SegmentMethodStructural)
	if structural == 0 {
		t.Fatal("fixture produced no structural spans; nothing to supersede")
	}

	lo, hi := sessionMsgBounds(t, s, sid)
	if err := s.PutCompactionSegments(CompactionSegments{
		SessionID: sid, CompactionID: 7, FromMsgID: lo, ToMsgID: hi,
		WindowSummary: "window summary",
		Spans: []CompactionSpan{
			{FromMsgID: lo, ToMsgID: hi, Label: "llm span", Summary: "what actually happened"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.AttachSegmentExpand(
		[]SearchResult{{MessageID: int(lo)}}, SegmentExpandFine)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Segment == nil {
		t.Fatal("no enclosing span attached")
	}
	if got[0].Segment.Label != "llm span" {
		t.Errorf("expand picked %q; want the compaction-derived span to win over structural",
			got[0].Segment.Label)
	}

	// Structural spans survive — they are coverage for ranges no
	// summariser has reached, not something the fold deletes.
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM topic_segments WHERE session_id = ? AND method = ?`,
		sid, SegmentMethodStructural); n != structural {
		t.Errorf("structural spans changed from %d to %d; they must remain as the provisional layer",
			structural, n)
	}

	// expand="none" stays inert.
	none, err := s.AttachSegmentExpand([]SearchResult{{MessageID: int(lo)}}, SegmentExpandNone)
	if err != nil {
		t.Fatal(err)
	}
	if none[0].Segment != nil {
		t.Error(`expand="none" must not attach a span`)
	}
}

// TestBackfillCompactionSegmentsProjectsHistory covers the historical
// wrinkle: sessions summarised before 🎯T64.11 have compactions but no
// spans. Projecting them costs no LLM call and is detected by the
// compaction_id join.
func TestBackfillCompactionSegmentsProjectsHistory(t *testing.T) {
	s, sid := segmentFixture(t, "aaaaaaaa-bbbb-cccc-dddd-000000000103")
	lo, hi := sessionMsgBounds(t, s, sid)

	if _, err := s.PutCompaction(Compaction{
		SessionID:   sid,
		Model:       "stub",
		EntryIDFrom: lo - 1,
		EntryIDTo:   hi,
		Summary:     "Investigated the tokenizer bug and shipped the vault exporter.",
		PayloadJSON: `{"summary":"x"}`,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := s.BackfillCompactionSegments(0)
	if err != nil {
		t.Fatalf("BackfillCompactionSegments: %v", err)
	}
	if n != 1 {
		t.Fatalf("projected %d compactions, want 1", n)
	}
	if got := countRows(t, s,
		`SELECT COUNT(*) FROM topic_segments WHERE session_id = ? AND method = ?`,
		sid, SegmentMethodCompaction); got != 1 {
		t.Errorf("got %d projected window spans, want 1", got)
	}

	// Idempotent: an already-projected compaction is skipped, so this is
	// safe on every backfill.
	again, err := s.BackfillCompactionSegments(0)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-run projected %d compactions, want 0", again)
	}
}

// TestSegmentAllSessionsWritesNoThemes is the load-bearing oracle for
// "global clustering is off the hot path". The backfill pass must leave
// the theme tables untouched — that super-quadratic rebuild was the
// dominant CPU cost this target removes.
func TestSegmentAllSessionsWritesNoThemes(t *testing.T) {
	s, sid := segmentFixture(t, "aaaaaaaa-bbbb-cccc-dddd-000000000104")

	if err := s.SegmentAllSessions(); err != nil {
		t.Fatalf("SegmentAllSessions: %v", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM themes`); n != 0 {
		t.Errorf("SegmentAllSessions wrote %d theme rows; clustering must stay off the hot path", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM theme_members`); n != 0 {
		t.Errorf("SegmentAllSessions wrote %d theme_members rows; clustering must stay off the hot path", n)
	}
	// It still did its actual job.
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM topic_segments WHERE session_id = ?`, sid); n == 0 {
		t.Error("SegmentAllSessions produced no spans")
	}
}

// TestIngestWritesNoThemes proves the same for the live ingest path: a
// transcript append must never trigger a corpus-wide recompute.
func TestIngestWritesNoThemes(t *testing.T) {
	s, _ := segmentFixture(t, "aaaaaaaa-bbbb-cccc-dddd-000000000105")
	if n := countRows(t, s, `SELECT COUNT(*) FROM themes`); n != 0 {
		t.Errorf("ingest wrote %d theme rows; the live path must not cluster", n)
	}
}

// TestSpanSearchIsThematicWithoutThemes covers the product claim: you
// retrieve by meaning through search over span text, ranked across
// sessions, with no precomputed theme objects involved.
func TestSpanSearchIsThematicWithoutThemes(t *testing.T) {
	s, sidA := segmentFixture(t, "aaaaaaaa-bbbb-cccc-dddd-000000000106")
	loA, hiA := sessionMsgBounds(t, s, sidA)
	if err := s.PutCompactionSegments(CompactionSegments{
		SessionID: sidA, CompactionID: 11, FromMsgID: loA, ToMsgID: hiA,
		WindowSummary: "session A window",
		Spans: []CompactionSpan{{
			FromMsgID: loA, ToMsgID: hiA,
			Label:   "fts tokenizer diacritics",
			Summary: "unicode normalisation in the search tokenizer",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	segs, err := s.QuerySegments(SegmentQuery{FTSQuery: "tokenizer", Limit: 10})
	if err != nil {
		t.Fatalf("QuerySegments: %v", err)
	}
	if len(segs) == 0 {
		t.Fatal("span FTS returned nothing; thematic search must work off span text alone")
	}
	found := false
	for _, sg := range segs {
		if sg.Label == "fts tokenizer diacritics" {
			found = true
		}
	}
	if !found {
		t.Errorf("span FTS missed the matching span; got %d results", len(segs))
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM themes`); n != 0 {
		t.Error("thematic span search must not depend on precomputed themes")
	}
}
