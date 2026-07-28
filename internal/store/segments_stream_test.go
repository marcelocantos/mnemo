// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestFinalisationSupersedesStreamSpans is the 🎯T132.3 acceptance.
//
// Batch has hindsight the live watcher could not have: it sees the whole
// window at once. So the finalised span wins at retrieval — but the
// stream span must survive, demoted, because the divergence between what
// the stream believed and what hindsight concluded is the freshness
// metric, and deleting the loser deletes the measurement.
func TestFinalisationSupersedesStreamSpans(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	if err := s.PutStreamSpans([]StreamSpan{
		{SessionID: "sess-f", FromMsgID: 10, ToMsgID: 20, Label: "live guess", Summary: "as it happened"},
		{SessionID: "sess-f", FromMsgID: 200, ToMsgID: 210, Label: "far away", Summary: "different window"},
	}); err != nil {
		t.Fatalf("PutStreamSpans: %v", err)
	}

	// Finalise a window covering the first stream span only.
	if err := s.PutCompactionSegments(CompactionSegments{
		SessionID: "sess-f", CompactionID: 1, FromMsgID: 1, ToMsgID: 100,
		WindowSummary: "the window, seen whole",
		Spans: []CompactionSpan{
			{FromMsgID: 10, ToMsgID: 25, Label: "with hindsight", Summary: "actually one topic"},
		},
	}); err != nil {
		t.Fatalf("PutCompactionSegments: %v", err)
	}

	var superseder string
	if err := s.readDB.QueryRow(`
		SELECT COALESCE(superseded_by, '') FROM topic_segments
		WHERE session_id = 'sess-f' AND method = ? AND from_msg_id = 10
	`, SegmentMethodStream).Scan(&superseder); err != nil {
		t.Fatal(err)
	}
	if superseder == "" {
		t.Error("the overlapped stream span was not superseded by finalisation")
	}

	// It must still be there. Demoted, not deleted.
	var n int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM topic_segments
		WHERE session_id = 'sess-f' AND method = ? AND from_msg_id = 10
	`, SegmentMethodStream).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("finalisation deleted the stream span instead of demoting it — " +
			"that would delete the freshness measurement with it")
	}

	// A stream span outside the finalised window keeps standing.
	var far string
	if err := s.readDB.QueryRow(`
		SELECT COALESCE(superseded_by, '') FROM topic_segments
		WHERE session_id = 'sess-f' AND method = ? AND from_msg_id = 200
	`, SegmentMethodStream).Scan(&far); err != nil {
		t.Fatal(err)
	}
	if far != "" {
		t.Error("a stream span outside the finalised window was superseded")
	}
}

// TestFreshnessDiffIsComputableFromRetainedSpans: the metric falls out of
// keeping both views, with nothing instrumented.
func TestFreshnessDiffIsComputableFromRetainedSpans(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	if err := s.PutStreamSpans([]StreamSpan{
		{SessionID: "sess-d", FromMsgID: 1, ToMsgID: 10, Label: "a"},
		{SessionID: "sess-d", FromMsgID: 11, ToMsgID: 30, Label: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCompactionSegments(CompactionSegments{
		SessionID: "sess-d", CompactionID: 7, FromMsgID: 1, ToMsgID: 40,
		WindowSummary: "window",
		Spans: []CompactionSpan{
			{FromMsgID: 1, ToMsgID: 14, Label: "a'"},
			{FromMsgID: 15, ToMsgID: 30, Label: "b'"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	diff, err := s.StreamFreshnessDiff("sess-d")
	if err != nil {
		t.Fatalf("StreamFreshnessDiff: %v", err)
	}
	if diff.StreamSpans != 2 {
		t.Errorf("stream spans = %d, want 2", diff.StreamSpans)
	}
	if diff.FinalSpans == 0 {
		t.Error("no finalised spans counted")
	}
	if diff.Superseded == 0 {
		t.Error("finalisation recorded no supersession, so there is nothing to price")
	}
	if diff.Pk < 0 || diff.Pk > 1 || diff.WindowDiff < 0 || diff.WindowDiff > 1 {
		t.Errorf("Pk=%v WindowDiff=%v outside [0,1]", diff.Pk, diff.WindowDiff)
	}
}

// TestFreshnessDiffIsZeroWithoutStreamSpans: a session nothing watched
// live has no freshness to price, and must not report a spurious score.
func TestFreshnessDiffIsZeroWithoutStreamSpans(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	diff, err := s.StreamFreshnessDiff("sess-none")
	if err != nil {
		t.Fatal(err)
	}
	if diff.Pk != 0 || diff.WindowDiff != 0 || diff.StreamSpans != 0 {
		t.Errorf("expected an empty diff, got %+v", diff)
	}
}
