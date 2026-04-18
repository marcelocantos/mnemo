// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"log/slog"
	"strings"
	"time"
)

// DaemonConnection records one MCP session observed by the daemon.
// The connection_id field holds the Mcp-Session-Id value — a stable
// identifier minted by the streamable-HTTP transport for the
// duration of the client's MCP session.
type DaemonConnection struct {
	ConnectionID string
	PID          int
	AcceptedAt   time.Time
	LastSeenAt   time.Time
	ClosedAt     *time.Time // nil while the connection is still open
}

// RecordConnectionOpen inserts a daemon_connections row. Idempotent
// via INSERT OR IGNORE. Called lazily on the first tool call for a
// given MCP session ID.
func (s *Store) RecordConnectionOpen(connectionID string, pid int, acceptedAt time.Time) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	ts := acceptedAt.UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO daemon_connections
			(connection_id, pid, accepted_at, last_seen_at)
		VALUES (?, ?, ?, ?)
	`, connectionID, pid, ts, ts); err != nil {
		slog.Warn("connection open insert failed", "conn", connectionID, "err", err)
	}
}

// RecordConnectionClose marks a connection as closed. The HTTP MCP
// transport does not expose a reliable disconnect signal, so this is
// currently only called when the daemon shuts down or when a future
// idle-timeout sweeper is added. Existing callers (tests) still work.
func (s *Store) RecordConnectionClose(connectionID string, closedAt time.Time) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	ts := closedAt.UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`
		UPDATE daemon_connections
		SET closed_at = ?, last_seen_at = ?
		WHERE connection_id = ? AND closed_at IS NULL
	`, ts, ts, connectionID); err != nil {
		slog.Warn("connection close update failed", "conn", connectionID, "err", err)
	}
}

// RecordConnectionSession upserts the (connection_id, session_id)
// binding, bumping last_seen_at on repeat observations. Called from
// tool-call paths that resolve a session identity for the current
// connection (notably mnemo_self). Idempotent. Uses time.Now()
// internally; tests that need deterministic times should use
// RecordConnectionSessionAt.
func (s *Store) RecordConnectionSession(connectionID, sessionID string) {
	s.RecordConnectionSessionAt(connectionID, sessionID, time.Now())
}

// RecordConnectionSessionAt is the time-injected variant used by tests.
func (s *Store) RecordConnectionSessionAt(connectionID, sessionID string, at time.Time) {
	if connectionID == "" || sessionID == "" {
		return
	}
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	ts := at.UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`
		INSERT INTO connection_sessions
			(connection_id, session_id, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(connection_id, session_id) DO UPDATE
			SET last_seen_at = excluded.last_seen_at
	`, connectionID, sessionID, ts, ts); err != nil {
		slog.Warn("connection session upsert failed",
			"conn", connectionID, "session", sessionID, "err", err)
		return
	}

	// Was a /clear boundary observed? If this connection had an
	// earlier session distinct from the new one, the most recent such
	// session is the definitive predecessor. Insert a session_chains
	// row with mechanism='mcp_connection' and confidence='definitive'.
	// INSERT OR IGNORE makes this idempotent across repeat observations
	// of the same binding.
	var predecessorID, predLastSeen string
	err := s.db.QueryRow(`
		SELECT session_id, last_seen_at FROM connection_sessions
		WHERE connection_id = ?
		  AND session_id != ?
		  AND first_seen_at < ?
		ORDER BY first_seen_at DESC
		LIMIT 1
	`, connectionID, sessionID, ts).Scan(&predecessorID, &predLastSeen)
	if err == sql.ErrNoRows {
		return // no predecessor — this is the first session on this connection
	}
	if err != nil {
		slog.Warn("chain lookup failed", "conn", connectionID, "err", err)
		return
	}

	var gapMs int64
	if t, perr := time.Parse(time.RFC3339Nano, predLastSeen); perr == nil {
		gapMs = at.UTC().Sub(t).Milliseconds()
	}
	if _, err := s.db.Exec(`
		INSERT OR IGNORE INTO session_chains
			(successor_id, predecessor_id, boundary, gap_ms, confidence, mechanism)
		VALUES (?, ?, 'clear', ?, 'definitive', 'mcp_connection')
	`, sessionID, predecessorID, gapMs); err != nil {
		slog.Warn("chain insert failed", "successor", sessionID, "err", err)
	}
}

// ConnectionSession is a single (connection_id, session_id) binding.
type ConnectionSession struct {
	ConnectionID string
	SessionID    string
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
}

// SessionsForConnection returns every session the connection has
// observed, ordered by first_seen_at ascending (oldest first).
func (s *Store) SessionsForConnection(connectionID string) ([]ConnectionSession, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(`
		SELECT connection_id, session_id, first_seen_at, last_seen_at
		FROM connection_sessions
		WHERE connection_id = ?
		ORDER BY first_seen_at ASC
	`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConnectionSessions(rows)
}

// ChainCandidate is one possible predecessor surfaced by
// InferChainHeuristic. Unlike session_chains rows (which are
// definitive observations), candidates are query-time inferences
// with a confidence tier computed from cwd match and temporal
// proximity.
type ChainCandidate struct {
	PredecessorID string
	GapMs         int64
	Confidence    string // "high" (gap < 30s) or "medium"
	Mechanism     string // always "cwd_most_recent" for this rule
}

// InferChainHeuristic computes predecessor candidates for a session
// that has no definitive connection-observed chain link. Uses the
// cwd-most-recent rule: look for the most recent sessions in the
// same cwd whose last message preceded this session's first message,
// limited to sessions where the successor's first user message
// contains the /clear command marker.
//
// Pure function — no DB writes, no cached state. Safe to call
// repeatedly; every call re-queries the underlying session_summary
// and session_meta.
//
// limit caps the number of candidates returned. Zero or negative
// defaults to 1 (the single best guess).
func (s *Store) InferChainHeuristic(sessionID string, limit int) ([]ChainCandidate, error) {
	if limit <= 0 {
		limit = 1
	}
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	// First: does this session's first user message actually look
	// like a /clear rollover? Non-rollover sessions have no inferred
	// predecessor by design.
	var firstText, firstTS string
	err := s.db.QueryRow(`
		SELECT m.text, m.timestamp
		FROM messages m
		WHERE m.session_id = ? AND m.role = 'user'
		ORDER BY m.id ASC LIMIT 1
	`, sessionID).Scan(&firstText, &firstTS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hasClearMarker(firstText) {
		return nil, nil
	}

	// Scope to same cwd.
	var cwd string
	_ = s.db.QueryRow(`SELECT cwd FROM session_meta WHERE session_id = ?`, sessionID).Scan(&cwd)
	if cwd == "" {
		return nil, nil
	}

	// Find most recent same-cwd sessions that ended before this one
	// started. Ordered newest-first; the caller gets up to limit.
	rows, err := s.db.Query(`
		SELECT ss.session_id, ss.last_msg
		FROM session_summary ss
		JOIN session_meta sm ON sm.session_id = ss.session_id
		WHERE sm.cwd = ?
		  AND ss.session_id != ?
		  AND ss.last_msg <= ?
		ORDER BY ss.last_msg DESC
		LIMIT ?
	`, cwd, sessionID, firstTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	succTime, _ := time.Parse(time.RFC3339, firstTS)
	var out []ChainCandidate
	for rows.Next() {
		var predID, predLast string
		if err := rows.Scan(&predID, &predLast); err != nil {
			continue
		}
		gapMs := int64(-1)
		if t, err := time.Parse(time.RFC3339, predLast); err == nil {
			gapMs = succTime.Sub(t).Milliseconds()
		}
		conf := "medium"
		if gapMs >= 0 && gapMs < 30_000 {
			conf = "high"
		}
		out = append(out, ChainCandidate{
			PredecessorID: predID,
			GapMs:         gapMs,
			Confidence:    conf,
			Mechanism:     "cwd_most_recent",
		})
	}
	return out, rows.Err()
}

// hasClearMarker reports whether the message text contains the
// Claude Code slash-command envelope for /clear. Used by
// InferChainHeuristic to identify genuine rollover sessions.
func hasClearMarker(text string) bool {
	return strings.Contains(text, "<command-name>/clear</command-name>")
}

// CurrentSessionForConnection returns the session_id most recently
// observed on the given connection (max last_seen_at). Returns an
// empty string if the connection has no recorded sessions yet (the
// client has opened an MCP session but not yet called a
// session-resolving tool like mnemo_self).
func (s *Store) CurrentSessionForConnection(connectionID string) (string, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	var sid string
	err := s.db.QueryRow(`
		SELECT session_id FROM connection_sessions
		WHERE connection_id = ?
		ORDER BY last_seen_at DESC, first_seen_at DESC
		LIMIT 1
	`, connectionID).Scan(&sid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sid, err
}

// ConnectionsForSession returns every connection that has ever
// observed the given session. Usually one, but ctrl-c + `claude
// --continue` produces two rows (pre- and post-restart).
func (s *Store) ConnectionsForSession(sessionID string) ([]ConnectionSession, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(`
		SELECT connection_id, session_id, first_seen_at, last_seen_at
		FROM connection_sessions
		WHERE session_id = ?
		ORDER BY first_seen_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConnectionSessions(rows)
}

func scanConnectionSessions(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]ConnectionSession, error) {
	var out []ConnectionSession
	for rows.Next() {
		var cs ConnectionSession
		var first, last string
		if err := rows.Scan(&cs.ConnectionID, &cs.SessionID, &first, &last); err != nil {
			return nil, err
		}
		cs.FirstSeenAt, _ = time.Parse(time.RFC3339Nano, first)
		cs.LastSeenAt, _ = time.Parse(time.RFC3339Nano, last)
		out = append(out, cs)
	}
	return out, rows.Err()
}

// OpenConnections returns the MCP sessions the daemon currently
// treats as live, ordered by acceptance time (oldest first).
func (s *Store) OpenConnections() ([]DaemonConnection, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(`
		SELECT connection_id, pid, accepted_at, last_seen_at, closed_at
		FROM daemon_connections
		WHERE closed_at IS NULL
		ORDER BY accepted_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DaemonConnection
	for rows.Next() {
		var c DaemonConnection
		var acceptedAt, lastSeenAt string
		var closedAt *string
		if err := rows.Scan(&c.ConnectionID, &c.PID, &acceptedAt, &lastSeenAt, &closedAt); err != nil {
			return nil, err
		}
		c.AcceptedAt, _ = time.Parse(time.RFC3339Nano, acceptedAt)
		c.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeenAt)
		if closedAt != nil {
			t, _ := time.Parse(time.RFC3339Nano, *closedAt)
			c.ClosedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
