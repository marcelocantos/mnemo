// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"time"
)

// DaemonConnection records one accepted proxy connection.
type DaemonConnection struct {
	ConnectionID string
	PID          int
	AcceptedAt   time.Time
	LastSeenAt   time.Time
	ClosedAt     *time.Time // nil while the connection is still open
}

// RecordConnectionOpen inserts a daemon_connections row. Idempotent
// via INSERT OR IGNORE (a restarted daemon will mint fresh connection
// IDs so collisions are not expected in normal operation, but the
// bridge contract calls this once per accept and we treat the
// connection_id as authoritative).
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

// RecordConnectionClose marks a connection as closed. Called when the
// proxy disconnects (EOF on the UDS) — Claude Code exit, ctrl-c, etc.
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

// OpenConnections returns the currently-open proxy connections,
// ordered by acceptance time (oldest first).
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
