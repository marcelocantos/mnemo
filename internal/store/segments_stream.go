// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
)

// Persisting spans drawn live by the streaming watcher (🎯T132.2).
//
// These rows differ from the batch ones in exactly two ways: method is
// 'stream', and they are provisional — the batch pass at session close
// redraws the same region with hindsight and may supersede them. They are
// written sealed because the automaton only hands over spans it has
// sealed; an open span is working state, not a claim.

// StreamSpan is one sealed span from the streaming watcher.
type StreamSpan struct {
	SessionID string
	FromMsgID int
	ToMsgID   int
	Label     string
	Summary   string
}

// PutStreamSpans upserts sealed stream spans for a session.
//
// Idempotent by construction: the id is derived from the span's own
// extent (SegmentID over session/from/to/method/level), so replaying an
// event stream after a crash converges on the same rows rather than
// duplicating them. That is what lets recovery be "replay from the last
// sealed state" instead of a bookkeeping reconstruction.
func (s *Store) PutStreamSpans(spans []StreamSpan) error {
	if len(spans) == 0 {
		return nil
	}
	tx, err := s.writeDB.Begin()
	if err != nil {
		return fmt.Errorf("stream spans begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, sp := range spans {
		id := segment.SegmentID(sp.SessionID, sp.FromMsgID, sp.ToMsgID, segmentLevelTopic, SegmentMethodStream)
		if _, err := tx.Exec(`
			INSERT INTO topic_segments (
				id, session_id, from_msg_id, to_msg_id, level, parent_id,
				method, confidence, sealed, label, summary, repo,
				first_ts, last_ts, computed_at
			) VALUES (?, ?, ?, ?, ?, NULL, ?, 0.7, 1, ?, ?, '', '', '', ?)
			ON CONFLICT(id) DO UPDATE SET
				label = excluded.label,
				summary = excluded.summary,
				computed_at = excluded.computed_at
		`, id, sp.SessionID, sp.FromMsgID, sp.ToMsgID, segmentLevelTopic,
			SegmentMethodStream, sp.Label, sp.Summary, now); err != nil {
			return fmt.Errorf("stream span upsert: %w", err)
		}
	}
	return tx.Commit()
}

// StreamSealedThrough returns the highest message id covered by a sealed
// stream span for a session, or 0 when there are none.
//
// This is the recovery cursor. A watcher that died mid-session restarts
// from here rather than from the beginning: everything below is already
// durable, and re-deriving it would pay the summariser twice for the same
// stretch of conversation.
func (s *Store) StreamSealedThrough(sessionID string) (int, error) {
	var through sql.NullInt64
	err := s.readDB.QueryRow(`
		SELECT MAX(to_msg_id) FROM topic_segments
		WHERE session_id = ? AND method = ? AND sealed = 1
	`, sessionID, SegmentMethodStream).Scan(&through)
	if err != nil {
		return 0, fmt.Errorf("stream sealed watermark: %w", err)
	}
	if !through.Valid {
		return 0, nil
	}
	return int(through.Int64), nil
}

// StreamSpanIDAt resolves a span by its extent, so a supersede event
// naming an earlier span can be attached to the row that holds it.
func (s *Store) StreamSpanIDAt(sessionID string, from, to int) (string, error) {
	var id string
	err := s.readDB.QueryRow(`
		SELECT id FROM topic_segments
		WHERE session_id = ? AND from_msg_id = ? AND to_msg_id = ? AND method = ?
	`, sessionID, from, to, SegmentMethodStream).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stream span lookup: %w", err)
	}
	return id, nil
}

// MarkSuperseded records that one span overturned another (🎯T132.1).
//
// Never a delete. A superseded span keeps its row and its content and
// merely sorts below live ones, because the divergence between what the
// stream believed and what hindsight concluded is the freshness metric —
// removing the loser would remove the measurement.
//
// Self-supersession is refused rather than ignored: it would make a span
// permanently outrank nothing, including itself, and the cycle is far
// easier to reject here than to diagnose later in a ranking query.
func (s *Store) MarkSuperseded(spanID, bySpanID string) error {
	if spanID == "" || bySpanID == "" {
		return nil
	}
	if spanID == bySpanID {
		return fmt.Errorf("span %s cannot supersede itself", spanID)
	}
	_, err := s.writeDB.Exec(
		`UPDATE topic_segments SET superseded_by = ? WHERE id = ?`, bySpanID, spanID)
	if err != nil {
		return fmt.Errorf("mark superseded: %w", err)
	}
	return nil
}

// SubstantiveMessagesSince loads a session's substantive messages past a
// cursor — the drip the streaming watcher feeds to the summariser.
//
// Noise is excluded here rather than in the watcher so the index does the
// filtering (idx_messages_session_id_substantive), and so the watcher
// never sees, counts, or pays for a "Tool loaded." line.
func (s *Store) SubstantiveMessagesSince(sessionID string, afterMsgID, limit int) ([]StreamMessage, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.readDB.Query(`
		SELECT id, role, text, timestamp
		FROM messages
		WHERE session_id = ? AND id > ? AND is_noise = 0
		ORDER BY id
		LIMIT ?
	`, sessionID, afterMsgID, limit)
	if err != nil {
		return nil, fmt.Errorf("stream drip: %w", err)
	}
	defer rows.Close()
	var out []StreamMessage
	for rows.Next() {
		var m StreamMessage
		if err := rows.Scan(&m.ID, &m.Role, &m.Text, &m.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// StreamMessage is one substantive message in a drip.
type StreamMessage struct {
	ID        int
	Role      string
	Text      string
	Timestamp string
}
