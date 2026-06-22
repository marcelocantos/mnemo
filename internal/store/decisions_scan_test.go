// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

// insertDecisionMsg appends a text message to a session and returns its id.
func insertDecisionMsg(t *testing.T, s *Store, session, role, text string) int64 {
	t.Helper()
	res, err := s.writeDB.Exec(
		`INSERT INTO messages (session_id, project, role, text, timestamp, content_type, is_noise)
		 VALUES (?, 'p', ?, ?, '2026-01-01T00:00:00Z', 'text', 0)`, session, role, text)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func countDecisions(t *testing.T, s *Store, session string) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(
		`SELECT COUNT(*) FROM decisions WHERE session_id = ?`, session).Scan(&n); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	return n
}

func scanWatermark(t *testing.T, s *Store, session string) int64 {
	t.Helper()
	var wm int64
	// Missing row → 0, matching the detector's semantics.
	_ = s.readDB.QueryRow(
		`SELECT scanned_through_id FROM decision_scan_state WHERE session_id = ?`,
		session).Scan(&wm)
	return wm
}

// TestDetectDecisionsWatermarkAdvances verifies 🎯T92: scanning a session
// records a watermark even when no decision pair exists, so the session is
// not rescanned from scratch on the next ingest. This is the core of the
// fix for the every-append full-history rescan.
func TestDetectDecisionsWatermarkAdvances(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const sess = "sess-1"

	// A decision-less session (just chatter).
	insertDecisionMsg(t, s, sess, "user", "hi there how are you")
	last := insertDecisionMsg(t, s, sess, "assistant", "I am well, thanks for asking, here is some chatter text.")

	detectDecisions(s.writeDB, sess, "")

	if got := countDecisions(t, s, sess); got != 0 {
		t.Fatalf("expected 0 decisions in a chatter-only session, got %d", got)
	}
	// The watermark must advance past the last message so the next scan
	// starts from there — the bug was that decision-less sessions never
	// recorded progress and were rescanned forever.
	if wm := scanWatermark(t, s, sess); wm != last {
		t.Fatalf("expected watermark %d after scan, got %d", last, wm)
	}
}

// TestDetectDecisionsIncrementalFindsNewPair verifies a decision pair that
// arrives after an earlier scan is still detected on the next scan — i.e.
// incremental scanning does not miss pairs spanning the resume boundary.
func TestDetectDecisionsIncrementalFindsNewPair(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const sess = "sess-2"

	// First scan: a lone proposal with no confirmation yet.
	insertDecisionMsg(t, s, sess, "assistant",
		"I'll refactor the watcher to gate the scan on an activity watermark, sound good?")
	detectDecisions(s.writeDB, sess, "")
	if got := countDecisions(t, s, sess); got != 0 {
		t.Fatalf("no confirmation yet → expected 0 decisions, got %d", got)
	}

	// The user confirms in a later append. The proposal sits on the resume
	// boundary; the inclusive watermark must let the pair be detected.
	insertDecisionMsg(t, s, sess, "user", "yes, go ahead")
	detectDecisions(s.writeDB, sess, "")
	if got := countDecisions(t, s, sess); got != 1 {
		t.Fatalf("expected the boundary-spanning pair to be detected, got %d decisions", got)
	}

	// A third scan with no new messages must be a cheap no-op (no dup row).
	detectDecisions(s.writeDB, sess, "")
	if got := countDecisions(t, s, sess); got != 1 {
		t.Fatalf("rescan must not duplicate the decision, got %d", got)
	}
}

// TestBackfillDecisionsConverges verifies 🎯T92: a decision-less session is
// backfilled at most once. The old predicate ("no decisions row") rescanned
// such sessions on every pass forever; the watermark makes the backlog drain.
func TestBackfillDecisionsConverges(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const sess = "sess-3"

	// session_meta drives the backfill candidate set.
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo) VALUES (?, '')`, sess); err != nil {
		t.Fatal(err)
	}
	insertDecisionMsg(t, s, sess, "user", "just some plain conversation with no decision in it")

	// First backfill scans the session and records a watermark.
	backfillDecisions(s.writeDB)
	if wm := scanWatermark(t, s, sess); wm == 0 {
		t.Fatalf("first backfill must record a watermark for the scanned session")
	}

	// After the first pass the session has a scan_state row, so it is no
	// longer a backfill candidate — the backlog has converged to empty.
	var remaining int
	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM session_meta sm
		WHERE NOT EXISTS (SELECT 1 FROM decision_scan_state st WHERE st.session_id = sm.session_id)
	`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("backfill must converge: expected 0 unscanned sessions, got %d", remaining)
	}
}

// TestBackfillDecisionsMigrationBulkSeeds verifies the 🎯T92 cold-migration
// path: when decisions already exist (the DB was backfilled by the old
// code), unwatermarked sessions are bulk-seeded with their MAX message id
// in one query rather than re-scanned, and no new full-history scan runs.
func TestBackfillDecisionsMigrationBulkSeeds(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Two unwatermarked sessions in session_meta with message history.
	for _, sess := range []string{"mig-a", "mig-b"} {
		if _, err := s.writeDB.Exec(
			`INSERT INTO session_meta (session_id, repo) VALUES (?, '')`, sess); err != nil {
			t.Fatal(err)
		}
		insertDecisionMsg(t, s, sess, "user", "some historical conversation content here")
		insertDecisionMsg(t, s, sess, "assistant", "more historical content, no actionable decision pair")
	}

	// Simulate "this DB has been backfilled before": a decision already
	// exists. This selects the bulk-seed migration path.
	if _, err := s.writeDB.Exec(`
		INSERT INTO decisions (session_id, proposal_text, confirmation_text, repo, timestamp)
		VALUES ('other', 'p', 'c', '', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	backfillDecisions(s.writeDB)

	// Both sessions must now be watermarked at their own MAX message id.
	for _, sess := range []string{"mig-a", "mig-b"} {
		var wm, maxID int64
		s.readDB.QueryRow(`SELECT scanned_through_id FROM decision_scan_state WHERE session_id=?`, sess).Scan(&wm)
		s.readDB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM messages WHERE session_id=? AND is_noise=0 AND content_type='text'`, sess).Scan(&maxID)
		if wm == 0 || wm != maxID {
			t.Fatalf("session %s: expected watermark == max msg id %d, got %d", sess, maxID, wm)
		}
	}
}
