// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"testing"
	"time"
)

// A tool answer that grows with the corpus eventually costs more agent
// context than it is worth (🎯T165): a 30-day mnemo_recent_activity call
// returned 1.7 MB of JSON, nearly all of it one topic per session. These
// two tests pin the bounds that stop that, and the disclosure that keeps
// a bounded answer honest.

func TestRecentActivityCapsPerRepoLists(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const sessions = recentActivityListCap + 7
	now := time.Now().UTC()
	for i := 0; i < sessions; i++ {
		id := fmt.Sprintf("sess-%02d", i)
		ts := now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339)
		if _, err := s.writeDB.Exec(`INSERT INTO session_summary
			(session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg)
			VALUES (?, 'proj', 'interactive', 10, 10, ?, ?)`, id, ts, ts); err != nil {
			t.Fatal(err)
		}
		if _, err := s.writeDB.Exec(`INSERT INTO session_meta
			(session_id, repo, cwd, work_type, topic)
			VALUES (?, 'acme/one', '/tmp/one', ?, ?)`,
			id, fmt.Sprintf("work-%02d", i), fmt.Sprintf("topic-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentActivity(7, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d repos, want 1", len(got))
	}
	r := got[0]
	const omitted = sessions - recentActivityListCap
	if len(r.Topics) != recentActivityListCap || r.TopicsOmitted != omitted {
		t.Errorf("topics: %d listed, %d omitted; want %d and %d",
			len(r.Topics), r.TopicsOmitted, recentActivityListCap, omitted)
	}
	if len(r.WorkTypes) != recentActivityListCap || r.WorkTypesOmitted != omitted {
		t.Errorf("work_types: %d listed, %d omitted; want %d and %d",
			len(r.WorkTypes), r.WorkTypesOmitted, recentActivityListCap, omitted)
	}
	// The counts the caps do not touch still describe every session.
	if r.Sessions != sessions {
		t.Errorf("sessions = %d, want %d", r.Sessions, sessions)
	}
}

func TestCapUsageRowsSummarisesTheTail(t *testing.T) {
	const extra = 37
	result := &UsageResult{}
	var wantCost float64
	for i := 0; i < maxUsageRows+extra; i++ {
		row := UsageRow{
			Period:      fmt.Sprintf("sess-%04d", i),
			InputTokens: int64(i),
			Messages:    1,
			CostUSD:     float64(i),
			Source:      "estimated",
		}
		if i >= maxUsageRows {
			wantCost += row.CostUSD
		}
		result.Rows = append(result.Rows, row)
		result.Total.InputTokens += row.InputTokens
		result.Total.CostUSD += row.CostUSD
	}
	totalBefore := result.Total

	capUsageRows(result)

	if len(result.Rows) != maxUsageRows || result.RowsOmitted != extra {
		t.Fatalf("kept %d rows, omitted %d; want %d and %d",
			len(result.Rows), result.RowsOmitted, maxUsageRows, extra)
	}
	if result.Rows[0].Period != "sess-0000" {
		t.Errorf("order changed: first row is %q", result.Rows[0].Period)
	}
	if result.OmittedTotal == nil || result.OmittedTotal.CostUSD != wantCost ||
		result.OmittedTotal.Messages != extra {
		t.Fatalf("omitted total = %+v, want cost %v over %d messages",
			result.OmittedTotal, wantCost, extra)
	}
	// Truncation is a presentation bound, never an accounting one.
	if result.Total != totalBefore {
		t.Errorf("total changed: %+v, want %+v", result.Total, totalBefore)
	}
}

// A result inside the cap carries no disclosure fields at all.
func TestCapUsageRowsLeavesSmallResultsAlone(t *testing.T) {
	result := &UsageResult{Rows: make([]UsageRow, maxUsageRows)}
	capUsageRows(result)
	if len(result.Rows) != maxUsageRows || result.RowsOmitted != 0 || result.OmittedTotal != nil {
		t.Fatalf("capped an uncapped result: %d rows, omitted %d, total %+v",
			len(result.Rows), result.RowsOmitted, result.OmittedTotal)
	}
}
