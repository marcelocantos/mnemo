// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

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

// TestLiveWatchableSessionsExcludesSummarisers is the recursion guard
// (🎯T132.2), and it exists because v0.72.0 shipped without one.
//
// A summariser is a Claude Code process: it writes its own transcript and
// holds it open, so LiveSessions reports it exactly like a user session.
// A watcher that followed one would spawn another summariser, whose
// session is also live. The concurrency cap bounds that but does not stop
// it — the cap simply fills with summarisers while real sessions starve.
func TestLiveWatchableSessionsExcludesSummarisers(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	workDir := "/tmp/mnemo-streamseg-abc"

	// A real user session.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, cwd, compactor_internal) VALUES ('user-1', '/Users/x/work/repo', 0)`,
	); err != nil {
		t.Fatal(err)
	}
	// A summariser already stamped by ingest — the durable signal.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, cwd, compactor_internal) VALUES ('summ-stamped', ?, 1)`, workDir,
	); err != nil {
		t.Fatal(err)
	}
	// A summariser ingest has seen but NOT yet stamped: this is the race
	// the cwd check exists to close. compactor_internal is still 0.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, cwd, compactor_internal) VALUES ('summ-unstamped', ?, 0)`, workDir,
	); err != nil {
		t.Fatal(err)
	}
	// A summariser from a PREVIOUS daemon run: stamped, but its cwd is an
	// older temp directory this process knows nothing about. claudia's
	// tmux substrate keeps agents alive across a daemon restart, so this
	// is a real state, and it is the case only compactor_internal can
	// catch. Without it the watcher adopts the orphans of its own
	// predecessor.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, cwd, compactor_internal)
		 VALUES ('summ-previous-run', '/tmp/mnemo-streamseg-OLD', 1)`,
	); err != nil {
		t.Fatal(err)
	}

	s.liveMu.Lock()
	s.liveCache = map[string]int{
		"user-1": 100, "summ-stamped": 200, "summ-unstamped": 300, "summ-previous-run": 400}
	s.liveCacheTime = time.Now()
	s.liveMu.Unlock()

	got := s.LiveWatchableSessions(workDir)

	if _, ok := got["user-1"]; !ok {
		t.Error("a real user session was excluded — the watcher would follow nothing")
	}
	if _, ok := got["summ-stamped"]; ok {
		t.Error("a stamped summariser session survived the filter (compactor_internal ignored)")
	}
	if _, ok := got["summ-unstamped"]; ok {
		t.Error("an unstamped summariser survived — the cwd check is what closes the " +
			"window before ingest stamps it, and it is not working")
	}
	if _, ok := got["summ-previous-run"]; ok {
		t.Error("a summariser from a previous daemon run survived — only " +
			"compactor_internal can catch it, since its cwd is a temp dir this " +
			"process never created")
	}
}

// TestLiveWatchableSessionsKeepsUnknownSessions: a session ingest has not
// caught up with yet must still be watchable. Excluding it would mean the
// watcher never follows a session until after it has been indexed, which
// is precisely the freshness the streaming tier exists to provide.
func TestLiveWatchableSessionsKeepsUnknownSessions(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.liveMu.Lock()
	s.liveCache = map[string]int{"brand-new": 111}
	s.liveCacheTime = time.Now()
	s.liveMu.Unlock()

	got := s.LiveWatchableSessions("/tmp/mnemo-streamseg-xyz")
	if _, ok := got["brand-new"]; !ok {
		t.Error("a session with no session_meta row was excluded; the watcher would " +
			"only ever see sessions that are already indexed")
	}
}

// TestLiveWatchableSessionsWithoutWorkDir: an empty working directory must
// not degrade into `cwd LIKE '%'` and exclude every session on the machine.
func TestLiveWatchableSessionsWithoutWorkDir(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, cwd, compactor_internal) VALUES ('user-2', '/Users/x/repo', 0)`,
	); err != nil {
		t.Fatal(err)
	}
	s.liveMu.Lock()
	s.liveCache = map[string]int{"user-2": 42}
	s.liveCacheTime = time.Now()
	s.liveMu.Unlock()

	if got := s.LiveWatchableSessions(""); len(got) != 1 {
		t.Errorf("empty workdir excluded everything: %v", got)
	}
}
