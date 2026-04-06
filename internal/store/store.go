// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package store provides a searchable transcript index across all
// Claude Code sessions. It ingests JSONL files from ~/.claude/projects/
// and maintains a realtime FTS5 index in SQLite.
package store

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
)

// Store is a searchable index of Claude Code transcripts.
type Store struct {
	db         *sql.DB
	projectDir string

	mu      sync.Mutex
	offsets map[string]int64 // file path → last read offset

	rwmu sync.RWMutex // protects db access: writers (ingest), readers (queries)
}

// SearchResult is a single search hit.
type SearchResult struct {
	SessionID string  `json:"session_id"`
	Project   string  `json:"project"`
	Role      string  `json:"role"`
	Text      string  `json:"text"`
	Timestamp string  `json:"timestamp"`
	Rank      float64 `json:"rank"`
}

// SessionInfo is a summary of a transcript session.
type SessionInfo struct {
	SessionID       string `json:"session_id"`
	Project         string `json:"project"`
	SessionType     string `json:"session_type"`
	Repo            string `json:"repo,omitempty"`
	GitBranch       string `json:"git_branch,omitempty"`
	WorkType        string `json:"work_type,omitempty"`
	Topic           string `json:"topic,omitempty"`
	TotalMsgs       int    `json:"total_msgs"`
	SubstantiveMsgs int    `json:"substantive_msgs"`
	FirstMsg        string `json:"first_msg"`
	LastMsg         string `json:"last_msg"`
}

// TypeStats holds per-session-type statistics.
type TypeStats struct {
	SessionType     string `json:"session_type"`
	Sessions        int    `json:"sessions"`
	TotalMsgs       int    `json:"total_msgs"`
	SubstantiveMsgs int    `json:"substantive_msgs"`
	NoiseMsgs       int    `json:"noise_msgs"`
}

// StatsResult holds full memory statistics.
type StatsResult struct {
	TotalSessions int         `json:"total_sessions"`
	TotalMessages int         `json:"total_messages"`
	ByType        []TypeStats `json:"by_type"`
}

// sessionTypeSQL returns a SQL CASE expression for deriving session type.
func sessionTypeSQL(col string) string {
	return `CASE
	WHEN ` + col + ` = 'subagents' THEN 'subagent'
	WHEN ` + col + ` LIKE '%worktrees%' THEN 'worktree'
	WHEN ` + col + ` LIKE '%-private-tmp%' THEN 'ephemeral'
	ELSE 'interactive'
END`
}

