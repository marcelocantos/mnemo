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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
)

// NoncePrefix is the prefix for self-identification nonces.
const NoncePrefix = "mnemo:self:"

// Store is a searchable index of Claude Code transcripts.
type Store struct {
	db         *sql.DB
	projectDir string

	mu      sync.Mutex
	offsets map[string]int64 // file path → last read offset

	rwmu sync.RWMutex // protects db access: writers (ingest), readers (queries)
}

// ContextMessage is a message surrounding a search hit.
type ContextMessage struct {
	ID        int    `json:"id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
}

// SearchResult is a single search hit with optional surrounding context.
type SearchResult struct {
	MessageID int              `json:"message_id"`
	SessionID string           `json:"session_id"`
	Project   string           `json:"project"`
	Role      string           `json:"role"`
	Text      string           `json:"text"`
	Timestamp string           `json:"timestamp"`
	Rank      float64          `json:"rank"`
	Before    []ContextMessage `json:"before,omitempty"`
	After     []ContextMessage `json:"after,omitempty"`
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

// RepoInfo holds summary information about a repository.
type RepoInfo struct {
	Repo         string `json:"repo"`
	Path         string `json:"path"`
	Sessions     int    `json:"sessions"`
	LastActivity string `json:"last_activity"`
}

// RecentActivityInfo summarises recent session activity for a single repo.
type RecentActivityInfo struct {
	Repo         string   `json:"repo"`
	Path         string   `json:"path"`
	Sessions     int      `json:"sessions"`
	Messages     int      `json:"messages"`
	LastActivity string   `json:"last_activity"`
	WorkTypes    []string `json:"work_types,omitempty"`
	Topics       []string `json:"topics,omitempty"`
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

// schemaVersion is incremented whenever the database schema changes.
// On mismatch the database file is deleted and rebuilt from transcripts.
const schemaVersion = 4

func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA cache_size = -64000",
		"PRAGMA mmap_size = 268435456",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return db, nil
}

// New creates or opens a transcript store.
func New(dbPath, projectDir string) (*Store, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	// Check schema version. On mismatch, blow away the database.
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err == nil && ver != schemaVersion && ver != 0 {
		slog.Info("schema version mismatch, rebuilding database", "have", ver, "want", schemaVersion)
		db.Close()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(dbPath + suffix)
		}
		db, err = openDB(dbPath)
		if err != nil {
			return nil, err
		}
	}

	// Set the schema version.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("set schema version: %w", err)
	}

	// Create tables.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			role TEXT NOT NULL,
			text TEXT NOT NULL,
			timestamp TEXT,
			type TEXT,
			is_noise INTEGER NOT NULL DEFAULT 0,
			content_type TEXT NOT NULL DEFAULT 'text',
			tool_name TEXT,
			tool_use_id TEXT,
			tool_input BLOB,
			is_error INTEGER NOT NULL DEFAULT 0,
			-- Computed columns: commonly queried fields from tool_input.
			tool_file_path TEXT GENERATED ALWAYS AS (tool_input->>'file_path'),
			tool_command TEXT GENERATED ALWAYS AS (tool_input->>'command'),
			tool_pattern TEXT GENERATED ALWAYS AS (tool_input->>'pattern'),
			tool_description TEXT GENERATED ALWAYS AS (tool_input->>'description'),
			tool_skill TEXT GENERATED ALWAYS AS (tool_input->>'skill'),
			tool_old_string TEXT GENERATED ALWAYS AS (tool_input->>'old_string'),
			tool_new_string TEXT GENERATED ALWAYS AS (tool_input->>'new_string'),
			tool_content TEXT GENERATED ALWAYS AS (tool_input->>'content'),
			tool_query TEXT GENERATED ALWAYS AS (tool_input->>'query'),
			tool_url TEXT GENERATED ALWAYS AS (tool_input->>'url'),
			tool_name_param TEXT GENERATED ALWAYS AS (tool_input->>'name'),
			tool_prompt TEXT GENERATED ALWAYS AS (tool_input->>'prompt'),
			tool_subject TEXT GENERATED ALWAYS AS (tool_input->>'subject'),
			tool_status TEXT GENERATED ALWAYS AS (tool_input->>'status'),
			tool_task_id TEXT GENERATED ALWAYS AS (COALESCE(tool_input->>'task_id', tool_input->>'taskId'))
		);
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
		CREATE TABLE IF NOT EXISTS session_nonces (
			nonce TEXT PRIMARY KEY,
			session_id TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_session_nonces_session ON session_nonces(session_id);
		CREATE TABLE IF NOT EXISTS session_summary (
			session_id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			session_type TEXT NOT NULL DEFAULT 'interactive',
			total_msgs INTEGER NOT NULL DEFAULT 0,
			substantive_msgs INTEGER NOT NULL DEFAULT 0,
			first_msg TEXT NOT NULL DEFAULT '',
			last_msg TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_session_summary_type ON session_summary(session_type);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	// Create indexes.
	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
		CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project);
		CREATE INDEX IF NOT EXISTS idx_messages_content_type ON messages(content_type);
		CREATE INDEX IF NOT EXISTS idx_messages_tool_name ON messages(tool_name);
		CREATE INDEX IF NOT EXISTS idx_messages_tool_use_id ON messages(tool_use_id);
		CREATE INDEX IF NOT EXISTS idx_messages_tool_file_path ON messages(tool_file_path) WHERE tool_file_path IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_command ON messages(tool_command) WHERE tool_command IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_pattern ON messages(tool_pattern) WHERE tool_pattern IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_description ON messages(tool_description) WHERE tool_description IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_skill ON messages(tool_skill) WHERE tool_skill IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_old_string ON messages(tool_old_string) WHERE tool_old_string IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_new_string ON messages(tool_new_string) WHERE tool_new_string IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_url ON messages(tool_url) WHERE tool_url IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_tool_task_id ON messages(tool_task_id) WHERE tool_task_id IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_is_error ON messages(is_error) WHERE is_error = 1;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create indexes: %w", err)
	}

	_, err = db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			text, role, project, session_id,
			content=messages,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS messages_ai;
		CREATE TRIGGER messages_ai AFTER INSERT ON messages
		BEGIN
			INSERT INTO messages_fts(rowid, text, role, project, session_id)
			SELECT new.id, new.text, new.role, new.project, new.session_id
			WHERE new.is_noise = 0;

			INSERT INTO session_summary (session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg)
			VALUES (new.session_id, new.project,
				` + sessionTypeSQL("new.project") + `,
				1,
				CASE WHEN new.is_noise = 0 THEN 1 ELSE 0 END,
				new.timestamp, new.timestamp)
			ON CONFLICT(session_id) DO UPDATE SET
				total_msgs = total_msgs + 1,
				substantive_msgs = substantive_msgs + CASE WHEN new.is_noise = 0 THEN 1 ELSE 0 END,
				first_msg = MIN(first_msg, new.timestamp),
				last_msg = MAX(last_msg, new.timestamp);
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
			ss.session_id,
			ss.project,
			ss.session_type,
			COALESCE(sm.repo, '') AS repo,
			COALESCE(sm.git_branch, '') AS git_branch,
			COALESCE(sm.work_type, '') AS work_type,
			COALESCE(sm.topic, '') AS topic,
			ss.total_msgs,
			ss.substantive_msgs,
			ss.first_msg,
			ss.last_msg
		FROM session_summary ss
		LEFT JOIN session_meta sm ON sm.session_id = ss.session_id;
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

// No migration functions needed — schema version mismatch deletes
// the database and rebuilds from transcripts.

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// parsedMessage is a single message ready for insertion.
type parsedMessage struct {
	role        string
	text        string
	timestamp   string
	typ         string
	isNoise     int
	contentType string
	toolName    string
	toolUseID   string
	toolInput   []byte // raw JSON, nil if not tool_use
	isError     int
}

// parsedFile is the result of parsing a single JSONL file.
type parsedFile struct {
	path      string
	sessionID string
	project   string
	messages  []parsedMessage
	cwd       string
	branch    string
	topic     string
	newOffset int64
}

// IngestAll scans the project directory and ingests all JSONL files
// using a parallel pipeline: collector → N workers → 1 writer.
func (s *Store) IngestAll() error {
	numWorkers := runtime.NumCPU()
	if numWorkers < 2 {
		numWorkers = 2
	}

	// Stage 1: Collector — gather paths, sort newest-first, filter already-done.
	type fileEntry struct {
		path   string
		mtime  time.Time
		size   int64
		offset int64 // already-ingested offset
	}
	var files []fileEntry
	filepath.Walk(s.projectDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		s.mu.Lock()
		offset := s.offsets[path]
		s.mu.Unlock()
		// Skip fully ingested files.
		if offset >= info.Size() {
			return nil
		}
		files = append(files, fileEntry{
			path:   path,
			mtime:  info.ModTime(),
			size:   info.Size(),
			offset: offset,
		})
		return nil
	})

	if len(files) == 0 {
		return nil
	}

	// Sort newest first so recent sessions are available quickly.
	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.After(files[j].mtime)
	})

	slog.Info("ingest starting", "files", len(files), "workers", numWorkers)

	// Stage 2: Workers — parse JSONL files in parallel.
	pathCh := make(chan fileEntry, numWorkers)
	parsedCh := make(chan parsedFile, numWorkers*2)

	var wg sync.WaitGroup
	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fe := range pathCh {
				if pf, err := parseFile(fe.path, fe.offset); err == nil {
					parsedCh <- pf
				} else {
					slog.Warn("parse failed", "file", filepath.Base(fe.path), "err", err)
				}
			}
		}()
	}

	// Feed the workers.
	go func() {
		for _, fe := range files {
			pathCh <- fe
		}
		close(pathCh)
		wg.Wait()
		close(parsedCh)
	}()

	// Stage 3: Writer — single goroutine, batched transactions.
	const commitInterval = 200 * time.Millisecond
	const insertSQL = `INSERT INTO messages
		(session_id, project, role, text, timestamp, type, is_noise,
		 content_type, tool_name, tool_use_id, tool_input, is_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, jsonb(?), ?)`

	if err := s.runWriter(parsedCh, insertSQL); err != nil {
		return err
	}

	// FTS5 optimize (segment merging) is skipped intentionally.
	// On a fresh 577k-message database it takes 10+ minutes of solid
	// CPU at 100%, blocking all reads. FTS5 works fine with multiple
	// segments — search performance is slightly suboptimal but queries
	// complete in milliseconds regardless. Segments merge naturally as
	// new data trickles in via the watcher.
	return nil
}

// runWriter is the single-goroutine writer for the parallel ingest pipeline.
// It consumes parsed files from the channel and inserts them in batched
// transactions, yielding the write lock every 200ms for readers.
func (s *Store) runWriter(parsedCh <-chan parsedFile, insertSQL string) error {
	const commitInterval = 200 * time.Millisecond

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { tx.Rollback() }()

	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return err
	}
	defer func() { stmt.Close() }()

	lastCommit := time.Now()

	commitBatch := func() error {
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
		// Yield write lock for readers.
		s.rwmu.Unlock()
		s.rwmu.Lock()
		tx, err = s.db.Begin()
		if err != nil {
			return err
		}
		stmt, err = tx.Prepare(insertSQL)
		if err != nil {
			tx.Rollback()
			return err
		}
		lastCommit = time.Now()
		return nil
	}

	for pf := range parsedCh {
		for _, m := range pf.messages {
			var toolInput any
			if m.toolInput != nil {
				toolInput = string(m.toolInput)
			}
			stmt.Exec(pf.sessionID, pf.project, m.role, m.text, m.timestamp, m.typ, m.isNoise,
				m.contentType, m.toolName, m.toolUseID, toolInput, m.isError)

			// Detect self-identification nonces.
			if m.contentType == "text" && strings.HasPrefix(m.text, NoncePrefix) {
				tx.Exec("INSERT OR IGNORE INTO session_nonces (nonce, session_id) VALUES (?, ?)",
					strings.TrimSpace(m.text), pf.sessionID)
			}
		}

		// Upsert session metadata.
		if pf.cwd != "" || pf.branch != "" || pf.topic != "" {
			repo := extractRepo(pf.cwd)
			workType := classifyWorkType(pf.branch)
			tx.Exec(`INSERT INTO session_meta (session_id, repo, cwd, git_branch, work_type, topic)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(session_id) DO UPDATE SET
					repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE session_meta.repo END,
					cwd = CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE session_meta.cwd END,
					git_branch = CASE WHEN excluded.git_branch != '' THEN excluded.git_branch ELSE session_meta.git_branch END,
					work_type = CASE WHEN excluded.work_type != '' THEN excluded.work_type ELSE session_meta.work_type END,
					topic = CASE WHEN excluded.topic != '' AND session_meta.topic = '' THEN excluded.topic ELSE session_meta.topic END`,
				pf.sessionID, repo, pf.cwd, pf.branch, workType, pf.topic)
		}

		// Update ingest offset.
		tx.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", pf.path, pf.newOffset)
		s.mu.Lock()
		s.offsets[pf.path] = pf.newOffset
		s.mu.Unlock()

		// Commit periodically.
		if time.Since(lastCommit) >= commitInterval {
			if err := commitBatch(); err != nil {
				return err
			}
		}
	}

	// Final commit.
	stmt.Close()
	return tx.Commit()
}

// parseFile reads and parses a JSONL transcript file, returning all
// extracted messages and metadata. Pure computation — no DB access.
func parseFile(path string, offset int64) (parsedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return parsedFile{}, err
	}
	defer f.Close()

	if offset > 0 {
		f.Seek(offset, 0)
	}

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	project := filepath.Base(filepath.Dir(path))

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	pf := parsedFile{
		path:      path,
		sessionID: sessionID,
		project:   project,
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry jsonlEntry
		if json.Unmarshal(line, &entry) != nil {
			continue
		}

		if entry.Cwd != "" && pf.cwd == "" {
			pf.cwd = entry.Cwd
		}
		if entry.GitBranch != "" && pf.branch == "" {
			pf.branch = entry.GitBranch
		}

		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}

		blocks := extractBlocks(entry.Message)
		if len(blocks) == 0 {
			continue
		}

		ts := entry.Timestamp
		if ts == "" {
			ts = time.Now().Format(time.RFC3339)
		}

		for _, b := range blocks {
			noise := 0
			if b.ContentType == "text" && isNoise(b.Text) {
				noise = 1
			}
			if pf.topic == "" && entry.Type == "user" && b.ContentType == "text" && noise == 0 && len(b.Text) >= 10 && !isBoilerplate(b.Text) {
				pf.topic = b.Text
				if len(pf.topic) > 200 {
					pf.topic = pf.topic[:197] + "..."
				}
			}

			isErr := 0
			if b.IsError {
				isErr = 1
			}

			pf.messages = append(pf.messages, parsedMessage{
				role:        entry.Type,
				text:        b.Text,
				timestamp:   ts,
				typ:         entry.Type,
				isNoise:     noise,
				contentType: b.ContentType,
				toolName:    b.ToolName,
				toolUseID:   b.ToolUseID,
				toolInput:   b.ToolInput,
				isError:     isErr,
			})
		}
	}

	pf.newOffset, _ = f.Seek(0, 1)
	return pf, nil
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
			if wErr := watcher.Add(path); wErr != nil {
				slog.Warn("failed to watch directory", "path", path, "err", wErr)
			}
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
					if wErr := watcher.Add(event.Name); wErr != nil {
						slog.Warn("failed to watch new directory", "path", event.Name, "err", wErr)
					}
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

// Search performs a full-text search and returns matching messages
// with optional surrounding context messages.
func (s *Store) Search(query string, limit int, sessionType, repoFilter string, contextBefore, contextAfter int, substantiveOnly bool) ([]SearchResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if sessionType == "" {
		sessionType = "interactive"
	}

	// Two-phase search: first get top FTS hits (fast, no JOINs),
	// then filter by session type/repo and enrich with message data.
	// This avoids JOINing the full FTS result set with large tables.
	needSessionFilter := sessionType != "all"
	needRepoFilter := repoFilter != ""

	// Phase 1: FTS-only scan with generous over-fetch.
	// Over-fetch 10x to account for session type filtering.
	fetchLimit := limit * 10
	if fetchLimit < 200 {
		fetchLimit = 200
	}
	ftsRows, err := s.db.Query(`
		SELECT rowid, rank FROM messages_fts
		WHERE messages_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, query, fetchLimit)
	if err != nil {
		return nil, err
	}

	type ftsHit struct {
		rowid int
		rank  float64
	}
	var hits []ftsHit
	for ftsRows.Next() {
		var h ftsHit
		if err := ftsRows.Scan(&h.rowid, &h.rank); err != nil {
			continue
		}
		hits = append(hits, h)
	}
	ftsRows.Close()

	if len(hits) == 0 {
		return nil, nil
	}

	// Phase 2: enrich hits with message data and apply filters.
	var results []SearchResult
	for _, h := range hits {
		if len(results) >= limit {
			break
		}

		row := s.db.QueryRow(`
			SELECT m.id, m.session_id, m.project, m.role, m.text, m.timestamp
			FROM messages m
			WHERE m.id = ?
		`, h.rowid)

		var r SearchResult
		if err := row.Scan(&r.MessageID, &r.SessionID, &r.Project, &r.Role, &r.Text, &r.Timestamp); err != nil {
			continue
		}
		r.Rank = h.rank

		// Apply session type filter.
		if needSessionFilter {
			var st string
			err := s.db.QueryRow("SELECT session_type FROM session_summary WHERE session_id = ?", r.SessionID).Scan(&st)
			if err != nil || st != sessionType {
				continue
			}
		}

		// Apply repo filter.
		if needRepoFilter {
			var count int
			pattern := "%" + repoFilter + "%"
			err := s.db.QueryRow("SELECT COUNT(*) FROM session_meta WHERE session_id = ? AND (cwd LIKE ? OR repo LIKE ?)", r.SessionID, pattern, pattern).Scan(&count)
			if err != nil || count == 0 {
				continue
			}
		}

		if len(r.Text) > 500 {
			r.Text = r.Text[:497] + "..."
		}
		results = append(results, r)
	}

	// Fetch context messages for each hit.
	if (contextBefore > 0 || contextAfter > 0) && len(results) > 0 {
		for i := range results {
			r := &results[i]
			if contextBefore > 0 {
				r.Before = s.fetchContext(r.SessionID, r.MessageID, contextBefore, true, substantiveOnly)
			}
			if contextAfter > 0 {
				r.After = s.fetchContext(r.SessionID, r.MessageID, contextAfter, false, substantiveOnly)
			}
		}
	}

	return results, nil
}

