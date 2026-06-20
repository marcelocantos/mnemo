// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Note is one cross-session inbox note (🎯T65). A producer session posts
// a note addressed to a consumer session's root directory (the inbox);
// the consumer pulls it. Notes are retained after delivery — read_at is
// set when a recv consumes the note, but the row is never deleted by the
// MVP.
type Note struct {
	ID          int64      `json:"id"`
	Inbox       string     `json:"inbox"`
	Body        string     `json:"body"`
	FromSession string     `json:"from_session,omitempty"`
	FromRepo    string     `json:"from_repo,omitempty"`
	PostedAt    time.Time  `json:"posted_at"`
	ReadAt      *time.Time `json:"read_at,omitempty"`
}

// NotePostParams are the inputs to PostNote.
type NotePostParams struct {
	Inbox string // directory path: absolute, or relative to the caller's session cwd
	Body  string

	// ConnectionID is the Mcp-Session-Id of the calling MCP session. It
	// resolves the caller's current Claude Code session, whose initial
	// cwd anchors a relative Inbox and whose repo defaults FromRepo.
	ConnectionID string

	// FromSession / FromRepo override the connection-derived defaults.
	// Both are optional; an explicit FromSession also anchors a relative
	// Inbox in place of the connection's current session.
	FromSession string
	FromRepo    string
}

// NoteRecvParams are the inputs to RecvNotes.
type NoteRecvParams struct {
	Inbox        string
	ConnectionID string
	UnreadOnly   bool
	MarkRead     bool
	Limit        int
}

// NoteListParams are the inputs to ListNotes. An empty Inbox lists every
// inbox touched within the window.
type NoteListParams struct {
	Inbox        string
	ConnectionID string
	Days         int
}

// canonicalizeInbox resolves a caller-supplied inbox to a single
// canonical absolute directory, identically for post and recv so both
// sides address the same notes row (🎯T65). Rules:
//   - reject any path containing "~" — no shell expansion, no ambiguity;
//   - a relative path joins baseCwd (the calling session's *initial*
//     cwd, NOT the process pwd, which drifts when an agent cd's);
//   - filepath.Clean collapses lexical ./.. ; filepath.EvalSymlinks
//     resolves symlinks and requires the path to exist.
//
// The resolved path must be an existing directory. A non-existent inbox
// is a clear error (the caller never gets a phantom row).
func canonicalizeInbox(inbox, baseCwd string) (string, error) {
	inbox = strings.TrimSpace(inbox)
	if inbox == "" {
		return "", fmt.Errorf("inbox is required")
	}
	if strings.Contains(inbox, "~") {
		return "", fmt.Errorf("inbox path must not contain '~'; pass an absolute path or one relative to the session cwd")
	}
	p := inbox
	if !filepath.IsAbs(p) {
		if baseCwd == "" {
			return "", fmt.Errorf("cannot resolve relative inbox %q: the calling session's cwd is unknown (call mnemo_self first, or pass an absolute path)", inbox)
		}
		p = filepath.Join(baseCwd, p)
	}
	p = filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("inbox directory %q does not exist or is unreadable: %w", p, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inbox %q: %w", resolved, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("inbox %q is not a directory", resolved)
	}
	return resolved, nil
}

// SessionRepo returns the repo recorded for the session in session_meta,
// or "" if not known. Mirror of SessionCWD.
func (s *Store) SessionRepo(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	var repo string
	s.readDB.QueryRow("SELECT COALESCE(repo, '') FROM session_meta WHERE session_id = ? LIMIT 1", sessionID).Scan(&repo)
	return repo
}

// resolveNoteOrigin derives the calling session id and its initial cwd
// for inbox canonicalization and from_* defaulting. An explicit
// fromSession wins; otherwise the connection's current session is used.
// baseCwd is "" when no session identity is available — relative inbox
// paths then fail canonicalization with a clear error.
func (s *Store) resolveNoteOrigin(connectionID, fromSession string) (sid, baseCwd string) {
	sid = fromSession
	if sid == "" && connectionID != "" {
		sid, _ = s.CurrentSessionForConnection(connectionID)
	}
	if sid != "" {
		baseCwd = s.SessionCWD(sid)
	}
	return sid, baseCwd
}

