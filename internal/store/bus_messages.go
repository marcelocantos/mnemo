// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// BusMessage is one row of the bus_messages table — a single message
// posted to a topic on the cross-session bus (🎯T42).
type BusMessage struct {
	ID       int64  `json:"id"`
	Topic    string `json:"topic"`
	PostedAt string `json:"posted_at"`
	PostedBy string `json:"posted_by,omitempty"`
	Body     string `json:"body"`
	ReplyTo  *int64 `json:"reply_to,omitempty"`
	ReadAt   string `json:"read_at,omitempty"`
}

// BusTopic summarises one topic: name, message count, last activity.
type BusTopic struct {
	Topic    string `json:"topic"`
	Messages int    `json:"messages"`
	Unread   int    `json:"unread"`
	LastPost string `json:"last_post,omitempty"`
}

// PostBusMessage inserts a message under topic (resolved if it's a
// session-derived form). postedBy is the optional session_id of the
// caller; empty is fine. replyTo can be nil.
func (s *Store) PostBusMessage(topic, body, postedBy string, replyTo *int64) (int64, error) {
	resolved, err := s.resolveBusTopic(topic)
	if err != nil {
		return 0, err
	}
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	res, err := s.db.Exec(`
		INSERT INTO bus_messages (topic, posted_at, posted_by, body, reply_to)
		VALUES (?, ?, ?, ?, ?)
	`, resolved, time.Now().UTC().Format(time.RFC3339), postedBy, body, replyTo)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecvBusMessages pulls messages from topic. since (RFC3339, may be
// empty) bounds posted_at; an empty since returns all messages.
// markRead sets read_at on each returned row to "now"; existing
// read_at values are preserved (idempotent re-recv shows the same
// rows but the read_at stays at the first-marked time).
//
// limit caps the result set; 0 means default (100).
func (s *Store) RecvBusMessages(topic, since string, markRead bool, limit int) ([]BusMessage, error) {
	resolved, err := s.resolveBusTopic(topic)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	args := []any{resolved}
	where := "topic = ?"
	if since != "" {
		where += " AND posted_at > ?"
		args = append(args, since)
	}
	q := `
		SELECT id, topic, posted_at, posted_by, body, reply_to, read_at
		FROM bus_messages
		WHERE ` + where + `
		ORDER BY posted_at ASC, id ASC
		LIMIT ?
	`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanBusMessages(rows)
	if err != nil {
		return nil, err
	}
	if markRead && len(out) > 0 {
		ids := make([]any, 0, len(out))
		placeholders := make([]string, 0, len(out))
		for _, m := range out {
			ids = append(ids, m.ID)
			placeholders = append(placeholders, "?")
		}
		nowStr := time.Now().UTC().Format(time.RFC3339)
		args := append([]any{nowStr}, ids...)
		if _, err := s.db.Exec(`
			UPDATE bus_messages
			SET read_at = ?
			WHERE id IN (`+strings.Join(placeholders, ",")+`)
			  AND read_at IS NULL
		`, args...); err != nil {
			return out, err
		}
		// Reflect the just-applied read_at on the returned rows so
		// the caller doesn't need a second query.
		for i := range out {
			if out[i].ReadAt == "" {
				out[i].ReadAt = nowStr
			}
		}
	}
	return out, nil
}

// ListBusMessages browses without modifying read_at. topic may be
// empty to list across every topic. unreadOnly filters to read_at
// IS NULL when true.
func (s *Store) ListBusMessages(topic string, unreadOnly bool, limit int) ([]BusMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	var where []string
	var args []any
	if topic != "" {
		resolved, err := s.resolveBusTopicLocked(topic)
		if err != nil {
			return nil, err
		}
		where = append(where, "topic = ?")
		args = append(args, resolved)
	}
	if unreadOnly {
		where = append(where, "read_at IS NULL")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	q := `
		SELECT id, topic, posted_at, posted_by, body, reply_to, read_at
		FROM bus_messages
		` + whereSQL + `
		ORDER BY posted_at DESC, id DESC
		LIMIT ?
	`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBusMessages(rows)
}

// ListBusTopics returns one row per distinct topic with message and
// unread counts plus the most recent posted_at, sorted by recency.
func (s *Store) ListBusTopics() ([]BusTopic, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(`
		SELECT
			topic,
			COUNT(*) AS messages,
			SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END) AS unread,
			MAX(posted_at) AS last_post
		FROM bus_messages
		GROUP BY topic
		ORDER BY last_post DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BusTopic
	for rows.Next() {
		var t BusTopic
		var lastPost sql.NullString
		if err := rows.Scan(&t.Topic, &t.Messages, &t.Unread, &lastPost); err != nil {
			return nil, err
		}
		if lastPost.Valid {
			t.LastPost = lastPost.String
		}
		out = append(out, t)
	}
	return out, nil
}

// resolveBusTopic maps an addressing form onto a canonical topic
// string. Returns the resolved canonical topic. Acquires its own
// read lock — call from outside any other lock.
//
// Forms recognised:
//
//	freeform           — returned verbatim (e.g. "deploy-watch")
//	session:<uuid>     — returned verbatim (already canonical)
//	session:repo=NAME  — resolves to session:<latest-session-uuid-for-NAME>
//	session:latest@DIR — resolves to session:<latest-session-uuid-with-cwd-under-DIR>
//
// Unrecognised session: subforms return an error so a typo
// ("session:rep=mnemo") doesn't silently land in a quiet ghost topic.
func (s *Store) resolveBusTopic(topic string) (string, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	return s.resolveBusTopicLocked(topic)
}

// resolveBusTopicLocked is the lock-held variant for callers that
// already hold s.rwmu.
func (s *Store) resolveBusTopicLocked(topic string) (string, error) {
	if !strings.HasPrefix(topic, "session:") {
		return topic, nil
	}
	rest := strings.TrimPrefix(topic, "session:")
	switch {
	case strings.HasPrefix(rest, "repo="):
		repo := strings.TrimPrefix(rest, "repo=")
		uuid, err := s.latestSessionForRepoLocked(repo)
		if err != nil {
			return "", err
		}
		if uuid == "" {
			return "", fmt.Errorf("bus topic %q: no sessions for repo %q", topic, repo)
		}
		return "session:" + uuid, nil
	case strings.HasPrefix(rest, "latest@"):
		dir := strings.TrimPrefix(rest, "latest@")
		uuid, err := s.latestSessionForCwdLocked(dir)
		if err != nil {
			return "", err
		}
		if uuid == "" {
			return "", fmt.Errorf("bus topic %q: no sessions under cwd %q", topic, dir)
		}
		return "session:" + uuid, nil
	default:
		// Bare session:<uuid> — pass through as the canonical form.
		// (No validation against the sessions table; the topic is
		// addressable even if the session hasn't been seen yet.)
		return topic, nil
	}
}

// latestSessionForRepoLocked returns the most-recently-active
// session_id matching the repo (substring match against
// session_meta.repo OR cwd, like other repo lookups in this package).
// Empty result is not an error — caller decides whether to error.
func (s *Store) latestSessionForRepoLocked(repo string) (string, error) {
	pattern := "%" + repo + "%"
	row := s.db.QueryRow(`
		SELECT sm.session_id
		FROM session_meta sm
		JOIN session_summary ss ON ss.session_id = sm.session_id
		WHERE sm.repo LIKE ? OR sm.cwd LIKE ?
		ORDER BY ss.last_msg DESC
		LIMIT 1
	`, pattern, pattern)
	var uuid string
	if err := row.Scan(&uuid); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return uuid, nil
}

// latestSessionForCwdLocked returns the most-recently-active session
// whose cwd starts with dir (i.e. dir is an ancestor or equal). Used
// for session:latest@/path addressing.
func (s *Store) latestSessionForCwdLocked(dir string) (string, error) {
	pattern := strings.TrimSuffix(dir, "/") + "%"
	row := s.db.QueryRow(`
		SELECT sm.session_id
		FROM session_meta sm
		JOIN session_summary ss ON ss.session_id = sm.session_id
		WHERE sm.cwd LIKE ?
		ORDER BY ss.last_msg DESC
		LIMIT 1
	`, pattern)
	var uuid string
	if err := row.Scan(&uuid); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return uuid, nil
}

func scanBusMessages(rows *sql.Rows) ([]BusMessage, error) {
	var out []BusMessage
	for rows.Next() {
		var m BusMessage
		var postedBy sql.NullString
		var replyTo sql.NullInt64
		var readAt sql.NullString
		if err := rows.Scan(&m.ID, &m.Topic, &m.PostedAt, &postedBy,
			&m.Body, &replyTo, &readAt); err != nil {
			return nil, err
		}
		if postedBy.Valid {
			m.PostedBy = postedBy.String
		}
		if replyTo.Valid {
			id := replyTo.Int64
			m.ReplyTo = &id
		}
		if readAt.Valid {
			m.ReadAt = readAt.String
		}
		out = append(out, m)
	}
	return out, nil
}
