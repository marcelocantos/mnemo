// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
)

// Segmentation methods recorded on topic_segments.method (🎯T64.11).
//
// Precedence at retrieval time is llm > compaction > structural: a span
// the summariser drew is a better answer to "what was this stretch
// about" than one a compaction window implies, which in turn beats one
// inferred from idle gaps. SegmentMethodRank encodes that order in SQL.
const (
	// SegmentMethodStructural is the always-on local pass: idle gaps and
	// turn cadence, zero egress. It covers sessions that have not been
	// summarised yet, and is the provisional layer for the live tail.
	SegmentMethodStructural = "structural"
	// SegmentMethodCompaction is a window-level span projected from a
	// compaction row. Every compaction is already a message-range-anchored
	// summary, so historical windows become span documents with no LLM
	// call — this is what BackfillCompactionSegments writes.
	SegmentMethodCompaction = "compaction"
	// SegmentMethodLLM is a topic span the summariser drew inside a
	// window it was already reading (Payload.Spans).
	SegmentMethodLLM = "llm"
)

// SegmentMethodRank orders methods by retrieval precedence, lowest
// first. Inlined into ORDER BY clauses so the best available span for a
// message wins without deleting the weaker ones — structural spans stay
// as coverage for ranges no summariser has reached.
const SegmentMethodRank = `CASE method
		WHEN '` + SegmentMethodLLM + `' THEN 0
		WHEN '` + SegmentMethodCompaction + `' THEN 1
		ELSE 2 END`

// Levels for compaction-derived spans. The window span is the coarse
// extent (expand="segment:coarse"); the summariser's topic spans nest
// inside it as the fine extent.
const (
	segmentLevelTopic  = 0
	segmentLevelWindow = 1
)

// CompactionSpan is one topic-coherent stretch inside a compacted
// window, as drawn by the summariser and already validated.
type CompactionSpan struct {
	FromMsgID int64
	ToMsgID   int64
	Label     string
	Summary   string
}

// CompactionSegments is the span index for a single compaction: the
// window itself plus the topic spans found within it.
type CompactionSegments struct {
	SessionID     string
	CompactionID  int64
	FromMsgID     int64
	ToMsgID       int64
	WindowSummary string
	Spans         []CompactionSpan
}