// New creates or opens a transcript store.
func New(dbPath, projectDir string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			role TEXT NOT NULL,
			text TEXT NOT NULL,
			timestamp TEXT,
			type TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
		CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project);
		CREATE TABLE IF NOT EXISTS ingest_state (
			path TEXT PRIMARY KEY,
			offset INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS session_meta (
			session_id TEXT PRIMARY KEY,
			repo TEXT NOT NULL DEFAULT '',
			cwd TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT '',
			work_type TEXT NOT NULL DEFAULT '',
			topic TEXT NOT NULL DEFAULT ''
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	_, err = db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			text, role, project, session_id,
			content=messages,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS messages_ai;
		CREATE TRIGGER messages_ai AFTER INSERT ON messages
		WHEN new.is_noise = 0
		BEGIN
			INSERT INTO messages_fts(rowid, text, role, project, session_id)
			VALUES (new.id, new.text, new.role, new.project, new.session_id);
		END;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create FTS: %w", err)
	}

	_, err = db.Exec(`
		DROP VIEW IF EXISTS sessions;
		CREATE VIEW sessions AS
		SELECT
			m.session_id,
			m.project,
			` + sessionTypeSQL("m.project") + ` AS session_type,
			COALESCE(sm.repo, '') AS repo,
			COALESCE(sm.git_branch, '') AS git_branch,
			COALESCE(sm.work_type, '') AS work_type,
			COALESCE(sm.topic, '') AS topic,
			COUNT(*) AS total_msgs,
			SUM(CASE WHEN m.is_noise = 0 THEN 1 ELSE 0 END) AS substantive_msgs,
			MIN(m.timestamp) AS first_msg,
			MAX(m.timestamp) AS last_msg
		FROM messages m
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		GROUP BY m.session_id;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create sessions view: %w", err)
	}

	// Backfill session_meta for sessions without metadata by
	// re-reading the first entry of each JSONL file.
	backfillSessionMeta(db, projectDir)

	s := &Store{
		db:         db,
		projectDir: projectDir,
		offsets:    make(map[string]int64),
	}

	rows, err := db.Query("SELECT path, offset FROM ingest_state")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var path string
			var offset int64
			rows.Scan(&path, &offset)
			s.offsets[path] = offset
		}
	}

	return s, nil
}

func migrate(db *sql.DB) error {
	var colCount int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'is_noise'
	`).Scan(&colCount)
	if err != nil {
		return err
	}
	if colCount > 0 {
		return nil
	}

	slog.Info("migrating — adding is_noise column and rebuilding FTS index")

	_, err = db.Exec(`ALTER TABLE messages ADD COLUMN is_noise INTEGER NOT NULL DEFAULT 0`)
	if err != nil {
		return fmt.Errorf("add column: %w", err)
	}

	_, err = db.Exec(`
		UPDATE messages SET is_noise = 1
		WHERE text LIKE '%[Request interrupted%'
		   OR text LIKE '%Your task is to create a detailed summary%'
		   OR text IN ('Tool loaded.', 'Tool loaded')
		   OR text LIKE '%<local-command-caveat>%'
		   OR (text LIKE '%<command-name>%' AND LENGTH(text) < 200)
	`)
	if err != nil {
		return fmt.Errorf("backfill is_noise: %w", err)
	}

	_, err = db.Exec(`
		DROP TRIGGER IF EXISTS messages_ai;
		DROP TABLE IF EXISTS messages_fts;
		CREATE VIRTUAL TABLE messages_fts USING fts5(
			text, role, project, session_id,
			content=messages,
			content_rowid=id
		);
		INSERT INTO messages_fts(rowid, text, role, project, session_id)
			SELECT id, text, role, project, session_id FROM messages WHERE is_noise = 0;
	`)
	if err != nil {
		return fmt.Errorf("rebuild FTS: %w", err)
	}

	var noiseCount int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE is_noise = 1`).Scan(&noiseCount)
	slog.Info("migration complete", "noise_rows_flagged", noiseCount)

	return nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// IngestAll scans the project directory and ingests all JSONL files.
func (s *Store) IngestAll() error {
	return filepath.Walk(s.projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		return s.ingestFile(path)
	})
}

// Watch watches for new/modified JSONL files and ingests them in realtime.
func (s *Store) Watch() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	filepath.Walk(s.projectDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			watcher.Add(path)
		}
		return nil
	})

	slog.Info("watching for transcript changes", "dir", s.projectDir)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if strings.HasSuffix(event.Name, ".jsonl") &&
				(event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
				if err := s.ingestFile(event.Name); err != nil {
					slog.Error("ingest failed", "file", event.Name, "err", err)
				}
			}
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					watcher.Add(event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			slog.Error("watcher error", "err", err)
		}
	}
}

// Search performs a full-text search and returns matching messages.
func (s *Store) Search(query string, limit int, sessionType string) ([]SearchResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if sessionType == "" {
		sessionType = "interactive"
	}

	var sqlQuery string
	var args []any

	if sessionType == "all" {
		sqlQuery = `
			SELECT m.session_id, m.project, m.role, m.text, m.timestamp, rank
			FROM messages_fts f
			JOIN messages m ON m.id = f.rowid
			WHERE messages_fts MATCH ?
			ORDER BY rank
			LIMIT ?
		`
		args = []any{query, limit}
	} else {
		sqlQuery = `
			SELECT m.session_id, m.project, m.role, m.text, m.timestamp, rank
			FROM messages_fts f
			JOIN messages m ON m.id = f.rowid
			WHERE messages_fts MATCH ?
			  AND (` + sessionTypeSQL("m.project") + `) = ?
			ORDER BY rank
			LIMIT ?
		`
		args = []any{query, sessionType, limit}
	}

	rows, err := s.db.Query(sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.SessionID, &r.Project, &r.Role, &r.Text, &r.Timestamp, &r.Rank); err != nil {
			continue
		}
		if len(r.Text) > 500 {
			r.Text = r.Text[:497] + "..."
		}
		results = append(results, r)
	}
	return results, nil
}

// ListSessions returns session summaries, filtered and sorted.
func (s *Store) ListSessions(sessionType string, minMessages int, limit int, projectFilter, repoFilter, workTypeFilter string) ([]SessionInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if sessionType == "" {
		sessionType = "interactive"
	}
	if minMessages <= 0 {
		minMessages = 6
	}
	if limit <= 0 {
		limit = 30
	}

	where := []string{"substantive_msgs >= ?"}
	args := []any{minMessages}

	if sessionType != "all" {
		where = append(where, "session_type = ?")
		args = append(args, sessionType)
	}
	if projectFilter != "" {
		where = append(where, "project LIKE ?")
		args = append(args, "%"+projectFilter+"%")
	}
	if repoFilter != "" {
		where = append(where, "repo LIKE ?")
		args = append(args, "%"+repoFilter+"%")
	}
	if workTypeFilter != "" {
		where = append(where, "work_type = ?")
		args = append(args, workTypeFilter)
	}

	args = append(args, limit)

	q := `SELECT session_id, project, session_type, repo, git_branch, work_type, topic,
			total_msgs, substantive_msgs, first_msg, last_msg
		FROM sessions
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY last_msg DESC
		LIMIT ?`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SessionInfo
	for rows.Next() {
		var si SessionInfo
		if err := rows.Scan(&si.SessionID, &si.Project, &si.SessionType,
			&si.Repo, &si.GitBranch, &si.WorkType, &si.Topic,
			&si.TotalMsgs, &si.SubstantiveMsgs, &si.FirstMsg, &si.LastMsg); err != nil {
			continue
		}
		results = append(results, si)
	}
	return results, nil
}

// Stats returns detailed index statistics broken down by session type.
func (s *Store) Stats() (*StatsResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	var result StatsResult

	err := s.db.QueryRow("SELECT COUNT(DISTINCT session_id), COUNT(*) FROM messages").
		Scan(&result.TotalSessions, &result.TotalMessages)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT
			` + sessionTypeSQL("project") + ` AS session_type,
			COUNT(DISTINCT session_id) AS sessions,
			COUNT(*) AS total_msgs,
			SUM(CASE WHEN is_noise = 0 THEN 1 ELSE 0 END) AS substantive_msgs,
			SUM(CASE WHEN is_noise = 1 THEN 1 ELSE 0 END) AS noise_msgs
		FROM messages
		GROUP BY session_type
		ORDER BY sessions DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var ts TypeStats
		if err := rows.Scan(&ts.SessionType, &ts.Sessions, &ts.TotalMsgs,
			&ts.SubstantiveMsgs, &ts.NoiseMsgs); err != nil {
			continue
		}
		result.ByType = append(result.ByType, ts)
	}
	return &result, nil
}

// SessionMessage is a single message from a session transcript.
type SessionMessage struct {
	ID        int    `json:"id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	IsNoise   bool   `json:"is_noise"`
}

