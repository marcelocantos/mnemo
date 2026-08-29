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

// SegmentExpand modes for Search (🎯T64.10).
const (
	SegmentExpandNone   = "none"
	SegmentExpandFine   = "segment"
	SegmentExpandCoarse = "segment:coarse"
)

// DefaultSegmentExpand is off until golden Pk/WindowDiff bar is cleared.
const DefaultSegmentExpand = SegmentExpandNone

// TopicSegment is a persisted topic span.
type TopicSegment struct {
	ID         string  `json:"id"`
	SessionID  string  `json:"session_id"`
	FromMsgID  int     `json:"from_msg_id"`
	ToMsgID    int     `json:"to_msg_id"`
	Level      int     `json:"level"`
	ParentID   string  `json:"parent_id,omitempty"`
	Method     string  `json:"method"`
	Confidence float64 `json:"confidence"`
	Sealed     bool    `json:"sealed"`
	Label      string  `json:"label,omitempty"`
	Summary    string  `json:"summary,omitempty"`
	Repo       string  `json:"repo,omitempty"`
	FirstTS    string  `json:"first_ts,omitempty"`
	LastTS     string  `json:"last_ts,omitempty"`
	ComputedAt string  `json:"computed_at,omitempty"`
	// SupersededBy names the span that later overturned this one, empty
	// when it still stands (🎯T132.1). Distinct from ParentID, which is
	// hierarchy: a parent encloses this span, a superseder replaces it.
	SupersededBy string `json:"superseded_by,omitempty"`
}

// SegmentQuery filters for QuerySegments / mnemo_segments.
type SegmentQuery struct {
	SessionID       string
	ThemeID         string
	ContainingMsgID int
	FTSQuery        string
	OverlapsThemeA  string
	OverlapsThemeB  string
	SealedOnly      bool
	Limit           int
}

// SegmentExpandHit is an enclosing span attached to a search hit.
type SegmentExpandHit struct {
	ID        string `json:"id"`
	FromMsgID int    `json:"from_msg_id"`
	ToMsgID   int    `json:"to_msg_id"`
	Label     string `json:"label,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Level     int    `json:"level"`
	Sealed    bool   `json:"sealed"`
}

// SegmentSession runs Tier-1 structural segmentation for one session
// and persists spans. Unsealed spans are replaced each pass; sealed
// spans are never rewritten.
//
// Structural spans are the provisional layer (🎯T64.11): they give every
// session immediate coverage with zero egress, and are superseded at
// retrieval time by the richer spans a compaction contributes for the
// ranges it has summarised (see SegmentMethodRank). No theme clustering
// happens here or anywhere on the ingest path.
//
// Watermark: if max(messages.id) ≤ segmented_through_id, the sealed
// prefix is complete and there is no new tail — skip work.
func (s *Store) SegmentSession(sessionID string) error {
	if sessionID == "" {
		return nil
	}
	var through int
	_ = s.readDB.QueryRow(
		`SELECT segmented_through_id FROM segment_scan_state WHERE session_id = ?`,
		sessionID,
	).Scan(&through)

	var maxMsgID int
	_ = s.readDB.QueryRow(
		`SELECT COALESCE(MAX(id), 0) FROM messages WHERE session_id = ?`,
		sessionID,
	).Scan(&maxMsgID)
	if through > 0 && maxMsgID > 0 && maxMsgID <= through {
		// No messages past the sealed watermark — nothing to recompute.
		return nil
	}

	rows, err := s.readDB.Query(`
		SELECT id, role, mnemo_text(text, text_z), timestamp, COALESCE(is_noise, 0)
		FROM messages
		WHERE session_id = ?
		ORDER BY id
	`, sessionID)
	if err != nil {
		return fmt.Errorf("segment load messages: %w", err)
	}
	var msgs []segment.Message
	for rows.Next() {
		var m segment.Message
		var noise int
		if err := rows.Scan(&m.ID, &m.Role, &m.Text, &m.Timestamp, &noise); err != nil {
			rows.Close()
			return err
		}
		m.IsNoise = noise != 0
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	spans := segment.Structural(msgs, segment.DefaultConfig())
	if len(spans) == 0 {
		return nil
	}

	var repo string
	_ = s.readDB.QueryRow(
		`SELECT COALESCE(repo, '') FROM session_meta WHERE session_id = ?`,
		sessionID,
	).Scan(&repo)

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.writeDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Drop unsealed rows for this session only — sealed rows stay.
	if _, err := tx.Exec(
		`DELETE FROM topic_segments WHERE session_id = ? AND sealed = 0`,
		sessionID,
	); err != nil {
		return err
	}

	ids := make([]string, len(spans))
	maxSealed := through
	for i, sp := range spans {
		id := segment.SegmentID(sessionID, sp.FromMsgID, sp.ToMsgID, sp.Level, sp.Method)
		ids[i] = id
		if sp.Sealed {
			var existing int
			_ = tx.QueryRow(
				`SELECT sealed FROM topic_segments WHERE id = ?`, id,
			).Scan(&existing)
			if existing == 1 {
				if sp.ToMsgID > maxSealed {
					maxSealed = sp.ToMsgID
				}
				continue
			}
		}
		parentID := sql.NullString{}
		if sp.ParentIdx >= 0 && sp.ParentIdx < len(ids) && ids[sp.ParentIdx] != "" {
			parentID = sql.NullString{String: ids[sp.ParentIdx], Valid: true}
		}
		sealed := 0
		if sp.Sealed {
			sealed = 1
			if sp.ToMsgID > maxSealed {
				maxSealed = sp.ToMsgID
			}
		}
		_, err := tx.Exec(`
			INSERT INTO topic_segments (
				id, session_id, from_msg_id, to_msg_id, level, parent_id,
				method, confidence, sealed, label, summary, repo,
				first_ts, last_ts, computed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				parent_id = excluded.parent_id,
				confidence = excluded.confidence,
				sealed = CASE WHEN topic_segments.sealed = 1 THEN 1 ELSE excluded.sealed END,
				label = CASE WHEN topic_segments.sealed = 1 THEN topic_segments.label ELSE excluded.label END,
				summary = CASE WHEN topic_segments.sealed = 1 THEN topic_segments.summary ELSE excluded.summary END,
				computed_at = excluded.computed_at
		`, id, sessionID, sp.FromMsgID, sp.ToMsgID, sp.Level, parentID,
			sp.Method, sp.Confidence, sealed, sp.Label, sp.Summary, repo,
			sp.FirstTS, sp.LastTS, now)
		if err != nil {
			return fmt.Errorf("insert segment %s: %w", id, err)
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO segment_scan_state (session_id, segmented_through_id, method, scanned_at)
		VALUES (?, ?, 'structural', ?)
		ON CONFLICT(session_id) DO UPDATE SET
			segmented_through_id = excluded.segmented_through_id,
			method = excluded.method,
			scanned_at = excluded.scanned_at
	`, sessionID, maxSealed, now); err != nil {
		return err
	}

	return tx.Commit()
}