// PostNote canonicalizes the inbox and inserts a note. from_session and
// from_repo default from the calling connection's current session.
func (s *Store) PostNote(p NotePostParams) (*Note, error) {
	if strings.TrimSpace(p.Body) == "" {
		return nil, fmt.Errorf("body is required")
	}
	sid, baseCwd := s.resolveNoteOrigin(p.ConnectionID, p.FromSession)
	inbox, err := canonicalizeInbox(p.Inbox, baseCwd)
	if err != nil {
		return nil, err
	}
	fromRepo := p.FromRepo
	if fromRepo == "" {
		fromRepo = s.SessionRepo(sid)
	}
	now := time.Now().UTC()
	res, err := s.writeDB.Exec(`
		INSERT INTO notes (inbox, body, from_session, from_repo, posted_at)
		VALUES (?, ?, ?, ?, ?)
	`, inbox, p.Body, sid, fromRepo, now.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("insert note: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("insert note id: %w", err)
	}
	return &Note{
		ID:          id,
		Inbox:       inbox,
		Body:        p.Body,
		FromSession: sid,
		FromRepo:    fromRepo,
		PostedAt:    now,
	}, nil
}

// RecvNotes returns the notes addressed to the given inbox, oldest first.
// With UnreadOnly it returns only undelivered notes; with MarkRead it
// stamps the returned notes read. The read mark is idempotent (guarded
// by read_at IS NULL), so concurrent receivers never double-deliver a
// note as unread.
func (s *Store) RecvNotes(p NoteRecvParams) ([]Note, error) {
	_, baseCwd := s.resolveNoteOrigin(p.ConnectionID, "")
	inbox, err := canonicalizeInbox(p.Inbox, baseCwd)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, inbox, body, from_session, from_repo, posted_at, read_at
	      FROM notes WHERE inbox = ?`
	args := []any{inbox}
	if p.UnreadOnly {
		q += ` AND read_at IS NULL`
	}
	q += ` ORDER BY posted_at ASC, id ASC`
	if p.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, p.Limit)
	}
	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	notes, err := scanNotes(rows)
	if err != nil {
		return nil, err
	}
	if !p.MarkRead || len(notes) == 0 {
		return notes, nil
	}
	now := time.Now().UTC()
	nowTS := now.Format(time.RFC3339Nano)
	var unreadIDs []any
	var placeholders []string
	for i := range notes {
		if notes[i].ReadAt == nil {
			unreadIDs = append(unreadIDs, notes[i].ID)
			placeholders = append(placeholders, "?")
		}
	}
	if len(unreadIDs) == 0 {
		return notes, nil
	}
	stmt := `UPDATE notes SET read_at = ? WHERE read_at IS NULL AND id IN (` +
		strings.Join(placeholders, ",") + `)`
	if _, err := s.writeDB.Exec(stmt, append([]any{nowTS}, unreadIDs...)...); err != nil {
		return nil, fmt.Errorf("mark notes read: %w", err)
	}
	for i := range notes {
		if notes[i].ReadAt == nil {
			t := now
			notes[i].ReadAt = &t
		}
	}
	return notes, nil
}

// ListNotes browses notes without consuming them. An empty Inbox lists
// every inbox touched within the window (default 30 days), newest first.
func (s *Store) ListNotes(p NoteListParams) ([]Note, error) {
	days := p.Days
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	q := `SELECT id, inbox, body, from_session, from_repo, posted_at, read_at
	      FROM notes WHERE posted_at >= ?`
	args := []any{cutoff}
	if strings.TrimSpace(p.Inbox) != "" {
		_, baseCwd := s.resolveNoteOrigin(p.ConnectionID, "")
		inbox, err := canonicalizeInbox(p.Inbox, baseCwd)
		if err != nil {
			return nil, err
		}
		q += ` AND inbox = ?`
		args = append(args, inbox)
	}
	q += ` ORDER BY posted_at DESC, id DESC`
	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return scanNotes(rows)
}

func scanNotes(rows *sql.Rows) ([]Note, error) {
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var postedAt string
		var readAt *string
		if err := rows.Scan(&n.ID, &n.Inbox, &n.Body, &n.FromSession, &n.FromRepo, &postedAt, &readAt); err != nil {
			return nil, err
		}
		n.PostedAt, _ = time.Parse(time.RFC3339Nano, postedAt)
		if readAt != nil {
			t, _ := time.Parse(time.RFC3339Nano, *readAt)
			n.ReadAt = &t
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