// ReadSession returns messages from a specific session, ordered by ID.
func (s *Store) ReadSession(sessionID string, role string, offset int, limit int) ([]SessionMessage, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 50
	}

	// Resolve prefix: if exact match fails, try prefix.
	resolvedID, err := s.resolveSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	where := []string{"session_id = ?"}
	args := []any{resolvedID}

	if role != "" {
		where = append(where, "role = ?")
		args = append(args, role)
	}

	args = append(args, limit, offset)

	q := `SELECT id, role, text, timestamp, is_noise FROM messages
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY id ASC
		LIMIT ? OFFSET ?`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SessionMessage
	for rows.Next() {
		var m SessionMessage
		var noise int
		if err := rows.Scan(&m.ID, &m.Role, &m.Text, &m.Timestamp, &noise); err != nil {
			continue
		}
		m.IsNoise = noise != 0
		results = append(results, m)
	}
	return results, nil
}

// resolveSessionID resolves a full or prefix session ID to an exact session ID.
func (s *Store) resolveSessionID(id string) (string, error) {
	// Try exact match first.
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE session_id = ? LIMIT 1", id).Scan(&count)
	if err != nil {
		return "", err
	}
	if count > 0 {
		return id, nil
	}

	// Try prefix match.
	rows, err := s.db.Query("SELECT DISTINCT session_id FROM messages WHERE session_id LIKE ? LIMIT 2", id+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var matches []string
	for rows.Next() {
		var sid string
		rows.Scan(&sid)
		matches = append(matches, sid)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session found matching %q", id)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous session prefix %q: matches %s and others", id, matches[0])
	}
}

