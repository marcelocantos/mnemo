// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Team-mnemo index operations (🎯T36).
//
// These read and write team_sessions / team_entries — the multi-author
// tables a central instance accumulates from contributor pushes. They
// never touch `entries`, `messages` or `session_meta`, and nothing in
// the single-user read path touches the team tables. The separation is
// the access-control boundary the design asks for, expressed as table
// names rather than as a filter that a future caller might forget.

// TeamPushedSession is one session as it arrives from a contributor:
// already filtered and redacted on their machine. The field set
// deliberately mirrors internal/teampush.Session rather than importing
// it — internal/store must not depend on a package that depends on it,
// and the wire format is allowed to evolve separately from storage.
type TeamPushedSession struct {
	SessionID string
	Source    string
	Repo      string
	Project   string
	GitBranch string
	WorkType  string
	Topic     string
	StartedAt string
	EndedAt   string

	// RedactionTally is the contributor's per-rule report, stored as
	// JSON so a reader can see what was removed and why the transcript
	// has gaps.
	RedactionTally map[string]int

	Entries []TeamPushedEntry
}

// TeamPushedEntry is one message within a pushed session.
type TeamPushedEntry struct {
	Index       int
	Role        string
	ContentType string
	Text        string
	Timestamp   string
	ToolName    string
	ToolUseID   string
	IsError     bool
}

// Team push outcome statuses, mirroring the wire protocol's.
const (
	TeamPushAccepted = "accepted"
	TeamPushSkipped  = "skipped"
	TeamPushConflict = "conflict"
)

// TeamIngestResult reports what happened to one session.
type TeamIngestResult struct {
	SessionID string
	Status    string
	Accepted  int
	Reason    string
}

// IngestTeamPush writes one contributor-pushed session into the team
// index and reports what it did.
//
// The three cases come straight from the design's conflict rules:
//
//   - Unknown session → insert, all entries accepted.
//   - Known session, same author → append only entries whose index is
//     not already stored. A contributor who pushes mid-session and
//     again later lands the tail without duplicating the head.
//   - Known session, DIFFERENT author → conflict, nothing written.
//     Session UUIDs are globally unique in a correctly functioning
//     system, so this means either a bug or an attempt to overwrite a
//     colleague's history. Refusing is the containment: an append-only
//     log that can be overwritten by whoever pushes last is not one.
//
// Existing entries are never rewritten, even by their own author. A
// re-push that changed the text of entry 7 leaves entry 7 as first
// pushed; only new indexes land. Immutability is what lets a reader
// trust that what they searched last week says the same thing today.
func (s *Store) IngestTeamPush(author string, sess TeamPushedSession, now time.Time) (TeamIngestResult, error) {
	res := TeamIngestResult{SessionID: sess.SessionID}
	if author == "" {
		return res, fmt.Errorf("team push: author is required")
	}
	if sess.SessionID == "" {
		return res, fmt.Errorf("team push: session_id is required")
	}

	tx, err := s.writeDB.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	var existingAuthor string
	err = tx.QueryRow(`SELECT pushed_author FROM team_sessions WHERE session_id = ?`,
		sess.SessionID).Scan(&existingAuthor)
	switch {
	case err == sql.ErrNoRows:
		// First push of this session.
	case err != nil:
		return res, err
	case existingAuthor != author:
		res.Status = TeamPushConflict
		res.Reason = fmt.Sprintf("session already pushed by %s", existingAuthor)
		return res, nil
	}

	stored := map[int]bool{}
	rows, err := tx.Query(`SELECT entry_index FROM team_entries WHERE session_id = ?`, sess.SessionID)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			rows.Close()
			return res, err
		}
		stored[idx] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	ts := now.UTC().Format(time.RFC3339)
	ins, err := tx.Prepare(`INSERT INTO team_entries
		(session_id, entry_index, pushed_author, repo, role, content_type,
		 text, timestamp, tool_name, tool_use_id, is_error, pushed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return res, err
	}
	defer ins.Close()

	for _, e := range sess.Entries {
		if stored[e.Index] {
			continue
		}
		isErr := 0
		if e.IsError {
			isErr = 1
		}
		if _, err := ins.Exec(sess.SessionID, e.Index, author, sess.Repo, e.Role,
			e.ContentType, e.Text, e.Timestamp, e.ToolName, e.ToolUseID, isErr, ts); err != nil {
			return res, err
		}
		res.Accepted++
	}

	if res.Accepted == 0 && existingAuthor != "" {
		res.Status = TeamPushSkipped
		res.Reason = "already present"
		return res, tx.Commit()
	}

	tallyJSON := ""
	if len(sess.RedactionTally) > 0 {
		b, err := json.Marshal(sess.RedactionTally)
		if err != nil {
			return res, err
		}
		tallyJSON = string(b)
	}

	// The session row's metadata is refreshed on every accepted push
	// (a session that grew has a later ended_at) but first_pushed_at is
	// preserved: it records when the content first became visible to
	// the team, which is the number an auditor asks for.
	if _, err := tx.Exec(`INSERT INTO team_sessions
		(session_id, pushed_author, source, repo, project, git_branch, work_type,
		 topic, started_at, ended_at, entry_count, redaction_tally,
		 first_pushed_at, last_pushed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			(SELECT COUNT(*) FROM team_entries WHERE session_id = ?), ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			source = excluded.source,
			repo = excluded.repo,
			project = excluded.project,
			git_branch = excluded.git_branch,
			work_type = excluded.work_type,
			topic = excluded.topic,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			entry_count = excluded.entry_count,
			redaction_tally = excluded.redaction_tally,
			last_pushed_at = excluded.last_pushed_at`,
		sess.SessionID, author, defaultStr(sess.Source, "claude"), sess.Repo, sess.Project,
		sess.GitBranch, sess.WorkType, sess.Topic, sess.StartedAt, sess.EndedAt,
		sess.SessionID, tallyJSON, ts, ts); err != nil {
		return res, err
	}

	res.Status = TeamPushAccepted
	return res, tx.Commit()
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// RecordTeamPush appends one row to the push audit log.
func (s *Store) RecordTeamPush(author, peerName string, now time.Time,
	sessions, accepted, skipped, conflicts, entries int) error {
	_, err := s.writeDB.Exec(`INSERT INTO team_pushes
		(pushed_author, peer_name, at, sessions, accepted, skipped, conflicts, entries)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		author, peerName, now.UTC().Format(time.RFC3339),
		sessions, accepted, skipped, conflicts, entries)
	return err
}