// PutCompactionSegments writes the span index for one compaction
// (🎯T64.11): a window-level span carrying the compaction's own summary,
// and the summariser's topic spans nested beneath it.
//
// Spans are sealed on write. A compacted window lies behind the
// session's compaction cursor and is never re-summarised, so unlike the
// structural tail there is no provisional phase to revisit.
//
// Idempotent: re-running for the same compaction replaces that
// compaction's spans and touches nothing else, so a backfill can be
// re-run and a re-compaction can correct itself.
func (s *Store) PutCompactionSegments(seg CompactionSegments) error {
	if seg.SessionID == "" || seg.CompactionID == 0 {
		return nil
	}
	if seg.FromMsgID > seg.ToMsgID {
		return fmt.Errorf("compaction segments: inverted window %d..%d",
			seg.FromMsgID, seg.ToMsgID)
	}

	var repo string
	_ = s.readDB.QueryRow(
		`SELECT COALESCE(repo, '') FROM session_meta WHERE session_id = ?`,
		seg.SessionID,
	).Scan(&repo)

	firstTS, lastTS := s.msgRangeTimestamps(seg.SessionID, seg.FromMsgID, seg.ToMsgID)

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Replace this compaction's prior spans (re-run / correction) before
	// inserting. Scoped to compaction_id so structural spans and other
	// compactions' spans are untouched.
	if _, err := tx.Exec(
		`DELETE FROM topic_segments WHERE compaction_id = ?`, seg.CompactionID,
	); err != nil {
		return err
	}

	windowLabel := firstLine(seg.WindowSummary)
	windowID := segment.SegmentID(seg.SessionID,
		int(seg.FromMsgID), int(seg.ToMsgID), segmentLevelWindow, SegmentMethodCompaction)
	if err := insertCompactionSegment(tx, compactionSegmentRow{
		ID:        windowID,
		SessionID: seg.SessionID,
		From:      seg.FromMsgID,
		To:        seg.ToMsgID,
		Level:     segmentLevelWindow,
		Method:    SegmentMethodCompaction,
		Label:     windowLabel,
		Summary:   seg.WindowSummary,
		Repo:      repo,
		FirstTS:   firstTS,
		LastTS:    lastTS,
		Now:       now,
		Compaction: sql.NullInt64{
			Int64: seg.CompactionID, Valid: true,
		},
	}); err != nil {
		return err
	}

	for _, sp := range seg.Spans {
		spFirst, spLast := s.msgRangeTimestamps(seg.SessionID, sp.FromMsgID, sp.ToMsgID)
		id := segment.SegmentID(seg.SessionID,
			int(sp.FromMsgID), int(sp.ToMsgID), segmentLevelTopic, SegmentMethodLLM)
		if id == windowID {
			// A single span covering the whole window would collide with
			// the window row; the window row already carries that content.
			continue
		}
		if err := insertCompactionSegment(tx, compactionSegmentRow{
			ID:        id,
			SessionID: seg.SessionID,
			From:      sp.FromMsgID,
			To:        sp.ToMsgID,
			Level:     segmentLevelTopic,
			Parent:    sql.NullString{String: windowID, Valid: true},
			Method:    SegmentMethodLLM,
			Label:     sp.Label,
			Summary:   sp.Summary,
			Repo:      repo,
			FirstTS:   spFirst,
			LastTS:    spLast,
			Now:       now,
			Compaction: sql.NullInt64{
				Int64: seg.CompactionID, Valid: true,
			},
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// compactionSegmentRow is the insert shape for a compaction-derived
// span. Grouped into a struct because the column list is long enough
// that positional arguments stop being readable at the call site.
type compactionSegmentRow struct {
	ID         string
	SessionID  string
	From       int64
	To         int64
	Level      int
	Parent     sql.NullString
	Method     string
	Label      string
	Summary    string
	Repo       string
	FirstTS    string
	LastTS     string
	Now        string
	Compaction sql.NullInt64
}

func insertCompactionSegment(tx *sql.Tx, r compactionSegmentRow) error {
	_, err := tx.Exec(`
		INSERT INTO topic_segments (
			id, session_id, from_msg_id, to_msg_id, level, parent_id,
			method, confidence, sealed, label, summary, repo,
			first_ts, last_ts, computed_at, compaction_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1.0, 1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			parent_id = excluded.parent_id,
			label = excluded.label,
			summary = excluded.summary,
			repo = excluded.repo,
			first_ts = excluded.first_ts,
			last_ts = excluded.last_ts,
			computed_at = excluded.computed_at,
			compaction_id = excluded.compaction_id
	`, r.ID, r.SessionID, r.From, r.To, r.Level, r.Parent,
		r.Method, r.Label, r.Summary, r.Repo, r.FirstTS, r.LastTS, r.Now, r.Compaction)
	if err != nil {
		return fmt.Errorf("insert compaction segment %s: %w", r.ID, err)
	}
	return nil
}

// msgRangeTimestamps resolves the wall-clock bounds of a message range.
// Best-effort: empty strings when the range has no timestamped rows.
func (s *Store) msgRangeTimestamps(sessionID string, from, to int64) (string, string) {
	var first, last sql.NullString
	_ = s.readDB.QueryRow(`
		SELECT MIN(timestamp), MAX(timestamp)
		FROM messages
		WHERE session_id = ? AND id >= ? AND id <= ?
	`, sessionID, from, to).Scan(&first, &last)
	return first.String, last.String
}

// firstLine reduces a summary to a label-sized noun phrase: the first
// sentence, capped. Compactions carry prose summaries but no label, and
// the FTS index ranks label alongside summary.
func firstLine(summary string) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		s = s[:i]
	}
	const maxLabel = 120
	if len(s) > maxLabel {
		s = strings.TrimSpace(s[:maxLabel])
	}
	return s
}

// BackfillCompactionSegments projects already-summarised windows into
// span documents (🎯T64.11).
//
// Spans only start being emitted for windows compacted after this
// feature landed, which would leave the entire historical corpus —
// every session summarised before the change — invisible to span
// search. But a compaction row is already a span document: it has a
// message range, a summary, and a session. Projecting it costs one
// INSERT and no LLM call, so history gets coverage immediately at
// window granularity, and any window later re-summarised gains finer
// topic spans on top.
//
// Compactions whose spans already exist are skipped via the
// compaction_id join, making this cheap to re-run and safe to call on
// every backfill. Newest sessions first, matching ingest and compaction
// ordering so recent work converges first. limit ≤ 0 means no cap.
func (s *Store) BackfillCompactionSegments(limit int) (int, error) {
	// entry_id_from is the PRIOR window's last message (the cursor), and
	// ReadSessionAfter starts strictly past it — so the window's own
	// first message is entry_id_from + 1. Projecting the raw value would
	// make every span overlap its predecessor by one message and
	// mis-anchor containing_msg_id lookups at the boundary.
	q := `
		SELECT c.id, c.session_id, c.entry_id_from + 1, c.entry_id_to, COALESCE(c.summary, '')
		FROM compactions c
		LEFT JOIN topic_segments ts ON ts.compaction_id = c.id
		JOIN session_summary ss ON ss.session_id = c.session_id
		WHERE ts.id IS NULL
		  AND c.entry_id_to > c.entry_id_from
		ORDER BY ss.last_msg DESC, c.id DESC
	`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.readDB.Query(q)
	if err != nil {
		return 0, fmt.Errorf("backfill compaction segments: %w", err)
	}
	var pending []CompactionSegments
	for rows.Next() {
		var seg CompactionSegments
		if err := rows.Scan(&seg.CompactionID, &seg.SessionID,
			&seg.FromMsgID, &seg.ToMsgID, &seg.WindowSummary); err != nil {
			rows.Close()
			return 0, err
		}
		if strings.TrimSpace(seg.WindowSummary) == "" {
			// Nothing to index — a span with no text is not findable.
			continue
		}
		pending = append(pending, seg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, seg := range pending {
		if err := s.PutCompactionSegments(seg); err != nil {
			slog.Warn("backfill compaction segments",
				"session", seg.SessionID, "compaction", seg.CompactionID, "err", err)
			continue
		}
		n++
	}
	return n, nil
}
