// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

// FreshnessDiff quantifies how much hindsight moved the boundaries the
// streaming watcher drew live (🎯T132.3).
//
// The measurement is free, and deliberately so. Because a superseded
// span is demoted rather than deleted, both the live view and the
// finalised one are still in the table, so the divergence between them
// can be computed at any time without instrumenting anything. Where
// hindsight routinely redraws a streaming boundary, that gap IS the cost
// of freshness for that session.
//
// Pk and WindowDiff are the standard text-segmentation penalties (lower
// is better, both in [0,1]); they already existed in internal/segment
// with no production caller, having been written for a scoring harness
// that had nothing to score.
type FreshnessDiff struct {
	SessionID   string  `json:"session_id"`
	StreamSpans int     `json:"stream_spans"`
	FinalSpans  int     `json:"final_spans"`
	Superseded  int     `json:"superseded"`
	Pk          float64 `json:"pk"`
	WindowDiff  float64 `json:"window_diff"`
}

// StreamFreshnessDiff compares the stream boundaries against the
// finalised ones for a session. Returns a zero diff when the session has
// no spans of either kind — nothing was live-segmented, so there is no
// freshness to price.
func (s *Store) StreamFreshnessDiff(sessionID string) (FreshnessDiff, error) {
	out := FreshnessDiff{SessionID: sessionID}

	boundaries := func(method string) ([]int, int, error) {
		rows, err := s.readDB.Query(`
			SELECT from_msg_id, to_msg_id FROM topic_segments
			WHERE session_id = ? AND method = ?
			ORDER BY from_msg_id
		`, sessionID, method)
		if err != nil {
			return nil, 0, err
		}
		defer rows.Close()
		var cuts []int
		n := 0
		for rows.Next() {
			var from, to int
			if err := rows.Scan(&from, &to); err != nil {
				return nil, 0, err
			}
			cuts = append(cuts, to)
			n++
		}
		return cuts, n, rows.Err()
	}

	streamCuts, streamN, err := boundaries(SegmentMethodStream)
	if err != nil {
		return out, fmt.Errorf("freshness diff (stream): %w", err)
	}
	finalCuts, finalN, err := boundaries(SegmentMethodLLM)
	if err != nil {
		return out, fmt.Errorf("freshness diff (final): %w", err)
	}
	out.StreamSpans, out.FinalSpans = streamN, finalN

	if err := s.readDB.QueryRow(`
		SELECT COUNT(*) FROM topic_segments
		WHERE session_id = ? AND method = ? AND superseded_by IS NOT NULL
	`, sessionID, SegmentMethodStream).Scan(&out.Superseded); err != nil {
		return out, fmt.Errorf("freshness diff (superseded): %w", err)
	}

	if streamN == 0 || finalN == 0 {
		return out, nil
	}

	// Pk and WindowDiff are defined over a sequence of units and walk
	// every index from 0 to n. Cuts here are messages.id — a GLOBAL
	// rowid, not a position in this session — so using them directly
	// would make n the largest rowid in the database (order 10^6 for a
	// session of a few dozen messages). That is both ruinously slow and
	// meaningless, since nearly every window would land in the empty
	// space between two ids. Map to ordinal position first.
	ord, err := s.substantiveOrdinals(sessionID)
	if err != nil {
		return out, err
	}
	n := len(ord)
	if n == 0 {
		return out, nil
	}
	toOrdinals := func(cuts []int) []int {
		var o []int
		for _, c := range cuts {
			if i, ok := ord[c]; ok {
				o = append(o, i)
			}
		}
		sort.Ints(o)
		return o
	}
	g, h := toOrdinals(finalCuts), toOrdinals(streamCuts)
	if len(g) == 0 || len(h) == 0 {
		return out, nil
	}
	// Window of half the mean final span, the usual Pk convention.
	window := n / (2 * len(g))
	if window < 1 {
		window = 1
	}
	out.Pk = segment.Pk(n, g, h, window)
	out.WindowDiff = segment.WindowDiff(n, g, h, window)
	return out, nil
}

// substantiveOrdinals maps a session's substantive messages.id values to
// their position in the session, which is the index space the
// segmentation scorers are defined over.
func (s *Store) substantiveOrdinals(sessionID string) (map[int]int, error) {
	rows, err := s.readDB.Query(`
		SELECT id FROM messages
		WHERE session_id = ? AND is_noise = 0 ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("substantive ordinals: %w", err)
	}
	defer rows.Close()
	ord := map[int]int{}
	i := 0
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ord[id] = i
		i++
	}
	return ord, rows.Err()
}

// LiveWatchableSessions returns the live sessions a segmentation watcher
// may follow, excluding mnemo's own summariser sessions (🎯T132.2).
//
// Without this the watcher watches the agents it just spawned. A
// summariser is a Claude Code process, so it writes its own transcript
// into ~/.claude/projects and holds it open, which is exactly what
// LiveSessions detects. Watching one spawns another, whose session is
// also live. The concurrency cap bounds the damage but does not prevent
// it: the cap fills with summarisers and real sessions are starved.
//
// Two independent exclusions, because each covers the other's blind spot:
//
//   - compactor_internal, stamped at ingest from the CompactorMarker
//     prefix. Durable and survives a daemon restart — which matters,
//     because claudia's tmux substrate keeps agents alive across one, so
//     a restarted daemon can meet summariser sessions it did not spawn.
//     But it is not immediate: a brand-new summariser has not been
//     ingested yet, leaving a window in which it looks like a user
//     session.
//
//   - the summariser working directory. Every agent this daemon spawns
//     runs in workDir, so its session records that as cwd. This needs no
//     ingest and closes the startup race — but it only knows about the
//     current process's directory, hence the pair.
func (s *Store) LiveWatchableSessions(summariserWorkDir string) map[string]int {
	live := s.LiveSessions()
	if len(live) == 0 {
		return live
	}

	ids := make([]any, 0, len(live))
	placeholders := make([]string, 0, len(live))
	for id := range live {
		ids = append(ids, id)
		placeholders = append(placeholders, "?")
	}

	// One query for both exclusions. A session with no session_meta row
	// yet is NOT excluded here — it is a genuine user session that ingest
	// has not caught up with — which is precisely why the cwd check below
	// cannot be dropped in favour of this.
	q := `SELECT session_id FROM session_meta
	      WHERE session_id IN (` + strings.Join(placeholders, ",") + `)
	        AND (COALESCE(compactor_internal, 0) = 1` + summariserCWDClause(summariserWorkDir) + `)`
	if summariserWorkDir != "" {
		ids = append(ids, summariserWorkDir+"%")
	}

	rows, err := s.readDB.Query(q, ids...)
	if err != nil {
		// Failing closed would stop the watcher entirely on a transient
		// error; failing open would watch summarisers. Neither is good,
		// so refuse to watch anything this tick and try again next one.
		slog.Warn("watchable-session filter failed; skipping this pass", "err", err)
		return map[string]int{}
	}
	defer rows.Close()

	excluded := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		excluded[id] = true
	}

	out := make(map[string]int, len(live))
	for id, pid := range live {
		if !excluded[id] {
			out[id] = pid
		}
	}
	return out
}

// summariserCWDClause adds the working-directory exclusion only when a
// directory is known, so an empty one cannot degrade into `cwd LIKE '%'`
// and exclude every session on the machine.
func summariserCWDClause(workDir string) string {
	if workDir == "" {
		return ""
	}
	return ` OR cwd LIKE ?`
}