// dirtySegmentSessionIDs returns sessions with messages past their seal
// watermark, newest activity first. Matches ingest (mtime DESC) and
// compaction (last_msg DESC) so backfill converges on recent work first.
func (s *Store) dirtySegmentSessionIDs() ([]string, error) {
	rows, err := s.readDB.Query(`
		SELECT ss.session_id
		FROM session_summary ss
		LEFT JOIN segment_scan_state st ON st.session_id = ss.session_id
		WHERE COALESCE(
			(SELECT MAX(m.id) FROM messages m WHERE m.session_id = ss.session_id),
			0
		) > COALESCE(st.segmented_through_id, 0)
		ORDER BY ss.last_msg DESC, ss.session_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// SegmentAllSessions segments sessions that have messages past their
// seal watermark (newest first), then projects any already-summarised
// windows that have no spans yet into span documents.
//
// It deliberately does NOT cluster (🎯T64.11). The previous global
// recluster was single-link agglomerative over every sealed span with a
// full rewrite per pass — super-quadratic in corpus size, and the
// dominant CPU cost of a backfill on a large index. Thematic retrieval
// is served by search over span text instead, so the pass is now linear
// in dirty sessions plus un-projected compactions.
func (s *Store) SegmentAllSessions() error {
	// Project compactions first. It is the cheap pass (one INSERT per
	// un-projected compaction, no LLM call) and the high-value one —
	// those spans carry real summaries, whereas the structural sweep
	// below is O(corpus) and can run for many minutes. Front-loading it
	// means an interrupted or restarted backfill still leaves every
	// summarised window searchable.
	n, err := s.BackfillCompactionSegments(0)
	if err != nil {
		return err
	}
	if n > 0 {
		slog.Info("projected compactions into topic spans", "count", n)
	}

	ids, err := s.dirtySegmentSessionIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.SegmentSession(id); err != nil {
			slog.Warn("segment session failed", "session", id, "err", err)
		}
	}
	return nil
}

// QuerySegments implements mnemo_segments query shapes.
func (s *Store) QuerySegments(q SegmentQuery) ([]TopicSegment, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	// overlaps_theme: segments that are members of both themes.
	if q.OverlapsThemeA != "" && q.OverlapsThemeB != "" {
		rows, err := s.readDB.Query(`
			SELECT s.id, s.session_id, s.from_msg_id, s.to_msg_id, s.level,
			       COALESCE(s.parent_id, ''), s.method, s.confidence, s.sealed,
			       COALESCE(s.label, ''), COALESCE(s.summary, ''), COALESCE(s.repo, ''),
			       COALESCE(s.first_ts, ''), COALESCE(s.last_ts, ''), COALESCE(s.computed_at, ''), COALESCE(s.superseded_by, '')
			FROM topic_segments s
			JOIN theme_members m1 ON m1.entity_id = s.id AND m1.doc_kind = 'segment' AND m1.theme_id = ?
			JOIN theme_members m2 ON m2.entity_id = s.id AND m2.doc_kind = 'segment' AND m2.theme_id = ?
			WHERE (? = 0 OR s.sealed = 1)
			ORDER BY s.first_ts, s.session_id
			LIMIT ?
		`, q.OverlapsThemeA, q.OverlapsThemeB, boolToInt(q.SealedOnly), q.Limit)
		if err != nil {
			return nil, err
		}
		return scanSegments(rows)
	}

	if q.ContainingMsgID > 0 {
		// messages.id is global, so a bare interval test can match spans
		// from OTHER sessions whose id range happens to cover this
		// message. Scope to the containing message's own session: a span
		// only ever describes the session it was computed for. When the
		// caller supplies SessionID we trust it; otherwise resolve it
		// from the message.
		sessionID := q.SessionID
		if sessionID == "" {
			_ = s.readDB.QueryRow(
				`SELECT session_id FROM messages WHERE id = ?`, q.ContainingMsgID,
			).Scan(&sessionID)
		}
		rows, err := s.readDB.Query(`
			SELECT id, session_id, from_msg_id, to_msg_id, level,
			       COALESCE(parent_id, ''), method, confidence, sealed,
			       COALESCE(label, ''), COALESCE(summary, ''), COALESCE(repo, ''),
			       COALESCE(first_ts, ''), COALESCE(last_ts, ''), COALESCE(computed_at, ''), COALESCE(superseded_by, '')
			FROM topic_segments
			WHERE from_msg_id <= ? AND to_msg_id >= ?
			  AND (? = '' OR session_id = ?)
			  AND (? = 0 OR sealed = 1)
			ORDER BY `+SegmentMethodRank+`, level ASC, (to_msg_id - from_msg_id) ASC
			LIMIT ?
		`, q.ContainingMsgID, q.ContainingMsgID, sessionID, sessionID,
			boolToInt(q.SealedOnly), q.Limit)
		if err != nil {
			return nil, err
		}
		return scanSegments(rows)
	}

	if q.ThemeID != "" {
		rows, err := s.readDB.Query(`
			SELECT s.id, s.session_id, s.from_msg_id, s.to_msg_id, s.level,
			       COALESCE(s.parent_id, ''), s.method, s.confidence, s.sealed,
			       COALESCE(s.label, ''), COALESCE(s.summary, ''), COALESCE(s.repo, ''),
			       COALESCE(s.first_ts, ''), COALESCE(s.last_ts, ''), COALESCE(s.computed_at, ''), COALESCE(s.superseded_by, '')
			FROM topic_segments s
			JOIN theme_members m ON m.entity_id = s.id AND m.doc_kind = 'segment' AND m.theme_id = ?
			WHERE (? = 0 OR s.sealed = 1)
			ORDER BY s.first_ts, s.session_id
			LIMIT ?
		`, q.ThemeID, boolToInt(q.SealedOnly), q.Limit)
		if err != nil {
			return nil, err
		}
		return scanSegments(rows)
	}

	if q.FTSQuery != "" {
		fts := relaxQuery(q.FTSQuery)
		rows, err := s.readDB.Query(`
			SELECT s.id, s.session_id, s.from_msg_id, s.to_msg_id, s.level,
			       COALESCE(s.parent_id, ''), s.method, s.confidence, s.sealed,
			       COALESCE(s.label, ''), COALESCE(s.summary, ''), COALESCE(s.repo, ''),
			       COALESCE(s.first_ts, ''), COALESCE(s.last_ts, ''), COALESCE(s.computed_at, ''), COALESCE(s.superseded_by, '')
			FROM topic_segments s
			JOIN topic_segments_fts f ON f.rowid = s.rowid
			WHERE topic_segments_fts MATCH ?
			  AND (? = 0 OR s.sealed = 1)
			ORDER BY rank
			LIMIT ?
		`, fts, boolToInt(q.SealedOnly), q.Limit)
		if err != nil {
			return nil, err
		}
		return scanSegments(rows)
	}

	if q.SessionID != "" {
		rows, err := s.readDB.Query(`
			SELECT id, session_id, from_msg_id, to_msg_id, level,
			       COALESCE(parent_id, ''), method, confidence, sealed,
			       COALESCE(label, ''), COALESCE(summary, ''), COALESCE(repo, ''),
			       COALESCE(first_ts, ''), COALESCE(last_ts, ''), COALESCE(computed_at, ''), COALESCE(superseded_by, '')
			FROM topic_segments
			WHERE session_id = ?
			  AND (? = 0 OR sealed = 1)
			ORDER BY level DESC, from_msg_id
			LIMIT ?
		`, q.SessionID, boolToInt(q.SealedOnly), q.Limit)
		if err != nil {
			return nil, err
		}
		return scanSegments(rows)
	}

	return nil, fmt.Errorf("segment query requires session_id, theme_id, containing_msg_id, query, or overlaps_theme pair")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanSegments(rows *sql.Rows) ([]TopicSegment, error) {
	defer rows.Close()
	var out []TopicSegment
	for rows.Next() {
		var t TopicSegment
		var sealed int
		if err := rows.Scan(
			&t.ID, &t.SessionID, &t.FromMsgID, &t.ToMsgID, &t.Level,
			&t.ParentID, &t.Method, &t.Confidence, &sealed,
			&t.Label, &t.Summary, &t.Repo, &t.FirstTS, &t.LastTS, &t.ComputedAt,
			&t.SupersededBy,
		); err != nil {
			return nil, err
		}
		t.Sealed = sealed != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// AttachSegmentExpand annotates search hits with enclosing sealed segments.
// mode "" or "none" returns results unchanged (byte-identical path when caller
// skips this entirely for none).
func (s *Store) AttachSegmentExpand(results []SearchResult, mode string) ([]SearchResult, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" || mode == SegmentExpandNone {
		return results, nil
	}
	for i := range results {
		if results[i].MessageID <= 0 {
			continue
		}
		segs, err := s.QuerySegments(SegmentQuery{
			ContainingMsgID: results[i].MessageID,
			SessionID:       results[i].SessionID,
			SealedOnly:      true,
			Limit:           20,
		})
		if err != nil {
			return nil, err
		}
		if len(segs) == 0 {
			continue
		}
		// Smallest enclosing = lowest level then narrowest already ordered.
		pick := segs[0]
		if mode == SegmentExpandCoarse {
			// Walk parent_id to root.
			for pick.ParentID != "" {
				parent, err := s.segmentByID(pick.ParentID)
				if err != nil || parent == nil {
					break
				}
				pick = *parent
			}
		}
		results[i].Segment = &SegmentExpandHit{
			ID:        pick.ID,
			FromMsgID: pick.FromMsgID,
			ToMsgID:   pick.ToMsgID,
			Label:     pick.Label,
			Summary:   pick.Summary,
			Level:     pick.Level,
			Sealed:    pick.Sealed,
		}
	}
	return results, nil
}

func (s *Store) segmentByID(id string) (*TopicSegment, error) {
	row := s.readDB.QueryRow(`
		SELECT id, session_id, from_msg_id, to_msg_id, level,
		       COALESCE(parent_id, ''), method, confidence, sealed,
		       COALESCE(label, ''), COALESCE(summary, ''), COALESCE(repo, ''),
		       COALESCE(first_ts, ''), COALESCE(last_ts, ''), COALESCE(computed_at, ''), COALESCE(superseded_by, '')
		FROM topic_segments WHERE id = ?
	`, id)
	var t TopicSegment
	var sealed int
	if err := row.Scan(
		&t.ID, &t.SessionID, &t.FromMsgID, &t.ToMsgID, &t.Level,
		&t.ParentID, &t.Method, &t.Confidence, &sealed,
		&t.Label, &t.Summary, &t.Repo, &t.FirstTS, &t.LastTS, &t.ComputedAt,
		&t.SupersededBy,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	t.Sealed = sealed != 0
	return &t, nil
}

// ThemeAncestors returns parent chain for a theme (dendrogram walk).
func (s *Store) ThemeAncestors(themeID string) ([]string, error) {
	var chain []string
	id := themeID
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		chain = append(chain, id)
		seen[id] = true
		var parent sql.NullString
		err := s.readDB.QueryRow(`SELECT parent_theme_id FROM themes WHERE id = ?`, id).Scan(&parent)
		if err != nil {
			if err == sql.ErrNoRows {
				break
			}
			return chain, err
		}
		if !parent.Valid {
			break
		}
		id = parent.String
	}
	return chain, nil
}
