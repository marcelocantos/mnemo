// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Compaction is a distilled representation of a session's work, produced
// by a summariser LLM. Payload is structured extraction (targets,
// decisions, files, open_threads, summary) serialised as JSON; Summary
// is the prose abstract held separately for quick reads and FTS later.
type Compaction struct {
	ID           int64
	SessionID    string
	ConnectionID string // tag linking back to the live proxy connection that drove this span; empty for pre-T25 rows
	GeneratedAt  time.Time
	Model        string
	PromptTokens int
	OutputTokens int
	CostUSD      float64
	// EntryIDFrom / EntryIDTo bound the compacted span. Despite the
	// "entry_id" name, these hold messages.id values (the autoincrement
	// PK), NOT entries.id: Compactor.Compact records the last compacted
	// message's messages.id, and ReadSessionAfter / the owed-predicate
	// in SelectCompactionCandidates advance and test against messages.id
	// to match (🎯T68.3). The column name is a historical misnomer; do
	// not join these to entries.id.
	EntryIDFrom int64
	EntryIDTo   int64
	PayloadJSON string
	Summary     string
}

// PutCompaction inserts a compaction row and returns the assigned ID.
func (s *Store) PutCompaction(c Compaction) (int64, error) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	generatedAt := c.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}
	payload := c.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	var connID any
	if c.ConnectionID != "" {
		connID = c.ConnectionID
	}
	res, err := s.db.Exec(`
		INSERT INTO compactions
			(session_id, connection_id, generated_at, model, prompt_tokens, output_tokens,
			 cost_usd, entry_id_from, entry_id_to, payload_json, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		c.SessionID, connID, generatedAt.UTC().Format(time.RFC3339Nano), c.Model,
		c.PromptTokens, c.OutputTokens, c.CostUSD,
		c.EntryIDFrom, c.EntryIDTo, payload, c.Summary,
	)
	if err != nil {
		return 0, fmt.Errorf("insert compaction: %w", err)
	}
	return res.LastInsertId()
}

// LatestCompaction returns the most recent compaction for a session, or
// (nil, nil) if none exist.
func (s *Store) LatestCompaction(sessionID string) (*Compaction, error) {
	row := s.db.QueryRow(`
		SELECT id, session_id, COALESCE(connection_id, ''), generated_at, model, prompt_tokens, output_tokens,
		       cost_usd, entry_id_from, entry_id_to, payload_json, summary
		FROM compactions
		WHERE session_id = ?
		ORDER BY generated_at DESC, id DESC
		LIMIT 1
	`, sessionID)
	return scanCompaction(row)
}

// ListCompactions returns compactions for a session in chronological order
// (oldest first), up to limit. A limit of 0 or negative returns all rows.
func (s *Store) ListCompactions(sessionID string, limit int) ([]Compaction, error) {
	q := `
		SELECT id, session_id, COALESCE(connection_id, ''), generated_at, model, prompt_tokens, output_tokens,
		       cost_usd, entry_id_from, entry_id_to, payload_json, summary
		FROM compactions
		WHERE session_id = ?
		ORDER BY generated_at ASC, id ASC`
	args := []any{sessionID}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list compactions: %w", err)
	}
	defer rows.Close()
	return scanCompactions(rows)
}

// ChainCompactions walks the session chain from the oldest /clear-bounded
// predecessor to the given session and returns the latest compaction per
// session along that chain, in chronological order (oldest span first).
// Sessions with no compaction are skipped silently.
func (s *Store) ChainCompactions(sessionID string) ([]Compaction, error) {
	chain, err := s.Chain(sessionID)
	if err != nil {
		return nil, fmt.Errorf("chain lookup: %w", err)
	}
	if len(chain) == 0 {
		// No chain recorded — treat the session as standalone.
		if latest, err := s.LatestCompaction(sessionID); err != nil {
			return nil, err
		} else if latest != nil {
			return []Compaction{*latest}, nil
		}
		return nil, nil
	}
	out := make([]Compaction, 0, len(chain))
	for _, link := range chain {
		latest, err := s.LatestCompaction(link.SessionID)
		if err != nil {
			return nil, err
		}
		if latest != nil {
			out = append(out, *latest)
		}
	}
	return out, nil
}

// scannable is anything with a Scan method matching *sql.Row / *sql.Rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanCompaction(r scannable) (*Compaction, error) {
	var c Compaction
	var generatedAt string
	err := r.Scan(&c.ID, &c.SessionID, &c.ConnectionID, &generatedAt, &c.Model,
		&c.PromptTokens, &c.OutputTokens, &c.CostUSD,
		&c.EntryIDFrom, &c.EntryIDTo, &c.PayloadJSON, &c.Summary)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan compaction: %w", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, generatedAt); err == nil {
		c.GeneratedAt = t
	} else if t, err := time.Parse("2006-01-02 15:04:05", generatedAt); err == nil {
		c.GeneratedAt = t
	}
	return &c, nil
}

func scanCompactions(rows *sql.Rows) ([]Compaction, error) {
	var out []Compaction
	for rows.Next() {
		c, err := scanCompaction(rows)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, *c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate compactions: %w", err)
	}
	return out, nil
}

// CompactionsForConnection returns all compactions tagged to the
// given connection_id, ordered by generated_at ascending (oldest
// first). Used by mnemo_restore to resolve "what did this connection
// compact before the current /clear" via a single definitive query.
func (s *Store) CompactionsForConnection(connectionID string) ([]Compaction, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(`
		SELECT id, session_id, COALESCE(connection_id, ''), generated_at, model, prompt_tokens, output_tokens,
		       cost_usd, entry_id_from, entry_id_to, payload_json, summary
		FROM compactions
		WHERE connection_id = ?
		ORDER BY generated_at ASC, id ASC
	`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("compactions for connection: %w", err)
	}
	defer rows.Close()
	return scanCompactions(rows)
}

// CompactionCandidate is one session the activity-driven watcher
// considers worth a compaction tick (🎯T59). ConnectionID is best-
// effort attribution — the most recently observed connection bound
// to this session, or empty if no binding exists.
type CompactionCandidate struct {
	SessionID    string
	CWD          string
	ConnectionID string
}

// SelectCompactionCandidates returns sessions that are *owed* a
// compaction — the desired-state predicate of the convergence model
// (🎯T68.1, replacing the recency-windowed polling of 🎯T67/🎯T59).
//
// A session is owed when it has new substantive messages since the
// most recent compaction's entry_id_to (or since the start of the
// transcript if none) — measured against the messages table
// directly, not the lifetime substantive_msgs counter — AND EITHER
// of two triggers fires:
//
//  1. delta_substantive_msgs >= minDeltaMsgs (compact an
//     actively-growing session once it has accumulated enough new
//     content), or
//  2. delta_substantive_msgs >= 1 AND last_msg <= idleCutoff (a
//     settled session — including every historical session, which is
//     idle by definition — is owed a span capturing its tail).
//
// There is deliberately NO recency floor in the predicate. Recency
// is a *scheduling priority* (ORDER BY last_msg DESC — recent
// sessions reconcile first), not a filter that permanently abandons
// old sessions. A never-compacted session from months ago is just a
// candidate with a far-back cursor; the watcher drains the backlog
// over successive scans under its per-scan compaction cap, so a large
// initial backlog never starves live sessions. This is what makes
// compaction converge to the fixed point "every owed session has a
// current span" rather than only servicing the last 24h.
//
// Sessions whose cumulative compaction-token cost already meets or
// exceeds maxBudgetRatio of the session's assistant token cost are
// filtered out at the candidate level (not just inside
// Compactor.Compact's checkBudget path): convergence is bounded by
// the token budget, not by recency. This also preserves the 🎯T67
// guarantee that a budget-exhausted session is not re-selected on
// every scan.
//
// Returns the candidate slice (capped at limit, recent-first) plus
// the total backlog — the count of owed sessions before the limit is
// applied — so the gap to the fixed point is observable via
// mnemo_compactor_status rather than silently truncated. ConnectionID
// is best-effort attribution: the most recently observed connection
// bound to the session, or empty when none (additive-only schema,
// NULL-tolerated).
func (s *Store) SelectCompactionCandidates(
	minDeltaMsgs int,
	idleCutoff time.Time,
	maxBudgetRatio float64,
	limit int,
) ([]CompactionCandidate, int, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	if limit <= 0 {
		limit = 100
	}
	if maxBudgetRatio <= 0 {
		maxBudgetRatio = 0.10
	}
	idleCutoffStr := idleCutoff.UTC().Format(time.RFC3339Nano)
	rows, err := s.db.Query(`
		WITH session_state AS (
		  SELECT
		    ss.session_id,
		    ss.last_msg,
		    COALESCE(sm.cwd, '')                                                                    AS cwd,
		    COALESCE((
		      SELECT cs.connection_id FROM connection_sessions cs
		      WHERE cs.session_id = ss.session_id
		      ORDER BY cs.last_seen_at DESC
		      LIMIT 1
		    ), '')                                                                                  AS connection_id,
		    COALESCE((
		      SELECT MAX(entry_id_to) FROM compactions
		      WHERE session_id = ss.session_id
		    ), 0)                                                                                   AS last_entry_id,
		    COALESCE((
		      SELECT SUM(prompt_tokens + output_tokens) FROM compactions
		      WHERE session_id = ss.session_id
		    ), 0)                                                                                   AS comp_tokens,
		    COALESCE((
		      SELECT SUM(input_tokens + output_tokens) FROM entries
		      WHERE session_id = ss.session_id AND type = 'assistant'
		    ), 0)                                                                                   AS sess_tokens
		  FROM session_summary ss
		  LEFT JOIN session_meta sm ON sm.session_id = ss.session_id
		)
		SELECT s.session_id, s.cwd, s.connection_id, COUNT(*) OVER () AS backlog
		FROM session_state s
		WHERE
		  -- Budget filter: skip sessions whose summariser cost has
		  -- already met the configured ratio. Unmeasurable sessions
		  -- (zero session tokens) are allowed through; the first
		  -- compaction has to run before there's anything to measure.
		  (s.sess_tokens = 0 OR s.comp_tokens * 1.0 / s.sess_tokens < ?)
		  AND (
		    -- Trigger A: enough new substantive messages since the
		    -- session's latest compaction (or since the start of the
		    -- transcript if none exists). last_entry_id is a messages.id
		    -- (compactions.entry_id_to is a misnamed messages.id, 🎯T68.3),
		    -- so the cursor comparison is m.id — matching ReadSessionAfter
		    -- and Compact, so owed ⟺ Compact yields a span.
		    (SELECT COUNT(*) FROM messages m
		     WHERE m.session_id = s.session_id
		       AND m.id > s.last_entry_id
		       AND m.is_noise = 0
		    ) >= ?
		    OR
		    -- Trigger B: at least one new substantive message AND
		    -- the session has been idle long enough. Captures small
		    -- one-shot sessions and every historical session (idle by
		    -- definition) — there is no recency floor; old sessions
		    -- are owed, not abandoned.
		    (s.last_msg <= ?
		     AND EXISTS (
		       SELECT 1 FROM messages m
		       WHERE m.session_id = s.session_id
		         AND m.id > s.last_entry_id
		         AND m.is_noise = 0
		     ))
		  )
		ORDER BY s.last_msg DESC
		LIMIT ?
	`, maxBudgetRatio, minDeltaMsgs, idleCutoffStr, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("select compaction candidates: %w", err)
	}
	defer rows.Close()
	var out []CompactionCandidate
	backlog := 0
	for rows.Next() {
		var c CompactionCandidate
		if err := rows.Scan(&c.SessionID, &c.CWD, &c.ConnectionID, &backlog); err != nil {
			return nil, 0, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, backlog, rows.Err()
}

// SessionTokens returns the total input + output tokens consumed by
// assistant messages in a session. Cache tokens are excluded — they
// are not paid tokens in the same sense, and the AC for 🎯T10
// measures summariser cost against "real" session cost.
func (s *Store) SessionTokens(sessionID string) (input int64, output int64, err error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	row := s.db.QueryRow(`
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM entries
		WHERE session_id = ? AND type = 'assistant'
	`, sessionID)
	if err := row.Scan(&input, &output); err != nil {
		return 0, 0, fmt.Errorf("session tokens: %w", err)
	}
	return input, output, nil
}

// CompactionTokens returns the cumulative prompt + output tokens
// consumed by every compaction run for a session.
func (s *Store) CompactionTokens(sessionID string) (prompt int64, output int64, err error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	row := s.db.QueryRow(`
		SELECT COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM compactions
		WHERE session_id = ?
	`, sessionID)
	if err := row.Scan(&prompt, &output); err != nil {
		return 0, 0, fmt.Errorf("compaction tokens: %w", err)
	}
	return prompt, output, nil
}
