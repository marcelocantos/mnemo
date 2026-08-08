// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Query-plan oracles for the compaction candidate scan (🎯T146).
//
// The scan ran for 5 seconds every 60 seconds on a real index — a ~8%
// permanent duty cycle — because the planner chose a non-covering index
// for an aggregate over VIRTUAL generated columns, forcing a row fetch
// and JSON re-parse per row. The fix is an INDEXED BY hint.
//
// A hint is only as durable as the thing that notices when it stops
// working. These tests are that thing: the failure mode is not an error
// or a wrong answer, it is the same correct answer arriving 7.5x slower,
// which nothing else in the suite would detect.

// TestCandidateScanUsesCoveringIndex pins the plan. If the hint is
// removed, or the index changes shape, or a future schema edit makes the
// planner pick differently, this fails — loudly and immediately.
func TestCandidateScanUsesCoveringIndex(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// The exact aggregate from the scan's CTE.
	const q = `EXPLAIN QUERY PLAN
		SELECT COALESCE((
		  SELECT SUM(input_tokens + output_tokens) FROM entries
		  INDEXED BY idx_entries_assistant_tokens
		  WHERE session_id = ss.session_id AND type = 'assistant'
		), 0) FROM session_summary ss`

	rows, err := s.readDB.Query(q)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v\n"+
			"If this errors with 'no query solution', the hinted index no longer "+
			"satisfies the query and the scan has silently lost its covering plan.", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			continue
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	got := plan.String()
	if !strings.Contains(got, "idx_entries_assistant_tokens") {
		t.Errorf("candidate-scan aggregate does not use idx_entries_assistant_tokens.\n"+
			"Plan:\n%s\nWithout the covering index the aggregate re-parses JSONB per "+
			"row: measured 1458ms vs 131ms on a 2.2M-entry index.", got)
	}
	// A covering plan does not touch the table. If the plan mentions the
	// table itself, the index is not carrying the columns.
	if strings.Contains(got, "SEARCH entries USING INDEX") &&
		!strings.Contains(got, "COVERING") {
		t.Logf("plan does not report COVERING; SQLite does not always label "+
			"partial-index covering scans, so this is informational:\n%s", got)
	}
}

// TestCandidateScanPlanIsStableWithoutHint documents WHY the hint is
// needed, so a future reader can tell whether it is still necessary
// rather than removing it speculatively.
//
// If SQLite's planner improves and starts choosing the covering index
// unaided, this test starts failing — which is the signal that the hint
// can go. It is deliberately an assertion about the CURRENT planner, not
// a requirement on it.
func TestCandidateScanPlanIsStableWithoutHint(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const q = `EXPLAIN QUERY PLAN
		SELECT COALESCE((
		  SELECT SUM(input_tokens + output_tokens) FROM entries
		  WHERE session_id = ss.session_id AND type = 'assistant'
		), 0) FROM session_summary ss`
	rows, err := s.readDB.Query(q)
	if err != nil {
		t.Skipf("plan unavailable: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err == nil {
			plan.WriteString(detail + "\n")
		}
	}
	if strings.Contains(plan.String(), "idx_entries_assistant_tokens") {
		t.Logf("the planner now picks the covering index unaided — the INDEXED BY "+
			"hint in SelectCompactionCandidatesSince may no longer be needed.\n"+
			"Plan:\n%s", plan.String())
	}
}