// CountTeamPushedSessionsSince counts sessions an author landed since
// cutoff. Backs the per-contributor rate limit, which bounds the blast
// radius of a stolen cert: revocation is only as good as the window
// before anyone notices.
func (s *Store) CountTeamPushedSessionsSince(author string, cutoff time.Time) (int, error) {
	var n int
	err := s.readDB.QueryRow(
		`SELECT COALESCE(SUM(sessions), 0) FROM team_pushes WHERE pushed_author = ? AND at >= ?`,
		author, cutoff.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

// TeamSearchHit is one team-index search result. Author is not optional
// and never empty: in a multi-author index, an unattributed result is
// unusable — the reader cannot tell whose claim they are reading.
type TeamSearchHit struct {
	SessionID   string `json:"session_id"`
	Author      string `json:"author"`
	Repo        string `json:"repo"`
	Role        string `json:"role"`
	ContentType string `json:"content_type"`
	Text        string `json:"text"`
	Timestamp   string `json:"timestamp"`
	ToolName    string `json:"tool_name,omitempty"`
	EntryIndex  int    `json:"entry_index"`
}

// TeamSearchParams filters a team-index search.
type TeamSearchParams struct {
	Query  string
	Author string
	Repo   string
	Days   int
	Limit  int
}

// SearchTeam runs FTS over the team index.
func (s *Store) SearchTeam(p TeamSearchParams) ([]TeamSearchHit, error) {
	if p.Limit <= 0 {
		p.Limit = 20
	}
	var q string
	var args []any
	if strings.TrimSpace(p.Query) != "" {
		q = `SELECT t.session_id, t.pushed_author, t.repo, t.role, t.content_type,
				t.text, t.timestamp, t.tool_name, t.entry_index
			FROM team_entries t
			JOIN team_entries_fts f ON f.rowid = t.id
			WHERE team_entries_fts MATCH ?`
		args = append(args, relaxQuery(p.Query))
	} else {
		q = `SELECT session_id, pushed_author, repo, role, content_type,
				text, timestamp, tool_name, entry_index
			FROM team_entries t
			WHERE 1 = 1`
	}
	if p.Author != "" {
		q += ` AND t.pushed_author = ?`
		args = append(args, p.Author)
	}
	if p.Repo != "" {
		q += ` AND t.repo LIKE ?`
		args = append(args, "%"+p.Repo+"%")
	}
	if p.Days > 0 {
		q += fmt.Sprintf(` AND t.timestamp >= datetime('now', '-%d days')`, p.Days)
	}
	if strings.TrimSpace(p.Query) != "" {
		q += ` ORDER BY rank LIMIT ?`
	} else {
		q += ` ORDER BY t.timestamp DESC LIMIT ?`
	}
	args = append(args, p.Limit)

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamSearchHit
	for rows.Next() {
		var h TeamSearchHit
		if err := rows.Scan(&h.SessionID, &h.Author, &h.Repo, &h.Role, &h.ContentType,
			&h.Text, &h.Timestamp, &h.ToolName, &h.EntryIndex); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TeamSessionInfo summarises one pushed session.
type TeamSessionInfo struct {
	SessionID      string         `json:"session_id"`
	Author         string         `json:"author"`
	Source         string         `json:"source"`
	Repo           string         `json:"repo"`
	GitBranch      string         `json:"git_branch,omitempty"`
	WorkType       string         `json:"work_type,omitempty"`
	Topic          string         `json:"topic,omitempty"`
	StartedAt      string         `json:"started_at,omitempty"`
	EndedAt        string         `json:"ended_at,omitempty"`
	EntryCount     int            `json:"entry_count"`
	RedactionTally map[string]int `json:"redaction_tally,omitempty"`
	LastPushedAt   string         `json:"last_pushed_at,omitempty"`
}

// TeamSessionsParams filters a team session listing.
type TeamSessionsParams struct {
	Author string
	Repo   string
	Days   int
	Limit  int
}

// ListTeamSessions lists pushed sessions, most recently active first.
func (s *Store) ListTeamSessions(p TeamSessionsParams) ([]TeamSessionInfo, error) {
	if p.Limit <= 0 {
		p.Limit = 20
	}
	q := `SELECT session_id, pushed_author, source, repo, git_branch, work_type,
			topic, started_at, ended_at, entry_count, redaction_tally, last_pushed_at
		FROM team_sessions WHERE 1 = 1`
	var args []any
	if p.Author != "" {
		q += ` AND pushed_author = ?`
		args = append(args, p.Author)
	}
	if p.Repo != "" {
		q += ` AND repo LIKE ?`
		args = append(args, "%"+p.Repo+"%")
	}
	if p.Days > 0 {
		q += fmt.Sprintf(` AND ended_at >= datetime('now', '-%d days')`, p.Days)
	}
	q += ` ORDER BY ended_at DESC LIMIT ?`
	args = append(args, p.Limit)

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamSessionInfo
	for rows.Next() {
		var si TeamSessionInfo
		var tally string
		if err := rows.Scan(&si.SessionID, &si.Author, &si.Source, &si.Repo, &si.GitBranch,
			&si.WorkType, &si.Topic, &si.StartedAt, &si.EndedAt, &si.EntryCount,
			&tally, &si.LastPushedAt); err != nil {
			return nil, err
		}
		if tally != "" {
			_ = json.Unmarshal([]byte(tally), &si.RedactionTally)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// ReadTeamSession returns a pushed session's entries in transcript
// order, paginated.
func (s *Store) ReadTeamSession(sessionID string, offset, limit int) ([]TeamSearchHit, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.readDB.Query(
		`SELECT session_id, pushed_author, repo, role, content_type,
			text, timestamp, tool_name, entry_index
		FROM team_entries
		WHERE session_id = ?
		ORDER BY entry_index LIMIT ? OFFSET ?`, sessionID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamSearchHit
	for rows.Next() {
		var h TeamSearchHit
		if err := rows.Scan(&h.SessionID, &h.Author, &h.Repo, &h.Role, &h.ContentType,
			&h.Text, &h.Timestamp, &h.ToolName, &h.EntryIndex); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// TeamActivity is per-repo, per-author activity in the team index —
// the "who is working on what" view a newcomer opens first.
type TeamActivity struct {
	Repo         string   `json:"repo"`
	Author       string   `json:"author"`
	Sessions     int      `json:"sessions"`
	Entries      int      `json:"entries"`
	LastActivity string   `json:"last_activity"`
	WorkTypes    []string `json:"work_types,omitempty"`
}

// TeamRecentActivity aggregates the team index by (repo, author).
func (s *Store) TeamRecentActivity(days int, repoFilter, authorFilter string) ([]TeamActivity, error) {
	if days <= 0 {
		days = 30
	}
	q := fmt.Sprintf(`SELECT repo, pushed_author, COUNT(*) AS sessions,
			COALESCE(SUM(entry_count), 0) AS entries,
			MAX(ended_at) AS last_activity,
			GROUP_CONCAT(DISTINCT work_type) AS work_types
		FROM team_sessions
		WHERE ended_at >= datetime('now', '-%d days')`, days)
	var args []any
	if repoFilter != "" {
		q += ` AND repo LIKE ?`
		args = append(args, "%"+repoFilter+"%")
	}
	if authorFilter != "" {
		q += ` AND pushed_author = ?`
		args = append(args, authorFilter)
	}
	q += ` GROUP BY repo, pushed_author ORDER BY last_activity DESC`

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamActivity
	for rows.Next() {
		var a TeamActivity
		var workTypes sql.NullString
		if err := rows.Scan(&a.Repo, &a.Author, &a.Sessions, &a.Entries,
			&a.LastActivity, &workTypes); err != nil {
			return nil, err
		}
		if workTypes.Valid && workTypes.String != "" {
			for _, w := range strings.Split(workTypes.String, ",") {
				if w = strings.TrimSpace(w); w != "" {
					a.WorkTypes = append(a.WorkTypes, w)
				}
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TeamAuthors lists the contributors present in the team index with
// their session counts, for the "who is on this team" question and to
// let an admin see whose content a purge would remove.
func (s *Store) TeamAuthors() ([]TeamActivity, error) {
	rows, err := s.readDB.Query(`SELECT pushed_author, COUNT(*) AS sessions,
			COALESCE(SUM(entry_count), 0) AS entries, MAX(ended_at) AS last_activity
		FROM team_sessions GROUP BY pushed_author ORDER BY sessions DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamActivity
	for rows.Next() {
		var a TeamActivity
		var last sql.NullString
		if err := rows.Scan(&a.Author, &a.Sessions, &a.Entries, &last); err != nil {
			return nil, err
		}
		a.LastActivity = last.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// RetractTeamSession removes a session from the team index. This is the
// design's reactive opt-out, and the ONE deletion path the team tables
// have: entries are otherwise append-only.
//
// requester must match the session's pushed_author unless admin is set.
// A contributor can withdraw what they published; they cannot withdraw
// what a colleague published, which would be censorship rather than
// privacy. Returns the number of entries removed.
func (s *Store) RetractTeamSession(sessionID, requester string, admin bool) (int, error) {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var author string
	err = tx.QueryRow(`SELECT pushed_author FROM team_sessions WHERE session_id = ?`,
		sessionID).Scan(&author)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("session %s is not in the team index", sessionID)
	}
	if err != nil {
		return 0, err
	}
	if !admin && author != requester {
		return 0, fmt.Errorf("session %s was pushed by %s; only its author or an admin can retract it",
			sessionID, author)
	}

	res, err := tx.Exec(`DELETE FROM team_entries WHERE session_id = ?`, sessionID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM team_sessions WHERE session_id = ?`, sessionID); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// PurgeTeamAuthor removes everything one contributor pushed. This is
// the admin containment action from the design: a compromised cert is
// revoked out of band, and this removes what it landed.
func (s *Store) PurgeTeamAuthor(author string) (sessions, entries int, err error) {
	tx, err := s.writeDB.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM team_entries WHERE pushed_author = ?`, author)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	entries = int(n)

	res, err = tx.Exec(`DELETE FROM team_sessions WHERE pushed_author = ?`, author)
	if err != nil {
		return 0, 0, err
	}
	n, _ = res.RowsAffected()
	sessions = int(n)

	return sessions, entries, tx.Commit()
}

// TeamIndexStats is the headline count for a central instance.
type TeamIndexStats struct {
	Sessions int    `json:"sessions"`
	Entries  int    `json:"entries"`
	Authors  int    `json:"authors"`
	Repos    int    `json:"repos"`
	Earliest string `json:"earliest,omitempty"`
	Latest   string `json:"latest,omitempty"`
}

// TeamStats summarises the team index.
func (s *Store) TeamStats() (*TeamIndexStats, error) {
	var st TeamIndexStats
	var earliest, latest sql.NullString
	err := s.readDB.QueryRow(`SELECT
			COUNT(*),
			COALESCE(SUM(entry_count), 0),
			COUNT(DISTINCT pushed_author),
			COUNT(DISTINCT repo),
			MIN(started_at),
			MAX(ended_at)
		FROM team_sessions`).Scan(&st.Sessions, &st.Entries, &st.Authors, &st.Repos,
		&earliest, &latest)
	if err != nil {
		return nil, err
	}
	st.Earliest = earliest.String
	st.Latest = latest.String
	return &st, nil
}

// ---------------------------------------------------------------------
// Contributor side: selecting local sessions for push (🎯T36.2).
//
// These read the LOCAL single-user tables — they are what a contributor
// runs on their own machine to decide what to offer the team index.
// Nothing here transmits; the caller redacts first.

// TeamPushCandidate is one local session eligible for push, with the
// metadata the payload needs.
type TeamPushCandidate struct {
	SessionID string
	Source    string
	Repo      string
	Project   string
	Cwd       string
	GitBranch string
	WorkType  string
	Topic     string
	StartedAt string
	EndedAt   string
	Messages  int
}

// TeamPushFilter selects which local sessions to offer.
type TeamPushFilter struct {
	// SessionID selects exactly one session (accepts a full id).
	SessionID string

	// Repo matches session_meta.repo as a substring.
	Repo string

	// Since is an RFC3339 date or datetime; sessions with no activity
	// at or after it are excluded.
	Since string

	// Limit caps the number of sessions returned. Zero means no cap.
	Limit int
}

// TeamPushCandidates lists local sessions matching the filter, most
// recent first.
//
// Sessions with no repo are excluded here rather than at validation
// time. Push is repo-scoped by design, and a contributor asking to push
// "everything since Monday" should get their repo work, not an error
// about the one ad-hoc session they ran in their home directory.
//
// Compactor-internal sessions are excluded too: they are mnemo's own
// summariser subprocesses, not the contributor's work, and pushing them
// would fill the team index with machine chatter.
func (s *Store) TeamPushCandidates(f TeamPushFilter) ([]TeamPushCandidate, error) {
	q := `SELECT sm.session_id, sm.source, sm.repo, sm.cwd, sm.git_branch,
			sm.work_type, sm.topic,
			COALESCE(MIN(m.timestamp), ''), COALESCE(MAX(m.timestamp), ''),
			COUNT(m.id),
			COALESCE(MAX(m.project), '')
		FROM session_meta sm
		JOIN messages_v m ON m.session_id = sm.session_id
		WHERE sm.repo != '' AND sm.compactor_internal = 0`
	var args []any
	if f.SessionID != "" {
		q += ` AND sm.session_id = ?`
		args = append(args, f.SessionID)
	}
	if f.Repo != "" {
		q += ` AND sm.repo LIKE ?`
		args = append(args, "%"+f.Repo+"%")
	}
	q += ` GROUP BY sm.session_id`
	if f.Since != "" {
		q += ` HAVING MAX(m.timestamp) >= ?`
		args = append(args, f.Since)
	}
	q += ` ORDER BY MAX(m.timestamp) DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamPushCandidate
	for rows.Next() {
		var c TeamPushCandidate
		if err := rows.Scan(&c.SessionID, &c.Source, &c.Repo, &c.Cwd, &c.GitBranch,
			&c.WorkType, &c.Topic, &c.StartedAt, &c.EndedAt, &c.Messages, &c.Project); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TeamPushMessage is one local message as read for push, before
// redaction.
type TeamPushMessage struct {
	Role        string
	ContentType string
	Text        string
	Timestamp   string
	ToolName    string
	ToolUseID   string
	IsError     bool
}

// TeamPushMessages reads one local session's messages in transcript
// order, excluding the content kinds that are never pushed.
//
// The exclusion happens in SQL, not in the caller. A filter applied
// after the read is one a future refactor can drop while every test
// still passes; a WHERE clause that never selects thinking blocks
// cannot be bypassed by forgetting to call something.
func (s *Store) TeamPushMessages(sessionID string) ([]TeamPushMessage, error) {
	rows, err := s.readDB.Query(`SELECT role, content_type, mnemo_text(text, text_z) AS text,
			COALESCE(timestamp, ''), COALESCE(tool_name, ''), COALESCE(tool_use_id, ''), is_error
		FROM messages
		WHERE session_id = ?
		  AND content_type NOT IN ('thinking', 'image')
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TeamPushMessage
	for rows.Next() {
		var m TeamPushMessage
		var isErr int
		if err := rows.Scan(&m.Role, &m.ContentType, &m.Text, &m.Timestamp,
			&m.ToolName, &m.ToolUseID, &isErr); err != nil {
			return nil, err
		}
		m.IsError = isErr != 0
		out = append(out, m)
	}
	return out, rows.Err()
}