// Query runs a read-only SQL query and returns rows as maps.
func (s *Store) Query(query string) ([]map[string]any, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var results []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		results = append(results, row)
		if len(results) >= 100 {
			break
		}
	}
	return results, nil
}

func backfillSessionMeta(db *sql.DB, projectDir string) {
	// Find sessions without metadata.
	rows, err := db.Query(`
		SELECT DISTINCT m.session_id, m.project
		FROM messages m
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		WHERE sm.session_id IS NULL
	`)
	if err != nil {
		slog.Warn("backfill query failed", "err", err)
		return
	}
	defer rows.Close()

	type pending struct {
		sessionID, project string
	}
	var sessions []pending
	for rows.Next() {
		var p pending
		rows.Scan(&p.sessionID, &p.project)
		sessions = append(sessions, p)
	}
	if len(sessions) == 0 {
		return
	}

	slog.Info("backfilling session metadata", "sessions", len(sessions))

	tx, _ := db.Begin()
	defer tx.Rollback()

	stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO session_meta
		(session_id, repo, cwd, git_branch, work_type, topic) VALUES (?, ?, ?, ?, ?, ?)`)
	defer stmt.Close()

	filled := 0
	for _, s := range sessions {
		path := filepath.Join(projectDir, s.project, s.sessionID+".jsonl")
		cwd, branch, topic := extractMetaFromFile(path)
		repo := extractRepo(cwd)
		workType := classifyWorkType(branch)
		stmt.Exec(s.sessionID, repo, cwd, branch, workType, topic)
		if repo != "" {
			filled++
		}
	}

	tx.Commit()
	slog.Info("backfill complete", "total", len(sessions), "with_repo", filled)
}

// extractMetaFromFile reads a JSONL file to extract cwd, gitBranch,
// and the first substantive user message as topic.
func extractMetaFromFile(path string) (cwd, branch, topic string) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		var entry map[string]any
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if c, _ := entry["cwd"].(string); c != "" && cwd == "" {
			cwd = c
		}
		if b, _ := entry["gitBranch"].(string); b != "" && branch == "" {
			branch = b
		}

		// Extract topic from first substantive user message.
		if topic == "" {
			if typ, _ := entry["type"].(string); typ == "user" {
				if msg, _ := entry["message"].(map[string]any); msg != nil {
					text := extractText(msg)
					if len(text) >= 10 && !isNoise(text) && !isBoilerplate(text) {
						topic = text
						if len(topic) > 200 {
							topic = topic[:197] + "..."
						}
					}
				}
			}
		}

		if cwd != "" && branch != "" && topic != "" {
			return
		}
	}
	return
}

// extractText pulls text content from a message's content field.
func extractText(msg map[string]any) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		for _, c := range content {
			cm, _ := c.(map[string]any)
			if cm["type"] == "text" {
				if text, _ := cm["text"].(string); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

// repoPattern extracts org/repo from paths like /Users/.../github.com/org/repo/...
var repoPattern = regexp.MustCompile(`/work/github\.com/([^/]+/[^/]+)`)

// extractRepo derives an org/repo string from a working directory path.
func extractRepo(cwd string) string {
	m := repoPattern.FindStringSubmatch(cwd)
	if m == nil {
		return ""
	}
	return m[1]
}

// classifyWorkType derives a work type from a git branch name.
func classifyWorkType(branch string) string {
	if branch == "" || branch == "HEAD" {
		return ""
	}

	b := strings.ToLower(branch)

	// Check prefix patterns.
	prefixes := map[string]string{
		"fix/":      "bugfix",
		"bugfix/":   "bugfix",
		"hotfix/":   "bugfix",
		"feature/":  "feature",
		"feat/":     "feature",
		"refactor/": "refactor",
		"chore/":    "chore",
		"docs/":     "docs",
		"test/":     "test",
		"ci/":       "ci",
		"release/":  "release",
		"review/":   "review",
	}
	for prefix, workType := range prefixes {
		if strings.HasPrefix(b, prefix) {
			return workType
		}
	}

	// Check if it contains common keywords.
	keywords := map[string]string{
		"fix":      "bugfix",
		"bug":      "bugfix",
		"feature":  "feature",
		"refactor": "refactor",
	}
	for kw, workType := range keywords {
		if strings.Contains(b, kw) {
			return workType
		}
	}

	// Default branch = general development.
	if b == "master" || b == "main" || b == "dev" || b == "develop" {
		return "development"
	}

	return "branch-work"
}

// isNoise returns true if a message text matches noise patterns.
func isNoise(text string) bool {
	if strings.Contains(text, "[Request interrupted") {
		return true
	}
	if strings.Contains(text, "Your task is to create a detailed summary") {
		return true
	}
	if text == "Tool loaded." || text == "Tool loaded" {
		return true
	}
	if strings.Contains(text, "<local-command-caveat>") {
		return true
	}
	if strings.Contains(text, "<command-name>") && len(text) < 200 {
		return true
	}
	return false
}

// isBoilerplate returns true if the text looks like a slash-command
// skill expansion rather than genuine user input.
// isBoilerplate returns true if the text is system/skill boilerplate
// rather than genuine user input — unsuitable as a session topic.
func isBoilerplate(text string) bool {
	return strings.HasPrefix(text, "Base directory for this skill:") ||
		strings.HasPrefix(text, "Read and execute ~/") ||
		strings.HasPrefix(text, "Read and return the full contents") ||
		strings.HasPrefix(text, "<task-notification>") ||
		strings.HasPrefix(text, "<system-reminder>")
}

func (s *Store) ingestFile(path string) error {
	s.mu.Lock()
	offset := s.offsets[path]
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if offset > 0 {
		f.Seek(offset, 0)
	}

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	project := filepath.Base(filepath.Dir(path))

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	count := 0
	var metaCwd, metaBranch, metaTopic string

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Extract session metadata from any entry.
		if cwd, _ := entry["cwd"].(string); cwd != "" && metaCwd == "" {
			metaCwd = cwd
		}
		if branch, _ := entry["gitBranch"].(string); branch != "" && metaBranch == "" {
			metaBranch = branch
		}

		typ, _ := entry["type"].(string)
		if typ != "user" && typ != "assistant" {
			continue
		}

		msg, _ := entry["message"].(map[string]any)
		if msg == nil {
			continue
		}

		ts, _ := entry["timestamp"].(string)
		if ts == "" {
			ts = time.Now().Format(time.RFC3339)
		}

		role := typ

		insertMsg := func(text string) {
			noise := 0
			if isNoise(text) {
				noise = 1
			}
			// Capture first substantive user message as topic.
			if metaTopic == "" && role == "user" && noise == 0 && len(text) >= 10 && !isBoilerplate(text) {
				metaTopic = text
				if len(metaTopic) > 200 {
					metaTopic = metaTopic[:197] + "..."
				}
			}
			stmt.Exec(sessionID, project, role, text, ts, typ, noise)
			count++
		}

		switch content := msg["content"].(type) {
		case string:
			if content != "" {
				insertMsg(content)
			}
		case []any:
			for _, c := range content {
				cm, _ := c.(map[string]any)
				if cm["type"] == "text" {
					if text, _ := cm["text"].(string); text != "" {
						insertMsg(text)
					}
				}
			}
		}
	}

	// Upsert session metadata.
	if metaCwd != "" || metaBranch != "" || metaTopic != "" {
		repo := extractRepo(metaCwd)
		workType := classifyWorkType(metaBranch)
		tx.Exec(`INSERT INTO session_meta (session_id, repo, cwd, git_branch, work_type, topic)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE session_meta.repo END,
				cwd = CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE session_meta.cwd END,
				git_branch = CASE WHEN excluded.git_branch != '' THEN excluded.git_branch ELSE session_meta.git_branch END,
				work_type = CASE WHEN excluded.work_type != '' THEN excluded.work_type ELSE session_meta.work_type END,
				topic = CASE WHEN excluded.topic != '' AND session_meta.topic = '' THEN excluded.topic ELSE session_meta.topic END`,
			sessionID, repo, metaCwd, metaBranch, workType, metaTopic)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	newOffset, _ := f.Seek(0, 1)
	s.mu.Lock()
	s.offsets[path] = newOffset
	s.mu.Unlock()

	s.db.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", path, newOffset)

	if count > 0 {
		slog.Debug("ingested", "file", filepath.Base(path), "messages", count)
	}
	return nil
}