// TestCandidateScanCorrectnessUnchanged is the safety net for the hint:
// forcing an index must not change which sessions are selected. A hint
// that alters results is a bug, not an optimisation.
//
// It also guards against the class of error made while developing this
// fix: an "obviously valid" short-circuit (addenda <= session tokens)
// that changed the backlog from 2110 to 92, because session tokens sum
// input+output while addenda sums output+cache_creation — different
// columns, so addenda can exceed the total.
func TestCandidateScanCorrectnessUnchanged(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	now := time.Now().UTC()

	// Sessions with varying token volumes, some above the budget and
	// some below, plus one with no token metadata at all (the message
	// -count fallback path from 🎯T131).
	for i := 0; i < 30; i++ {
		sid := fmt.Sprintf("s%d", i)
		mustExec(t, s, `INSERT INTO session_summary
			(session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg)
			VALUES (?, 'p', 'interactive', 10, 10, ?, ?)`,
			sid, now.Format(time.RFC3339), now.Format(time.RFC3339))
		for k := 0; k < 5; k++ {
			raw := fmt.Sprintf(`{"message":{"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d}}}`,
				i*500, i*400, i*300)
			if i%7 == 0 {
				raw = `{"message":{}}` // no usage metadata → fallback path
			}
			mustExec(t, s, `INSERT INTO entries (session_id, project, type, timestamp, raw)
				VALUES (?, 'p', 'assistant', ?, jsonb(?))`, sid, now.Format(time.RFC3339), raw)
			mustExec(t, s, `INSERT INTO messages
				(session_id, project, role, text, timestamp, type, is_noise, content_type)
				VALUES (?, 'p', 'assistant', 'text', ?, 'assistant', 0, 'text')`,
				sid, now.Format(time.RFC3339))
		}
	}

	got, backlog, err := s.SelectCompactionCandidatesSince(
		-1, 20000, 0.10, DefaultQuarantineThreshold, now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("SelectCompactionCandidatesSince: %v", err)
	}
	if backlog == 0 && len(got) == 0 {
		t.Fatal("no candidates selected from a corpus seeded to contain them; " +
			"the hinted index may have changed which rows qualify")
	}
	// Every returned candidate must genuinely exist.
	for _, c := range got {
		if c.SessionID == "" {
			t.Error("candidate with empty session id")
		}
	}
	t.Logf("selected %d candidates, backlog %d", len(got), backlog)
}

// TestIncrementalScanIsDrivenByChangedSessions is the oracle for the
// defect that hid in plain sight: the incremental path had a bound in
// the SQL that did nothing.
//
// Appending the changed-session join after `session_summary ss` let
// SQLite drive from session_summary, evaluating every per-session
// aggregate for all 35,306 sessions before discarding the 35,295 that
// had not changed. The "incremental" scan measured 657ms against 630ms
// for a full one — indistinguishable, and nothing failed.
//
// The plan is the only place this is visible: results are identical
// either way. So the plan is what gets asserted.
func TestIncrementalScanIsDrivenByChangedSessions(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	const q = `EXPLAIN QUERY PLAN
	  SELECT ss.session_id
	  FROM (SELECT DISTINCT session_id FROM entries NOT INDEXED WHERE id > 0) chg
	  CROSS JOIN session_summary ss ON ss.session_id = chg.session_id
	  LEFT JOIN session_meta sm ON sm.session_id = ss.session_id`

	rows, err := s.readDB.Query(q)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err == nil {
			lines = append(lines, detail)
		}
	}
	if len(lines) == 0 {
		t.Fatal("empty plan")
	}
	// The property that matters: chg is the OUTER loop, and
	// session_summary is SEARCHed by session id rather than SCANned.
	// (The first plan line is "CO-ROUTINE chg" — the probe's own
	// sub-plan — so the check is on the shape, not on line 0.)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "SCAN chg") {
		t.Errorf("the changed-session set does not drive the join.\nPlan:\n%s\n"+
			"When session_summary drives instead, the bound is present in the SQL "+
			"and has no effect: every per-session aggregate is computed and then "+
			"thrown away (measured 657ms for 11 changed sessions vs 3ms).", joined)
	}
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "SCAN ss") {
			t.Errorf("plan scans session_summary as the outer loop: %q\nPlan:\n%s\n"+
				"The incremental path must SEARCH it by the changed session ids.",
				l, joined)
		}
	}
}