// fetchContext retrieves messages before or after a given message ID within the same session.
// If substantiveOnly is true, only non-noise user/assistant messages are included.
func (s *Store) fetchContext(sessionID string, messageID int, count int, before, substantiveOnly bool) []ContextMessage {
	filter := ""
	if substantiveOnly {
		filter = " AND is_noise = 0 AND role IN ('user', 'assistant')"
	}
	var q string
	if before {
		q = `SELECT id, role, text, timestamp FROM messages
			WHERE session_id = ? AND id < ?` + filter + ` ORDER BY id DESC LIMIT ?`
	} else {
		q = `SELECT id, role, text, timestamp FROM messages
			WHERE session_id = ? AND id > ?` + filter + ` ORDER BY id ASC LIMIT ?`
	}

	rows, err := s.db.Query(q, sessionID, messageID, count)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var msgs []ContextMessage
	for rows.Next() {
		var m ContextMessage
		if err := rows.Scan(&m.ID, &m.Role, &m.Text, &m.Timestamp); err != nil {
			continue
		}
		if len(m.Text) > 500 {
			m.Text = m.Text[:497] + "..."
		}
		msgs = append(msgs, m)
	}

	// Reverse "before" results so they're in chronological order.
	if before {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	return msgs
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

	rows, err := s.db.Query(`
		SELECT
			session_type,
			COUNT(*) AS sessions,
			SUM(total_msgs) AS total_msgs,
			SUM(substantive_msgs) AS substantive_msgs,
			SUM(total_msgs - substantive_msgs) AS noise_msgs
		FROM session_summary
		GROUP BY session_type
		ORDER BY sessions DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result StatsResult
	for rows.Next() {
		var ts TypeStats
		if err := rows.Scan(&ts.SessionType, &ts.Sessions, &ts.TotalMsgs,
			&ts.SubstantiveMsgs, &ts.NoiseMsgs); err != nil {
			continue
		}
		result.TotalSessions += ts.Sessions
		result.TotalMessages += ts.TotalMsgs
		result.ByType = append(result.ByType, ts)
	}
	return &result, nil
}

// ListRepos returns a list of repositories with session counts and last activity.
// The optional filter supports bare names ("mnemo"), org/repo paths
// ("marcelocantos/mnemo"), and globs ("marcelocantos/sql*").
func (s *Store) ListRepos(filter string) ([]RepoInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	// Convert glob-style filter to SQL LIKE pattern.
	var where string
	var args []any
	if filter != "" {
		pattern := strings.ReplaceAll(filter, "*", "%")
		if !strings.ContainsAny(pattern, "/%") {
			// Bare name: match anywhere in repo or cwd.
			pattern = "%" + pattern + "%"
		} else if !strings.Contains(pattern, "%") {
			// Exact org/repo or path fragment: substring match.
			pattern = "%" + pattern + "%"
		}
		where = "WHERE (sm.repo LIKE ? OR sm.cwd LIKE ?)"
		args = []any{pattern, pattern}
	}

	q := `
		SELECT
			CASE WHEN sm.repo != '' THEN sm.repo ELSE sm.cwd END AS display_repo,
			MAX(sm.cwd) AS path,
			COUNT(DISTINCT sm.session_id) AS sessions,
			MAX(ss.last_msg) AS last_activity
		FROM session_meta sm
		JOIN session_summary ss ON ss.session_id = sm.session_id
		` + where + `
		GROUP BY display_repo
		HAVING display_repo != ''
		ORDER BY last_activity DESC
	`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RepoInfo
	for rows.Next() {
		var r RepoInfo
		if err := rows.Scan(&r.Repo, &r.Path, &r.Sessions, &r.LastActivity); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

// RecentActivity returns per-repo summaries of session activity within the
// given recency window. Only interactive sessions are included.
func (s *Store) RecentActivity(days int, repoFilter string) ([]RecentActivityInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if days <= 0 {
		days = 7
	}

	where := []string{
		"ss.session_type = 'interactive'",
		"ss.last_msg >= datetime('now', ?)",
	}
	args := []any{fmt.Sprintf("-%d days", days)}

	if repoFilter != "" {
		where = append(where, "(sm.repo LIKE ? OR sm.cwd LIKE ?)")
		pattern := "%" + repoFilter + "%"
		args = append(args, pattern, pattern)
	}

	q := `
		SELECT
			CASE WHEN sm.repo != '' THEN sm.repo ELSE sm.cwd END AS display_repo,
			MAX(sm.cwd) AS path,
			COUNT(DISTINCT ss.session_id) AS sessions,
			SUM(ss.substantive_msgs) AS messages,
			MAX(ss.last_msg) AS last_activity,
			GROUP_CONCAT(DISTINCT NULLIF(sm.work_type, '')) AS work_types,
			GROUP_CONCAT(DISTINCT NULLIF(sm.topic, '')) AS topics
		FROM session_summary ss
		JOIN session_meta sm ON sm.session_id = ss.session_id
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY display_repo
		HAVING display_repo != ''
		ORDER BY last_activity DESC
	`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RecentActivityInfo
	for rows.Next() {
		var r RecentActivityInfo
		var workTypes, topics sql.NullString
		if err := rows.Scan(&r.Repo, &r.Path, &r.Sessions, &r.Messages,
			&r.LastActivity, &workTypes, &topics); err != nil {
			continue
		}
		if workTypes.Valid && workTypes.String != "" {
			r.WorkTypes = strings.Split(workTypes.String, ",")
		}
		if topics.Valid && topics.String != "" {
			r.Topics = strings.Split(topics.String, ",")
		}
		results = append(results, r)
	}
	return results, nil
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
	// Try exact match first (session_summary has one row per session).
	var exists int
	err := s.db.QueryRow("SELECT 1 FROM session_summary WHERE session_id = ?", id).Scan(&exists)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	// Try prefix match.
	rows, err := s.db.Query("SELECT session_id FROM session_summary WHERE session_id LIKE ? LIMIT 2", id+"%")
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

// ResolveNonce looks up the session ID associated with a self-identification nonce.
func (s *Store) ResolveNonce(nonce string) (string, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	var sessionID string
	err := s.db.QueryRow(
		"SELECT session_id FROM session_nonces WHERE nonce = ?", nonce,
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("nonce not found — transcript may not be ingested yet")
	}
	return sessionID, nil
}

// Query runs a read-only SQL query and returns rows as maps.
func (s *Store) Query(query string) ([]map[string]any, error) {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return nil, fmt.Errorf("only SELECT and WITH queries are allowed")
	}

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
	// Quick check: any sessions missing metadata?
	var missing int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM session_summary ss
		WHERE NOT EXISTS (SELECT 1 FROM session_meta sm WHERE sm.session_id = ss.session_id)
	`).Scan(&missing); err != nil || missing == 0 {
		return
	}

	// Find sessions without metadata.
	rows, err := db.Query(`
		SELECT ss.session_id, ss.project
		FROM session_summary ss
		WHERE NOT EXISTS (SELECT 1 FROM session_meta sm WHERE sm.session_id = ss.session_id)
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
		var entry jsonlEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		if entry.Cwd != "" && cwd == "" {
			cwd = entry.Cwd
		}
		if entry.GitBranch != "" && branch == "" {
			branch = entry.GitBranch
		}

		// Extract topic from first substantive user text message.
		if topic == "" && entry.Type == "user" {
			for _, b := range extractBlocks(entry.Message) {
				if b.ContentType == "text" && len(b.Text) >= 10 && !isNoise(b.Text) && !isBoilerplate(b.Text) {
					topic = b.Text
					if len(topic) > 200 {
						topic = topic[:197] + "..."
					}
					break
				}
			}
		}

		if cwd != "" && branch != "" && topic != "" {
			return
		}
	}
	return
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

// jsonlEntry is the minimal structure of a JSONL transcript line.
type jsonlEntry struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	GitBranch string          `json:"gitBranch"`
	Message   json.RawMessage `json:"message"`
}

// jsonlMessage is the message field within a JSONL entry.
type jsonlMessage struct {
	Content json.RawMessage `json:"content"`
}

// contentBlock represents a parsed content block from a message.
type contentBlock struct {
	ContentType string // text, tool_use, tool_result, thinking
	Text        string // the displayable text
	ToolName    string // for tool_use
	ToolUseID   string // for tool_use and tool_result
	ToolInput   []byte // raw JSON for tool_use input
	IsError     bool   // for tool_result
}

// rawContentBlock is the JSON shape of a content block.
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// extractBlocks extracts all content blocks from a raw message JSON.
func extractBlocks(raw json.RawMessage) []contentBlock {
	var msg jsonlMessage
	if json.Unmarshal(raw, &msg) != nil || msg.Content == nil {
		return nil
	}

	// Try string content first (simple user messages).
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		if s != "" {
			return []contentBlock{{ContentType: "text", Text: s}}
		}
		return nil
	}

	// Parse array of content blocks.
	var raws []rawContentBlock
	if json.Unmarshal(msg.Content, &raws) != nil {
		return nil
	}

	var blocks []contentBlock
	for _, r := range raws {
		switch r.Type {
		case "text":
			if r.Text != "" {
				blocks = append(blocks, contentBlock{ContentType: "text", Text: r.Text})
			}
		case "thinking":
			if r.Thinking != "" {
				blocks = append(blocks, contentBlock{ContentType: "thinking", Text: r.Thinking})
			}
		case "tool_use":
			text := r.Name
			if r.Input != nil {
				text = r.Name + " " + string(r.Input)
			}
			blocks = append(blocks, contentBlock{
				ContentType: "tool_use",
				Text:        text,
				ToolName:    r.Name,
				ToolUseID:   r.ID,
				ToolInput:   r.Input,
			})
		case "tool_result":
			// tool_result content can be string or array of blocks.
			var resultText string
			if r.Content != nil {
				// Try string.
				if json.Unmarshal(r.Content, &resultText) != nil {
					// Try array of text blocks.
					var parts []rawContentBlock
					if json.Unmarshal(r.Content, &parts) == nil {
						var texts []string
						for _, p := range parts {
							if p.Type == "text" && p.Text != "" {
								texts = append(texts, p.Text)
							}
						}
						resultText = strings.Join(texts, "\n")
					}
				}
			}
			blocks = append(blocks, contentBlock{
				ContentType: "tool_result",
				Text:        resultText,
				ToolUseID:   r.ToolUseID,
				IsError:     r.IsError,
			})
		}
	}
	return blocks
}

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
		if errors.Is(err, os.ErrNotExist) {
			return nil // file deleted between event and open
		}
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

	const yieldInterval = 200 * time.Millisecond
	const lineCheckInterval = 50

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { tx.Rollback() }()

	const insertSQL = `INSERT INTO messages
		(session_id, project, role, text, timestamp, type, is_noise,
		 content_type, tool_name, tool_use_id, tool_input, is_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, jsonb(?), ?)`
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return err
	}
	defer func() { stmt.Close() }()

	lockAcquired := time.Now()
	linesSinceLockCheck := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Extract session metadata from any entry.
		if entry.Cwd != "" && metaCwd == "" {
			metaCwd = entry.Cwd
		}
		if entry.GitBranch != "" && metaBranch == "" {
			metaBranch = entry.GitBranch
		}

		if entry.Type != "user" && entry.Type != "assistant" {
			// TODO: index system entries in a future pass.
			continue
		}

		blocks := extractBlocks(entry.Message)
		if len(blocks) == 0 {
			continue
		}

		ts := entry.Timestamp
		if ts == "" {
			ts = time.Now().Format(time.RFC3339)
		}

		for _, b := range blocks {
			noise := 0
			if b.ContentType == "text" && isNoise(b.Text) {
				noise = 1
			}
			// Capture first substantive user text message as topic.
			if metaTopic == "" && entry.Type == "user" && b.ContentType == "text" && noise == 0 && len(b.Text) >= 10 && !isBoilerplate(b.Text) {
				metaTopic = b.Text
				if len(metaTopic) > 200 {
					metaTopic = metaTopic[:197] + "..."
				}
			}

			// tool_input: pass raw JSON or nil.
			var toolInput any
			if b.ToolInput != nil {
				toolInput = string(b.ToolInput)
			}

			isErr := 0
			if b.IsError {
				isErr = 1
			}

			stmt.Exec(sessionID, project, entry.Type, b.Text, ts, entry.Type, noise,
				b.ContentType, b.ToolName, b.ToolUseID, toolInput, isErr)
			count++

			// Detect self-identification nonces.
			if b.ContentType == "text" && strings.HasPrefix(b.Text, NoncePrefix) {
				nonce := strings.TrimSpace(b.Text)
				tx.Exec("INSERT OR IGNORE INTO session_nonces (nonce, session_id) VALUES (?, ?)", nonce, sessionID)
			}
		}

		// Periodically yield the write lock so readers aren't starved.
		linesSinceLockCheck++
		if linesSinceLockCheck >= lineCheckInterval {
			linesSinceLockCheck = 0
			if time.Since(lockAcquired) >= yieldInterval {
				// Commit current transaction with offset update.
				curOffset, _ := f.Seek(0, 1)
				tx.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", path, curOffset)

				stmt.Close()
				if err := tx.Commit(); err != nil {
					return err
				}

				s.mu.Lock()
				s.offsets[path] = curOffset
				s.mu.Unlock()

				// Yield write lock to let readers through.
				s.rwmu.Unlock()
				s.rwmu.Lock()
				lockAcquired = time.Now()

				// Start a new transaction.
				tx, err = s.db.Begin()
				if err != nil {
					return err
				}
				stmt, err = tx.Prepare(insertSQL)
				if err != nil {
					return err
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

	newOffset, _ := f.Seek(0, 1)
	tx.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", path, newOffset)

	if err := tx.Commit(); err != nil {
		return err
	}

	s.mu.Lock()
	s.offsets[path] = newOffset
	s.mu.Unlock()

	if count > 0 {
		slog.Debug("ingested", "file", filepath.Base(path), "messages", count)
	}
	return nil
}
