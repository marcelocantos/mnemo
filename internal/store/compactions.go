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
	res, err := s.writeDB.Exec(`
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
	row := s.readDB.QueryRow(`
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
	rows, err := s.readDB.Query(q, args...)
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
	rows, err := s.readDB.Query(`
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
// considers worth a compaction tick. ConnectionID is best-effort
// attribution — the most recently observed connection bound to this
// session, or empty if no binding exists.
type CompactionCandidate struct {
	SessionID    string
	ConnectionID string
}

// SelectCompactionCandidates returns sessions that are *owed* a
// compaction under the token-volume convergence model (🎯T72,
// superseding the message-count + idle triggers of 🎯T68.1/🎯T67).
//
// A session is owed when its *addenda token volume* — the work done
// past the latest compaction's cursor — meets or exceeds budgetTokens.
// The metric is SUM(output_tokens + cache_creation_tokens) over the
// session's assistant entries lying after the cursor:
//
//   - output_tokens is non-overlapping per turn; cache_creation_tokens
//     is the uncached input the model actually had to process. Neither
//     double-counts the way input_tokens does (input_tokens re-counts
//     the whole prior conversation every turn), so their sum is the
//     cleanest measure of content volume.
//   - The cursor is MAX(compactions.entry_id_to), a messages.id (the
//     column is a historical misnomer, 🎯T68.3). It is mapped to the
//     owning entries.id so the sum ranges over entries past the cursor;
//     when no compaction exists the cursor is 0 and the sum covers the
//     whole session — so the *size floor* (a tiny session has nothing
//     dense to compress; its raw entries ARE its retrieval form) and
//     the *re-compaction trigger* are the same measurement applied to
//     different ranges, exactly as the redesign requires.
//
// Two filters precede the volume test:
//
//   - compactor_internal = 0 excludes claudia-spawned summariser
//     sessions, flagged at ingest by the CompactorMarker prefix. This
//     is the precise recursion guard that replaces the over-broad
//     excludeCWD prefix check — a genuine dev session sharing the mnemo
//     repo cwd is now eligible.
//   - the ratio guard skips sessions whose cumulative summariser cost
//     already meets maxBudgetRatio of the session's assistant token
//     cost (a runaway backstop; rarely fires because sess_tokens is the
//     large cumulative input+output sum, not the addenda metric).
//
// There is no recency floor: recency is the ORDER BY priority only
// (recent sessions reconcile first), and the per-scan compaction cap
// drains any historical backlog over successive scans without starving
// live work. Returns the candidate slice (capped at limit, recent-first)
// plus the total backlog — owed count before the limit — so the gap to
// the fixed point "every session above the floor has bounded addenda"
// is observable via mnemo_compactor_status rather than silently
// truncated.
func (s *Store) SelectCompactionCandidates(
	budgetTokens int64,
	maxBudgetRatio float64,
	quarantineThreshold int,
	quarantineSince time.Time,
	limit int,
) ([]CompactionCandidate, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if budgetTokens <= 0 {
		budgetTokens = DefaultAddendaBudgetTokens
	}
	if maxBudgetRatio <= 0 {
		maxBudgetRatio = 0.10
	}
	if quarantineThreshold <= 0 {
		quarantineThreshold = DefaultQuarantineThreshold
	}
	quarantineSinceStr := quarantineSince.UTC().Format(time.RFC3339Nano)
	rows, err := s.readDB.Query(`
		WITH session_state AS (
		  SELECT
		    ss.session_id,
		    ss.last_msg,
		    COALESCE(sm.compactor_internal, 0)                                                      AS compactor_internal,
		    COALESCE((
		      SELECT cs.connection_id FROM connection_sessions cs
		      WHERE cs.session_id = ss.session_id
		      ORDER BY cs.last_seen_at DESC
		      LIMIT 1
		    ), '')                                                                                  AS connection_id,
		    COALESCE((
		      SELECT MAX(entry_id_to) FROM compactions
		      WHERE session_id = ss.session_id
		    ), 0)                                                                                   AS cursor_msg_id,
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
		SELECT s.session_id, s.connection_id, COUNT(*) OVER () AS backlog
		FROM session_state s
		WHERE
		  -- Recursion guard: claudia-spawned summariser sessions are
		  -- excluded by the ingest-stamped flag, not by cwd prefix.
		  s.compactor_internal = 0
		  -- Runaway backstop: skip sessions whose summariser cost has
		  -- already met the configured ratio. Unmeasurable sessions
		  -- (zero session tokens) are allowed through.
		  AND (s.sess_tokens = 0 OR s.comp_tokens * 1.0 / s.sess_tokens < ?)
		  -- Unified floor + re-compaction trigger: owed when the addenda
		  -- token volume past the cursor meets the budget. The cursor
		  -- (a messages.id) is mapped to its entries.id so the sum ranges
		  -- over assistant entries strictly after the compacted span;
		  -- cursor 0 (no compaction) makes this the whole-session volume.
		  AND COALESCE((
		    SELECT SUM(e.output_tokens + e.cache_creation_tokens)
		    FROM entries e
		    WHERE e.session_id = s.session_id
		      AND e.type = 'assistant'
		      AND e.id > COALESCE((
		        SELECT m.entry_id FROM messages m WHERE m.id = s.cursor_msg_id
		      ), 0)
		  ), 0) >= ?
		  -- Durable quarantine (🎯T77): skip sessions that have failed at
		  -- least the threshold number of times and whose last failure is
		  -- still within the cooldown window. The row is deleted on any
		  -- clean tick, and a session past the cooldown gets one parole
		  -- retry, so a transiently-broken session recovers while a
		  -- permanently-broken one stops failing every scan.
		  AND NOT EXISTS (
		    SELECT 1 FROM compactor_quarantine q
		    WHERE q.session_id = s.session_id
		      AND q.fail_count >= ?
		      AND q.last_failed_at > ?
		  )
		ORDER BY s.last_msg DESC
		LIMIT ?
	`, maxBudgetRatio, budgetTokens, quarantineThreshold, quarantineSinceStr, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("select compaction candidates: %w", err)
	}
	defer rows.Close()
	var out []CompactionCandidate
	backlog := 0
	for rows.Next() {
		var c CompactionCandidate
		if err := rows.Scan(&c.SessionID, &c.ConnectionID, &backlog); err != nil {
			return nil, 0, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, backlog, rows.Err()
}

// DefaultAddendaBudgetTokens is the default re-compaction / size-floor
// threshold (🎯T72): a session is owed a compaction once the token
// volume past its latest cursor reaches this many tokens, and a session
// whose whole-session volume is below it is never compacted. Tied to the
// compaction context window; tunable via WatcherConfig.AddendaBudgetTokens.
const DefaultAddendaBudgetTokens int64 = 50_000

// DefaultQuarantineThreshold is how many failures (hard failures plus
// non-payload deferrals) a session may accrue before the candidate
// query excludes it for the cooldown window (🎯T77).
const DefaultQuarantineThreshold = 4

// DefaultQuarantineCooldown is how long a quarantined session stays
// excluded after its last failure before earning one parole retry; if
// it fails again the clock restarts (🎯T77).
const DefaultQuarantineCooldown = 6 * time.Hour

// RecordCompactionFailure bumps a session's durable failure count and
// stamps the time + a short error excerpt, so a persistently-failing
// session is throttled across daemon restarts (🎯T77).
func (s *Store) RecordCompactionFailure(sessionID, errMsg string) error {
	if len(errMsg) > 300 {
		errMsg = errMsg[:300]
	}
	_, err := s.writeDB.Exec(`
		INSERT INTO compactor_quarantine (session_id, fail_count, last_failed_at, last_error)
		VALUES (?, 1, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			fail_count = fail_count + 1,
			last_failed_at = excluded.last_failed_at,
			last_error = excluded.last_error
	`, sessionID, time.Now().UTC().Format(time.RFC3339Nano), errMsg)
	if err != nil {
		return fmt.Errorf("record compaction failure: %w", err)
	}
	return nil
}

// ClearCompactionFailure forgets a session's quarantine after a clean
// tick (a successful compaction or a benign no-op).
func (s *Store) ClearCompactionFailure(sessionID string) error {
	if _, err := s.writeDB.Exec(
		`DELETE FROM compactor_quarantine WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("clear compaction failure: %w", err)
	}
	return nil
}

// QuarantinedCount returns how many sessions are currently quarantined
// — fail_count at/over the threshold with their last failure inside the
// cooldown window (last_failed_at > since).
func (s *Store) QuarantinedCount(threshold int, since time.Time) int {
	if threshold <= 0 {
		threshold = DefaultQuarantineThreshold
	}
	var n int
	_ = s.readDB.QueryRow(`
		SELECT COUNT(*) FROM compactor_quarantine
		WHERE fail_count >= ? AND last_failed_at > ?`,
		threshold, since.UTC().Format(time.RFC3339Nano)).Scan(&n)
	return n
}

// AddendaTokens returns the addenda token volume for a session past a
// cursor — SUM(output_tokens + cache_creation_tokens) over assistant
// entries whose entries.id lies after the entry owning cursorMsgID (a
// messages.id, matching compactions.entry_id_to). A cursorMsgID of 0
// (no compaction) yields the whole-session volume, so the same call
// answers both "is this session above the size floor?" and "how much
// has accrued since the last compaction?" (🎯T72). It is the Go-level
// twin of the predicate inlined in SelectCompactionCandidates.
func (s *Store) AddendaTokens(sessionID string, cursorMsgID int64) (int64, error) {
	var tokens int64
	err := s.readDB.QueryRow(`
		SELECT COALESCE(SUM(e.output_tokens + e.cache_creation_tokens), 0)
		FROM entries e
		WHERE e.session_id = ?
		  AND e.type = 'assistant'
		  AND e.id > COALESCE((SELECT m.entry_id FROM messages m WHERE m.id = ?), 0)
	`, sessionID, cursorMsgID).Scan(&tokens)
	if err != nil {
		return 0, fmt.Errorf("addenda tokens: %w", err)
	}
	return tokens, nil
}

// SessionCompactedView is the retrieval form of a session under the
// 🎯T72 model: the durable compaction summaries plus the live addenda
// tail (substantive messages past the latest cursor, computed on the
// fly from the index — never stored separately). When a session has
// never been compacted, Summaries is empty and Addenda is the whole
// session: the raw entries ARE the retrieval form.
type SessionCompactedView struct {
	SessionID     string
	Summaries     []string         // compaction summaries, oldest span first
	Cursor        int64            // MAX(entry_id_to); 0 when never compacted
	Addenda       []SessionMessage // substantive messages with id > Cursor
	AddendaTokens int64            // token volume of the addenda
}

// CompactedView assembles "the compacted view of session X" (🎯T72):
// the compaction summaries followed by the addenda tail past the latest
// cursor. The tail is computed live from the index, not stored.
// addendaLimit caps the tail (<= 0 → ReadSessionAfter's default).
func (s *Store) CompactedView(sessionID string, addendaLimit int) (*SessionCompactedView, error) {
	resolved, err := s.resolveSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	sessionID = resolved

	comps, err := s.ListCompactions(sessionID, 0)
	if err != nil {
		return nil, err
	}
	view := &SessionCompactedView{SessionID: sessionID}
	for _, c := range comps {
		if c.Summary != "" {
			view.Summaries = append(view.Summaries, c.Summary)
		}
		if c.EntryIDTo > view.Cursor {
			view.Cursor = c.EntryIDTo
		}
	}
	if view.Addenda, err = s.ReadSessionAfter(sessionID, view.Cursor, addendaLimit); err != nil {
		return nil, err
	}
	if view.AddendaTokens, err = s.AddendaTokens(sessionID, view.Cursor); err != nil {
		return nil, err
	}
	return view, nil
}

// SessionTokens returns the total input + output tokens consumed by
// assistant messages in a session. Cache tokens are excluded — they
// are not paid tokens in the same sense, and the AC for 🎯T10
// measures summariser cost against "real" session cost.
func (s *Store) SessionTokens(sessionID string) (input int64, output int64, err error) {
	row := s.readDB.QueryRow(`
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
	row := s.readDB.QueryRow(`
		SELECT COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(output_tokens), 0)
		FROM compactions
		WHERE session_id = ?
	`, sessionID)
	if err := row.Scan(&prompt, &output); err != nil {
		return 0, 0, fmt.Errorf("compaction tokens: %w", err)
	}
	return prompt, output, nil
}
