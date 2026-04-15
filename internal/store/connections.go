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
