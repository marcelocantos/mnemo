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
	"github.com/marcelocantos/sqldeep/go/sqldeep"
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

// StatusResult is the top-level response from Status.
type StatusResult struct {
	Days  int          `json:"days"`
	Repos []RepoStatus `json:"repos"`
}

// RepoStatus summarises recent activity for a single repo.
type RepoStatus struct {
	Repo         string          `json:"repo"`
	Path         string          `json:"path"`
	LastActivity string          `json:"last_activity"`
	Sessions     []SessionStatus `json:"sessions"`
}

// SessionStatus summarises a single session with conversation excerpts.
type SessionStatus struct {
	SessionID  string          `json:"session_id"`
	LastMsg    string          `json:"last_msg"`
	Messages   int             `json:"messages"`
	WorkType   string          `json:"work_type,omitempty"`
	Topic      string          `json:"topic,omitempty"`
	Excerpts   []MessageExcerpt `json:"excerpts"`
}

// MessageExcerpt is a possibly-truncated message with its database ID for drill-down.
type MessageExcerpt struct {
	ID        int    `json:"id"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	Truncated bool   `json:"truncated,omitempty"`
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

// UsageRow holds aggregated token usage for a single group (date, model, repo).
type UsageRow struct {
	Period              string  `json:"period"`
	Model               string  `json:"model,omitempty"`
	Repo                string  `json:"repo,omitempty"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	Messages            int     `json:"messages"`
	CostUSD             float64 `json:"cost_usd"`
}

// UsageResult holds aggregated token usage with totals.
type UsageResult struct {
	Days  int        `json:"days"`
	Rows  []UsageRow `json:"rows"`
	Total UsageRow   `json:"total"`
}

// modelCosts maps model slug prefixes to per-token costs in USD.
// Prices are per-million tokens; we store per-token for calculation.
var modelCosts = map[string]struct{ input, output, cacheRead, cacheWrite float64 }{
	"claude-opus-4":   {15.0 / 1e6, 75.0 / 1e6, 1.5 / 1e6, 18.75 / 1e6},
	"claude-sonnet-4": {3.0 / 1e6, 15.0 / 1e6, 0.3 / 1e6, 3.75 / 1e6},
	"claude-haiku-4":  {0.80 / 1e6, 4.0 / 1e6, 0.08 / 1e6, 1.0 / 1e6},
	"claude-3-5":      {3.0 / 1e6, 15.0 / 1e6, 0.3 / 1e6, 3.75 / 1e6},
}

func estimateCost(model string, input, output, cacheRead, cacheCreate int64) float64 {
	for prefix, cost := range modelCosts {
		if strings.HasPrefix(model, prefix) {
			return float64(input)*cost.input +
				float64(output)*cost.output +
				float64(cacheRead)*cost.cacheRead +
				float64(cacheCreate)*cost.cacheWrite
		}
	}
	// Fallback: use sonnet pricing as a reasonable middle ground.
	c := modelCosts["claude-sonnet-4"]
	return float64(input)*c.input + float64(output)*c.output +
		float64(cacheRead)*c.cacheRead + float64(cacheCreate)*c.cacheWrite
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

// fts5Operators matches explicit FTS5 syntax that should not be rewritten.
var fts5Operators = regexp.MustCompile(`(?i)\b(OR|NOT|AND|NEAR)\b|"`)

// relaxQuery rewrites a plain word list into an OR query so that partial
// matches surface instead of requiring every term. Queries that already
// contain explicit FTS5 operators (OR, NOT, AND, NEAR, quoted phrases)
// are returned unchanged.
func relaxQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return q
	}
	// If the query uses any explicit FTS5 operators, leave it alone.
	if fts5Operators.MatchString(q) {
		return q
	}
	words := strings.Fields(q)
	if len(words) <= 1 {
		return q
	}
	return strings.Join(words, " OR ")
}

// schemaVersion is incremented whenever the database schema changes.
// On mismatch the database file is deleted and rebuilt from transcripts.
const schemaVersion = 7

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
		CREATE TABLE IF NOT EXISTS entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			type TEXT NOT NULL,
			timestamp TEXT,
			raw BLOB,
			-- Virtual columns for high-query entry-level fields.
			model TEXT GENERATED ALWAYS AS (raw->>'$.message.model'),
			stop_reason TEXT GENERATED ALWAYS AS (raw->>'$.message.stop_reason'),
			input_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.input_tokens')),
			output_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.output_tokens')),
			cache_read_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_read_input_tokens')),
			cache_creation_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_creation_input_tokens')),
			agent_id TEXT GENERATED ALWAYS AS (raw->>'$.agentId'),
			version TEXT GENERATED ALWAYS AS (raw->>'$.version'),
			slug TEXT GENERATED ALWAYS AS (raw->>'$.slug'),
			is_sidechain INTEGER GENERATED ALWAYS AS (CASE WHEN json_extract(raw, '$.isSidechain') THEN 1 ELSE 0 END),
			data_type TEXT GENERATED ALWAYS AS (raw->>'$.data.type'),
			data_command TEXT GENERATED ALWAYS AS (raw->>'$.data.command'),
			data_hook_event TEXT GENERATED ALWAYS AS (raw->>'$.data.hookEvent'),
			top_tool_use_id TEXT GENERATED ALWAYS AS (raw->>'$.toolUseID'),
			parent_tool_use_id TEXT GENERATED ALWAYS AS (raw->>'$.parentToolUseID')
		);
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_id INTEGER REFERENCES entries(id),
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
		CREATE TABLE IF NOT EXISTS snapshot_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_id INTEGER NOT NULL REFERENCES entries(id),
			session_id TEXT NOT NULL,
			file_path TEXT NOT NULL,
			backup_time TEXT
		);
		CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project TEXT NOT NULL,
			file_path TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			memory_type TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS skills (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS claude_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL DEFAULT '',
			file_path TEXT NOT NULL UNIQUE,
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS audit_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL,
			date TEXT NOT NULL DEFAULT '',
			skill TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			raw_text TEXT NOT NULL,
			UNIQUE(file_path, date, skill)
		);
		CREATE TABLE IF NOT EXISTS targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL,
			target_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			weight REAL NOT NULL DEFAULT 0,
			description TEXT NOT NULL DEFAULT '',
			raw_text TEXT NOT NULL,
			UNIQUE(file_path, target_id)
		);
		CREATE TABLE IF NOT EXISTS plans (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL UNIQUE,
			phase TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	// Create indexes.
	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_entries_session ON entries(session_id);
		CREATE INDEX IF NOT EXISTS idx_entries_project ON entries(project);
		CREATE INDEX IF NOT EXISTS idx_entries_type ON entries(type);
		CREATE INDEX IF NOT EXISTS idx_entries_timestamp ON entries(timestamp);
		CREATE INDEX IF NOT EXISTS idx_entries_model ON entries(model) WHERE model IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_entries_agent_id ON entries(agent_id) WHERE agent_id IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_entries_data_type ON entries(data_type) WHERE data_type IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_entries_data_hook_event ON entries(data_hook_event) WHERE data_hook_event IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_entries_top_tool_use_id ON entries(top_tool_use_id) WHERE top_tool_use_id IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_entries_parent_tool_use_id ON entries(parent_tool_use_id) WHERE parent_tool_use_id IS NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
		CREATE INDEX IF NOT EXISTS idx_messages_project ON messages(project);
		CREATE INDEX IF NOT EXISTS idx_messages_entry_id ON messages(entry_id) WHERE entry_id IS NOT NULL;
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
		CREATE INDEX IF NOT EXISTS idx_snapshot_files_session ON snapshot_files(session_id);
		CREATE INDEX IF NOT EXISTS idx_snapshot_files_entry ON snapshot_files(entry_id);
		CREATE INDEX IF NOT EXISTS idx_snapshot_files_path ON snapshot_files(file_path);
		CREATE INDEX IF NOT EXISTS idx_memories_project ON memories(project);
		CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(memory_type);
		CREATE INDEX IF NOT EXISTS idx_skills_name ON skills(name);
		CREATE INDEX IF NOT EXISTS idx_claude_configs_repo ON claude_configs(repo);
		CREATE INDEX IF NOT EXISTS idx_audit_entries_repo ON audit_entries(repo);
		CREATE INDEX IF NOT EXISTS idx_audit_entries_date ON audit_entries(date);
		CREATE INDEX IF NOT EXISTS idx_audit_entries_skill ON audit_entries(skill);
		CREATE INDEX IF NOT EXISTS idx_targets_repo ON targets(repo);
		CREATE INDEX IF NOT EXISTS idx_targets_status ON targets(status);
		CREATE INDEX IF NOT EXISTS idx_targets_target_id ON targets(target_id);
		CREATE INDEX IF NOT EXISTS idx_plans_repo ON plans(repo);
		CREATE INDEX IF NOT EXISTS idx_plans_phase ON plans(phase);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create indexes: %w", err)
	}

	_, err = db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS snapshot_files_fts USING fts5(
			file_path,
			content=snapshot_files,
			content_rowid=id
		);
		CREATE VIRTUAL TABLE IF NOT EXISTS audit_entries_fts USING fts5(
			summary, raw_text, repo,
			content=audit_entries,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS audit_entries_ai;
		CREATE TRIGGER audit_entries_ai AFTER INSERT ON audit_entries
		BEGIN
			INSERT INTO audit_entries_fts(rowid, summary, raw_text, repo)
			VALUES (new.id, new.summary, new.raw_text, new.repo);
		END;
		DROP TRIGGER IF EXISTS audit_entries_au;
		CREATE TRIGGER audit_entries_au AFTER UPDATE ON audit_entries
		BEGIN
			INSERT INTO audit_entries_fts(audit_entries_fts, rowid, summary, raw_text, repo)
			VALUES ('delete', old.id, old.summary, old.raw_text, old.repo);
			INSERT INTO audit_entries_fts(rowid, summary, raw_text, repo)
			VALUES (new.id, new.summary, new.raw_text, new.repo);
		END;
		DROP TRIGGER IF EXISTS audit_entries_ad;
		CREATE TRIGGER audit_entries_ad AFTER DELETE ON audit_entries
		BEGIN
			INSERT INTO audit_entries_fts(audit_entries_fts, rowid, summary, raw_text, repo)
			VALUES ('delete', old.id, old.summary, old.raw_text, old.repo);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			name, description, content, project,
			content=memories,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS memories_ai;
		CREATE TRIGGER memories_ai AFTER INSERT ON memories
		BEGIN
			INSERT INTO memories_fts(rowid, name, description, content, project)
			VALUES (new.id, new.name, new.description, new.content, new.project);
		END;
		DROP TRIGGER IF EXISTS memories_au;
		CREATE TRIGGER memories_au AFTER UPDATE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, name, description, content, project)
			VALUES ('delete', old.id, old.name, old.description, old.content, old.project);
			INSERT INTO memories_fts(rowid, name, description, content, project)
			VALUES (new.id, new.name, new.description, new.content, new.project);
		END;
		DROP TRIGGER IF EXISTS memories_ad;
		CREATE TRIGGER memories_ad AFTER DELETE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, name, description, content, project)
			VALUES ('delete', old.id, old.name, old.description, old.content, old.project);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS skills_fts USING fts5(
			name, description, content,
			content=skills,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS skills_ai;
		CREATE TRIGGER skills_ai AFTER INSERT ON skills
		BEGIN
			INSERT INTO skills_fts(rowid, name, description, content)
			VALUES (new.id, new.name, new.description, new.content);
		END;
		DROP TRIGGER IF EXISTS skills_au;
		CREATE TRIGGER skills_au AFTER UPDATE ON skills
		BEGIN
			INSERT INTO skills_fts(skills_fts, rowid, name, description, content)
			VALUES ('delete', old.id, old.name, old.description, old.content);
			INSERT INTO skills_fts(rowid, name, description, content)
			VALUES (new.id, new.name, new.description, new.content);
		END;
		DROP TRIGGER IF EXISTS skills_ad;
		CREATE TRIGGER skills_ad AFTER DELETE ON skills
		BEGIN
			INSERT INTO skills_fts(skills_fts, rowid, name, description, content)
			VALUES ('delete', old.id, old.name, old.description, old.content);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS claude_configs_fts USING fts5(
			content, repo,
			content=claude_configs,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS claude_configs_ai;
		CREATE TRIGGER claude_configs_ai AFTER INSERT ON claude_configs
		BEGIN
			INSERT INTO claude_configs_fts(rowid, content, repo)
			VALUES (new.id, new.content, new.repo);
		END;
		DROP TRIGGER IF EXISTS claude_configs_au;
		CREATE TRIGGER claude_configs_au AFTER UPDATE ON claude_configs
		BEGIN
			INSERT INTO claude_configs_fts(claude_configs_fts, rowid, content, repo)
			VALUES ('delete', old.id, old.content, old.repo);
			INSERT INTO claude_configs_fts(rowid, content, repo)
			VALUES (new.id, new.content, new.repo);
		END;
		DROP TRIGGER IF EXISTS claude_configs_ad;
		CREATE TRIGGER claude_configs_ad AFTER DELETE ON claude_configs
		BEGIN
			INSERT INTO claude_configs_fts(claude_configs_fts, rowid, content, repo)
			VALUES ('delete', old.id, old.content, old.repo);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS targets_fts USING fts5(
			name, description, raw_text, repo,
			content=targets,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS targets_ai;
		CREATE TRIGGER targets_ai AFTER INSERT ON targets
		BEGIN
			INSERT INTO targets_fts(rowid, name, description, raw_text, repo)
			VALUES (new.id, new.name, new.description, new.raw_text, new.repo);
		END;
		DROP TRIGGER IF EXISTS targets_au;
		CREATE TRIGGER targets_au AFTER UPDATE ON targets
		BEGIN
			INSERT INTO targets_fts(targets_fts, rowid, name, description, raw_text, repo)
			VALUES ('delete', old.id, old.name, old.description, old.raw_text, old.repo);
			INSERT INTO targets_fts(rowid, name, description, raw_text, repo)
			VALUES (new.id, new.name, new.description, new.raw_text, new.repo);
		END;
		DROP TRIGGER IF EXISTS targets_ad;
		CREATE TRIGGER targets_ad AFTER DELETE ON targets
		BEGIN
			INSERT INTO targets_fts(targets_fts, rowid, name, description, raw_text, repo)
			VALUES ('delete', old.id, old.name, old.description, old.raw_text, old.repo);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS plans_fts USING fts5(
			content, repo, phase,
			content=plans,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS plans_ai;
		CREATE TRIGGER plans_ai AFTER INSERT ON plans
		BEGIN
			INSERT INTO plans_fts(rowid, content, repo, phase)
			VALUES (new.id, new.content, new.repo, new.phase);
		END;
		DROP TRIGGER IF EXISTS plans_au;
		CREATE TRIGGER plans_au AFTER UPDATE ON plans
		BEGIN
			INSERT INTO plans_fts(plans_fts, rowid, content, repo, phase)
			VALUES ('delete', old.id, old.content, old.repo, old.phase);
			INSERT INTO plans_fts(rowid, content, repo, phase)
			VALUES (new.id, new.content, new.repo, new.phase);
		END;
		DROP TRIGGER IF EXISTS plans_ad;
		CREATE TRIGGER plans_ad AFTER DELETE ON plans
		BEGIN
			INSERT INTO plans_fts(plans_fts, rowid, content, repo, phase)
			VALUES ('delete', old.id, old.content, old.repo, old.phase);
		END;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create snapshot_files FTS: %w", err)
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

	// Trigger to extract file paths from file-history-snapshot entries
	// and populate snapshot_files + its FTS index.
	_, err = db.Exec(`
		DROP TRIGGER IF EXISTS entries_file_snapshot;
		CREATE TRIGGER entries_file_snapshot AFTER INSERT ON entries
		WHEN new.type = 'file-history-snapshot'
		BEGIN
			INSERT INTO snapshot_files (entry_id, session_id, file_path, backup_time)
			SELECT new.id, new.session_id, f.key, f.value->>'backupTime'
			FROM json_each(new.raw, '$.snapshot.trackedFileBackups') f
			WHERE f.key != '';

			INSERT INTO snapshot_files_fts(rowid, file_path)
			SELECT sf.id, sf.file_path
			FROM snapshot_files sf
			WHERE sf.entry_id = new.id;
		END;
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create snapshot trigger: %w", err)
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

// MemoryInfo holds a single memory record from the index.
type MemoryInfo struct {
	ID          int    `json:"id"`
	Project     string `json:"project"`
	FilePath    string `json:"file_path"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MemoryType  string `json:"memory_type"`
	Content     string `json:"content"`
	UpdatedAt   string `json:"updated_at"`
}

// IngestMemories scans all memory directories under projectDir and ingests them.
func (s *Store) IngestMemories() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	memDirs, err := filepath.Glob(filepath.Join(s.projectDir, "*/memory"))
	if err != nil {
		return err
	}

	count := 0
	for _, dir := range memDirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			continue
		}
		project := filepath.Base(filepath.Dir(dir))
		for _, f := range files {
			if err := s.ingestMemoryFileLocked(f, project); err != nil {
				slog.Error("ingest memory failed", "file", f, "err", err)
				continue
			}
			count++
		}
	}
	slog.Info("ingested memories", "count", count)
	return nil
}

// ingestMemoryFile ingests a single memory file (acquires write lock).
func (s *Store) ingestMemoryFile(path string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	// Derive project from path: .../projects/<project>/memory/<file>.md
	dir := filepath.Dir(path)
	project := filepath.Base(filepath.Dir(dir))
	return s.ingestMemoryFileLocked(path, project)
}

// ingestMemoryFileLocked ingests a single memory file (caller must hold rwmu write lock).
func (s *Store) ingestMemoryFileLocked(path, project string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File was deleted — remove from index.
			s.db.Exec("DELETE FROM memories WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	content := string(data)
	name, description, memType, body := parseMemoryFrontmatter(content)
	now := time.Now().Format(time.RFC3339)

	// Use body (content after frontmatter) for the stored content,
	// but if there's no frontmatter, use the whole file.
	if body == "" {
		body = content
	}

	_, err = s.db.Exec(`
		INSERT INTO memories (project, file_path, name, description, memory_type, content, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			project = excluded.project,
			name = excluded.name,
			description = excluded.description,
			memory_type = excluded.memory_type,
			content = excluded.content,
			updated_at = excluded.updated_at
	`, project, path, name, description, memType, body, now)
	return err
}

// parseMemoryFrontmatter extracts YAML frontmatter fields from a memory file.
func parseMemoryFrontmatter(content string) (name, description, memType, body string) {
	if !strings.HasPrefix(content, "---\n") {
		return "", "", "", content
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return "", "", "", content
	}
	frontmatter := content[4 : 4+end]
	body = strings.TrimSpace(content[4+end+4:])

	for _, line := range strings.Split(frontmatter, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		switch key {
		case "name":
			name = val
		case "description":
			description = val
		case "type":
			memType = val
		}
	}
	return
}

// deleteMemoryFile removes a memory file from the index (acquires write lock).
func (s *Store) deleteMemoryFile(path string) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	s.db.Exec("DELETE FROM memories WHERE file_path = ?", path)
}

// SearchMemories searches across all indexed memory files.
func (s *Store) SearchMemories(query string, memType string, project string, limit int) ([]MemoryInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	if query == "" && memType == "" && project == "" {
		// List all memories.
		where := []string{"1=1"}
		var args []any
		if memType != "" {
			where = append(where, "memory_type = ?")
			args = append(args, memType)
		}
		if project != "" {
			where = append(where, "project LIKE ?")
			args = append(args, "%"+project+"%")
		}
		q := `SELECT id, project, file_path, name, description, memory_type, content, updated_at
			FROM memories WHERE ` + strings.Join(where, " AND ") + ` ORDER BY updated_at DESC LIMIT ?`
		args = append(args, limit)
		return s.queryMemories(q, args...)
	}

	if query != "" {
		ftsQuery := relaxQuery(query)
		// FTS search with optional filters.
		q := `SELECT m.id, m.project, m.file_path, m.name, m.description, m.memory_type, m.content, m.updated_at
			FROM memories m
			JOIN memories_fts f ON f.rowid = m.id
			WHERE memories_fts MATCH ?`
		args := []any{ftsQuery}
		if memType != "" {
			q += " AND m.memory_type = ?"
			args = append(args, memType)
		}
		if project != "" {
			q += " AND m.project LIKE ?"
			args = append(args, "%"+project+"%")
		}
		q += " ORDER BY rank LIMIT ?"
		args = append(args, limit)
		return s.queryMemories(q, args...)
	}

	// No query, just filters.
	where := []string{"1=1"}
	var args []any
	if memType != "" {
		where = append(where, "memory_type = ?")
		args = append(args, memType)
	}
	if project != "" {
		where = append(where, "project LIKE ?")
		args = append(args, "%"+project+"%")
	}
	q := `SELECT id, project, file_path, name, description, memory_type, content, updated_at
		FROM memories WHERE ` + strings.Join(where, " AND ") + ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	return s.queryMemories(q, args...)
}

func (s *Store) queryMemories(q string, args ...any) ([]MemoryInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MemoryInfo
	for rows.Next() {
		var m MemoryInfo
		if err := rows.Scan(&m.ID, &m.Project, &m.FilePath, &m.Name, &m.Description,
			&m.MemoryType, &m.Content, &m.UpdatedAt); err != nil {
			continue
		}
		results = append(results, m)
	}
	return results, nil
}

// SkillInfo holds a single skill record from the index.
type SkillInfo struct {
	ID          int    `json:"id"`
	FilePath    string `json:"file_path"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	UpdatedAt   string `json:"updated_at"`
}

// skillsDir returns the path to ~/.claude/skills/.
func skillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// IngestSkills scans ~/.claude/skills/*.md and ingests them.
func (s *Store) IngestSkills() error {
	dir, err := skillsDir()
	if err != nil {
		return err
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}

	count := 0
	for _, f := range files {
		if err := s.ingestSkillFileLocked(f); err != nil {
			slog.Error("ingest skill failed", "file", f, "err", err)
			continue
		}
		count++
	}
	slog.Info("ingested skills", "count", count)
	return nil
}

// ingestSkillFile ingests a single skill file (acquires write lock).
func (s *Store) ingestSkillFile(path string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	return s.ingestSkillFileLocked(path)
}

// ingestSkillFileLocked ingests a single skill file (caller must hold rwmu write lock).
func (s *Store) ingestSkillFileLocked(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.db.Exec("DELETE FROM skills WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	content := string(data)
	name, description, _, body := parseMemoryFrontmatter(content)

	// If no frontmatter, derive name from filename and use first non-blank line as description.
	if name == "" {
		base := filepath.Base(path)
		stem := strings.TrimSuffix(base, ".md")
		name = strings.NewReplacer("-", " ", "_", " ").Replace(stem)
	}
	if description == "" {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				description = line
				break
			}
		}
	}
	if body == "" {
		body = content
	}

	now := time.Now().Format(time.RFC3339)
	_, err = s.db.Exec(`
		INSERT INTO skills (file_path, name, description, content, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			name = excluded.name,
			description = excluded.description,
			content = excluded.content,
			updated_at = excluded.updated_at
	`, path, name, description, body, now)
	return err
}

// deleteSkillFile removes a skill file from the index (acquires write lock).
func (s *Store) deleteSkillFile(path string) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	s.db.Exec("DELETE FROM skills WHERE file_path = ?", path)
}

// SearchSkills searches across all indexed skill files.
func (s *Store) SearchSkills(query string, limit int) ([]SkillInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	if query == "" {
		return s.querySkills(`SELECT id, file_path, name, description, content, updated_at
			FROM skills ORDER BY name ASC LIMIT ?`, limit)
	}

	ftsQuery := relaxQuery(query)
	return s.querySkills(`SELECT s.id, s.file_path, s.name, s.description, s.content, s.updated_at
		FROM skills s
		JOIN skills_fts f ON f.rowid = s.id
		WHERE skills_fts MATCH ?
		ORDER BY rank LIMIT ?`, ftsQuery, limit)
}

func (s *Store) querySkills(q string, args ...any) ([]SkillInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SkillInfo
	for rows.Next() {
		var sk SkillInfo
		if err := rows.Scan(&sk.ID, &sk.FilePath, &sk.Name, &sk.Description, &sk.Content, &sk.UpdatedAt); err != nil {
			continue
		}
		results = append(results, sk)
	}
	return results, nil
}

// ClaudeConfigInfo holds a single CLAUDE.md record from the index.
type ClaudeConfigInfo struct {
	ID        int    `json:"id"`
	Repo      string `json:"repo"`
	FilePath  string `json:"file_path"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
}

// IngestClaudeConfigs scans all repo roots discovered via session_meta and ingests CLAUDE.md files.
// It also checks ~/.claude/CLAUDE.md and ~/CLAUDE.md.
func (s *Store) IngestClaudeConfigs() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	// Gather unique cwd values from session_meta.
	rows, err := s.db.Query("SELECT DISTINCT cwd FROM session_meta WHERE cwd != ''")
	if err != nil {
		return fmt.Errorf("query session_meta: %w", err)
	}
	var cwds []string
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err == nil && cwd != "" {
			cwds = append(cwds, cwd)
		}
	}
	rows.Close()

	// Find repo roots for each cwd by walking up to a .git directory.
	seen := map[string]bool{}
	for _, cwd := range cwds {
		root := findRepoRoot(cwd)
		if root != "" && !seen[root] {
			seen[root] = true
			claudePath := filepath.Join(root, "CLAUDE.md")
			repo := extractRepo(cwd)
			if err := s.ingestClaudeConfigFileLocked(claudePath, repo); err != nil && !os.IsNotExist(err) {
				slog.Error("ingest claude config failed", "file", claudePath, "err", err)
			}
		}
	}

	// Also check ~/.claude/CLAUDE.md and ~/CLAUDE.md.
	homeDir, err := os.UserHomeDir()
	if err == nil {
		for _, extra := range []struct{ path, repo string }{
			{filepath.Join(homeDir, ".claude", "CLAUDE.md"), "global"},
			{filepath.Join(homeDir, "CLAUDE.md"), "home"},
		} {
			if !seen[extra.path] {
				seen[extra.path] = true
				if err := s.ingestClaudeConfigFileLocked(extra.path, extra.repo); err != nil && !os.IsNotExist(err) {
					slog.Error("ingest claude config failed", "file", extra.path, "err", err)
				}
			}
		}
	}

	slog.Info("ingested claude configs", "repos", len(seen))
	return nil
}

// findRepoRoot walks up from dir to find the nearest directory containing .git.
// Returns "" if no .git ancestor is found.
func findRepoRoot(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// ingestClaudeConfigFileLocked ingests a single CLAUDE.md file (caller must hold rwmu write lock).
func (s *Store) ingestClaudeConfigFileLocked(path, repo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.db.Exec("DELETE FROM claude_configs WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	content := string(data)
	now := time.Now().Format(time.RFC3339)

	_, err = s.db.Exec(`
		INSERT INTO claude_configs (repo, file_path, content, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			repo = excluded.repo,
			content = excluded.content,
			updated_at = excluded.updated_at
	`, repo, path, content, now)
	return err
}

// SearchClaudeConfigs searches across all indexed CLAUDE.md files.
func (s *Store) SearchClaudeConfigs(query string, repo string, limit int) ([]ClaudeConfigInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	var q string
	var args []any

	if query != "" {
		ftsQuery := relaxQuery(query)
		q = `SELECT c.id, c.repo, c.file_path, c.content, c.updated_at
			FROM claude_configs c
			JOIN claude_configs_fts f ON f.rowid = c.id
			WHERE claude_configs_fts MATCH ?`
		args = []any{ftsQuery}
		if repo != "" {
			q += " AND c.repo LIKE ?"
			args = append(args, "%"+repo+"%")
		}
		q += " ORDER BY rank LIMIT ?"
		args = append(args, limit)
	} else {
		q = `SELECT id, repo, file_path, content, updated_at FROM claude_configs`
		if repo != "" {
			q += " WHERE repo LIKE ?"
			args = append(args, "%"+repo+"%")
		}
		q += " ORDER BY updated_at DESC LIMIT ?"
		args = append(args, limit)
	}

	return s.queryClaudeConfigs(q, args...)
}

func (s *Store) queryClaudeConfigs(q string, args ...any) ([]ClaudeConfigInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []ClaudeConfigInfo
	for rows.Next() {
		var c ClaudeConfigInfo
		if err := rows.Scan(&c.ID, &c.Repo, &c.FilePath, &c.Content, &c.UpdatedAt); err != nil {
			continue
		}
		results = append(results, c)
	}
	return results, nil
}


// AuditEntryInfo holds a single audit log entry from the index.
type AuditEntryInfo struct {
	ID       int    `json:"id"`
	Repo     string `json:"repo"`
	FilePath string `json:"file_path"`
	Date     string `json:"date"`
	Skill    string `json:"skill"`
	Version  string `json:"version"`
	Summary  string `json:"summary"`
	RawText  string `json:"raw_text"`
}

// TargetInfo holds a single convergence target from a docs/targets.md file.
type TargetInfo struct {
	ID          int     `json:"id"`
	Repo        string  `json:"repo"`
	FilePath    string  `json:"file_path"`
	TargetID    string  `json:"target_id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Weight      float64 `json:"weight"`
	Description string  `json:"description"`
	RawText     string  `json:"raw_text"`
}

// PlanInfo holds a single indexed plan file.
type PlanInfo struct {
	ID        int    `json:"id"`
	Repo      string `json:"repo"`
	FilePath  string `json:"file_path"`
	Phase     string `json:"phase"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
}

// versionPattern matches version strings like v1.2.3.
var versionPattern = regexp.MustCompile(`v\d+\.\d+\.\d+`)

// parseAuditLogEntries parses a docs/audit-log.md file into individual entries.
// Each ## heading starts a new entry. The first word after ## is the date;
// skill follows a — or / separator; version matches vN.N.N.
func parseAuditLogEntries(content string) []AuditEntryInfo {
	var entries []AuditEntryInfo
	lines := strings.Split(content, "\n")

	var current *AuditEntryInfo
	var rawLines []string

	flush := func() {
		if current != nil {
			current.RawText = strings.TrimSpace(strings.Join(rawLines, "\n"))
			// Use the heading line as summary if no body lines.
			if current.Summary == "" {
				current.Summary = current.RawText
			}
			entries = append(entries, *current)
			current = nil
			rawLines = nil
		}
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			flush()
			heading := strings.TrimPrefix(line, "## ")
			entry := AuditEntryInfo{}

			// Parse date: first word.
			fields := strings.Fields(heading)
			if len(fields) > 0 {
				entry.Date = fields[0]
			}

			// Parse skill: look for — or / after the date.
			// Format: "DATE — /skill vN.N.N" or "DATE — skill description"
			rest := strings.TrimSpace(strings.TrimPrefix(heading, entry.Date))
			rest = strings.TrimPrefix(rest, "—")
			rest = strings.TrimSpace(rest)
			// Strip leading slash if present (skill name indicator).
			if strings.HasPrefix(rest, "/") {
				rest = rest[1:]
				// First word after / is the skill.
				skillFields := strings.Fields(rest)
				if len(skillFields) > 0 {
					entry.Skill = skillFields[0]
				}
			} else if rest != "" {
				// No slash — use first word as skill.
				skillFields := strings.Fields(rest)
				if len(skillFields) > 0 {
					entry.Skill = skillFields[0]
				}
			}

			// Parse version: find vN.N.N in the heading.
			if m := versionPattern.FindString(heading); m != "" {
				entry.Version = m
			}

			// Use the full heading (after ##) as the initial summary.
			entry.Summary = heading

			current = &entry
			rawLines = []string{line}
		} else if current != nil {
			rawLines = append(rawLines, line)
		}
	}
	flush()
	return entries
}

// IngestAuditLogs scans all repo roots discovered from session_meta and
// ingests any docs/audit-log.md files found. Audit logs change rarely so
// startup-only ingest is sufficient; no file watcher is set up.
func (s *Store) IngestAuditLogs() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	// Collect distinct cwd values from session metadata.
	rows, err := s.db.Query("SELECT DISTINCT cwd FROM session_meta WHERE cwd != ''")
	if err != nil {
		return err
	}
	cwds := make([]string, 0)
	for rows.Next() {
		var cwd string
		if rows.Scan(&cwd) == nil {
			cwds = append(cwds, cwd)
		}
	}
	rows.Close()

	// Walk up each cwd to find the repo root (contains .git).
	seen := make(map[string]bool)
	count := 0
	for _, cwd := range cwds {
		root := findRepoRoot(cwd)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true

		auditPath := filepath.Join(root, "docs", "audit-log.md")
		if _, err := os.Stat(auditPath); os.IsNotExist(err) {
			continue
		}

		// Derive repo name from the root path (last two path components).
		repo := repoNameFromPath(root)
		if err := s.ingestAuditLogFileLocked(auditPath, repo); err != nil {
			slog.Error("ingest audit log failed", "file", auditPath, "err", err)
			continue
		}
		count++
	}
	slog.Info("ingested audit logs", "count", count)
	return nil
}

// repoNameFromPath returns a "org/repo" style name from a filesystem path,
// taking the last two non-empty path components.
func repoNameFromPath(path string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	// Remove empty segments.
	var nonempty []string
	for _, p := range parts {
		if p != "" {
			nonempty = append(nonempty, p)
		}
	}
	if len(nonempty) >= 2 {
		return nonempty[len(nonempty)-2] + "/" + nonempty[len(nonempty)-1]
	}
	if len(nonempty) == 1 {
		return nonempty[0]
	}
	return path
}

// ingestAuditLogFile ingests a single audit log file (acquires write lock).
func (s *Store) ingestAuditLogFile(path, repo string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	return s.ingestAuditLogFileLocked(path, repo)
}

// ingestAuditLogFileLocked ingests a single audit log file (caller must hold rwmu write lock).
func (s *Store) ingestAuditLogFileLocked(path, repo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = s.db.Exec("DELETE FROM audit_entries WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	entries := parseAuditLogEntries(string(data))

	// Full replace: delete existing entries for this file, then insert fresh.
	if _, err := s.db.Exec("DELETE FROM audit_entries WHERE file_path = ?", path); err != nil {
		return fmt.Errorf("delete old audit entries: %w", err)
	}

	for _, e := range entries {
		_, err := s.db.Exec(`
			INSERT OR IGNORE INTO audit_entries (repo, file_path, date, skill, version, summary, raw_text)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, repo, path, e.Date, e.Skill, e.Version, e.Summary, e.RawText)
		if err != nil {
			slog.Error("insert audit entry failed", "file", path, "date", e.Date, "err", err)
		}
	}
	return nil
}

// SearchAuditLogs searches across all indexed audit log entries.
func (s *Store) SearchAuditLogs(query string, repo string, skill string, limit int) ([]AuditEntryInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	if query != "" {
		ftsQuery := relaxQuery(query)
		q := `SELECT ae.id, ae.repo, ae.file_path, ae.date, ae.skill, ae.version, ae.summary, ae.raw_text
			FROM audit_entries ae
			JOIN audit_entries_fts f ON f.rowid = ae.id
			WHERE audit_entries_fts MATCH ?`
		args := []any{ftsQuery}
		if repo != "" {
			q += " AND ae.repo LIKE ?"
			args = append(args, "%"+repo+"%")
		}
		if skill != "" {
			q += " AND ae.skill = ?"
			args = append(args, skill)
		}
		q += " ORDER BY rank LIMIT ?"
		args = append(args, limit)
		return s.queryAuditEntries(q, args...)
	}

	// No query — list with optional filters.
	where := []string{"1=1"}
	var args []any
	if repo != "" {
		where = append(where, "repo LIKE ?")
		args = append(args, "%"+repo+"%")
	}
	if skill != "" {
		where = append(where, "skill = ?")
		args = append(args, skill)
	}
	q := `SELECT id, repo, file_path, date, skill, version, summary, raw_text
		FROM audit_entries WHERE ` + strings.Join(where, " AND ") + ` ORDER BY date DESC LIMIT ?`
	args = append(args, limit)
	return s.queryAuditEntries(q, args...)
}

func (s *Store) queryAuditEntries(q string, args ...any) ([]AuditEntryInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []AuditEntryInfo
	for rows.Next() {
		var e AuditEntryInfo
		if err := rows.Scan(&e.ID, &e.Repo, &e.FilePath, &e.Date, &e.Skill,
			&e.Version, &e.Summary, &e.RawText); err != nil {
			continue
		}
		results = append(results, e)
	}
	return results, nil
}

// targetHeading matches a ### 🎯TN or ### 🎯TN.N heading line.
var targetHeading = regexp.MustCompile(`^### (🎯T[\d]+(?:\.[\d]+)*)\s*(.*)$`)

// parseTargetsFile parses a docs/targets.md file and returns all targets found.
func parseTargetsFile(repo, filePath string, data []byte) []TargetInfo {
	lines := strings.Split(string(data), "\n")
	var targets []TargetInfo
	var cur *TargetInfo
	var rawLines []string

	flush := func() {
		if cur == nil {
			return
		}
		cur.RawText = strings.TrimSpace(strings.Join(rawLines, "\n"))
		targets = append(targets, *cur)
		cur = nil
		rawLines = nil
	}

	for _, line := range lines {
		if m := targetHeading.FindStringSubmatch(line); m != nil {
			flush()
			cur = &TargetInfo{
				Repo:     repo,
				FilePath: filePath,
				TargetID: m[1],
				Name:     strings.TrimSpace(m[2]),
			}
			rawLines = []string{line}
			continue
		}
		if cur != nil {
			// Check for next ### heading (non-target) to end the block.
			if strings.HasPrefix(line, "### ") {
				flush()
				continue
			}
			rawLines = append(rawLines, line)
			// Parse metadata lines.
			if strings.HasPrefix(line, "- **Status**:") {
				cur.Status = strings.TrimSpace(strings.TrimPrefix(line, "- **Status**:"))
			} else if strings.HasPrefix(line, "- **Weight**:") {
				wStr := strings.TrimSpace(strings.TrimPrefix(line, "- **Weight**:"))
				var w float64
				fmt.Sscanf(wStr, "%f", &w)
				cur.Weight = w
			}
		}
	}
	flush()

	// Extract description: first non-empty, non-metadata paragraph after the metadata lines.
	for i := range targets {
		t := &targets[i]
		bodyLines := strings.Split(t.RawText, "\n")
		// Skip the heading line and metadata lines.
		inMeta := true
		var descLines []string
		for _, l := range bodyLines[1:] {
			trimmed := strings.TrimSpace(l)
			if inMeta {
				if strings.HasPrefix(trimmed, "- **") || trimmed == "" {
					continue
				}
				inMeta = false
			}
			if trimmed == "" && len(descLines) > 0 {
				break
			}
			if trimmed != "" {
				descLines = append(descLines, trimmed)
			}
		}
		t.Description = strings.Join(descLines, " ")
	}

	return targets
}

// IngestTargets scans all repos known from session_meta and ingests their
// docs/targets.md files. This is run at startup only; no realtime watching.
func (s *Store) IngestTargets() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	// Collect unique cwds from session_meta.
	rows, err := s.db.Query("SELECT DISTINCT cwd FROM session_meta WHERE cwd != ''")
	if err != nil {
		return fmt.Errorf("query cwds: %w", err)
	}
	var cwds []string
	for rows.Next() {
		var cwd string
		if rows.Scan(&cwd) == nil && cwd != "" {
			cwds = append(cwds, cwd)
		}
	}
	rows.Close()

	// Deduplicate repo roots.
	seen := map[string]bool{}
	count := 0
	for _, cwd := range cwds {
		root := findRepoRoot(cwd)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true

		targetsPath := filepath.Join(root, "docs", "targets.md")
		data, err := os.ReadFile(targetsPath)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("cannot read targets.md", "path", targetsPath, "err", err)
			}
			continue
		}

		// Derive repo name from root path.
		repo := filepath.Base(root)

		parsed := parseTargetsFile(repo, targetsPath, data)
		if len(parsed) == 0 {
			continue
		}

		// Delete existing targets for this file and re-insert.
		if _, err := s.db.Exec("DELETE FROM targets WHERE file_path = ?", targetsPath); err != nil {
			slog.Warn("delete targets failed", "path", targetsPath, "err", err)
			continue
		}
		for _, t := range parsed {
			_, err := s.db.Exec(`
				INSERT INTO targets (repo, file_path, target_id, name, status, weight, description, raw_text)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(file_path, target_id) DO UPDATE SET
					repo = excluded.repo,
					name = excluded.name,
					status = excluded.status,
					weight = excluded.weight,
					description = excluded.description,
					raw_text = excluded.raw_text
			`, t.Repo, t.FilePath, t.TargetID, t.Name, t.Status, t.Weight, t.Description, t.RawText)
			if err != nil {
				slog.Warn("insert target failed", "target_id", t.TargetID, "err", err)
				continue
			}
			count++
		}
	}
	slog.Info("ingested targets", "count", count)
	return nil
}

// SearchTargets searches across indexed convergence targets.
func (s *Store) SearchTargets(query string, repo string, status string, limit int) ([]TargetInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	var q string
	var args []any

	if query != "" {
		ftsQuery := relaxQuery(query)
		q = `SELECT t.id, t.repo, t.file_path, t.target_id, t.name, t.status, t.weight, t.description, t.raw_text
			FROM targets t
			JOIN targets_fts f ON f.rowid = t.id
			WHERE targets_fts MATCH ?`
		args = []any{ftsQuery}
		if repo != "" {
			q += " AND t.repo LIKE ?"
			args = append(args, "%"+repo+"%")
		}
		if status != "" {
			q += " AND t.status = ?"
			args = append(args, status)
		}
		q += " ORDER BY rank LIMIT ?"
		args = append(args, limit)
	} else {
		where := []string{"1=1"}
		if repo != "" {
			where = append(where, "repo LIKE ?")
			args = append(args, "%"+repo+"%")
		}
		if status != "" {
			where = append(where, "status = ?")
			args = append(args, status)
		}
		q = `SELECT id, repo, file_path, target_id, name, status, weight, description, raw_text
			FROM targets WHERE ` + strings.Join(where, " AND ") + ` ORDER BY weight DESC, target_id LIMIT ?`
		args = append(args, limit)
	}

	return s.queryTargets(q, args...)
}

func (s *Store) queryTargets(q string, args ...any) ([]TargetInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []TargetInfo
	for rows.Next() {
		var t TargetInfo
		if err := rows.Scan(&t.ID, &t.Repo, &t.FilePath, &t.TargetID, &t.Name,
			&t.Status, &t.Weight, &t.Description, &t.RawText); err != nil {
			continue
		}
		results = append(results, t)
	}
	return results, nil
}

// IngestPlans scans .planning/ directories in all known repos and indexes them.
// Plans change during active GSD work but are read-heavy, so startup-only
// ingestion is sufficient — no realtime watch is registered.
func (s *Store) IngestPlans() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	// Collect unique repo roots from all known cwds.
	rows, err := s.db.Query("SELECT DISTINCT cwd FROM session_meta WHERE cwd != ''")
	if err != nil {
		return err
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			continue
		}
		root := findRepoRoot(cwd)
		if root != "" && !seen[root] {
			seen[root] = true
		}
	}
	rows.Close()

	count := 0
	for root := range seen {
		planningDir := filepath.Join(root, ".planning")
		if _, err := os.Stat(planningDir); os.IsNotExist(err) {
			continue
		}
		repo := extractRepo(root)

		// Walk all .md files under .planning/
		if err := filepath.Walk(planningDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.ToLower(filepath.Ext(path)) != ".md" {
				return nil
			}
			if err2 := s.ingestPlanFileLocked(path, repo, planningDir); err2 != nil {
				slog.Error("ingest plan failed", "file", path, "err", err2)
			} else {
				count++
			}
			return nil
		}); err != nil {
			slog.Error("walk planning dir failed", "dir", planningDir, "err", err)
		}
	}
	slog.Info("ingested plans", "count", count)
	return nil
}

// ingestPlanFileLocked ingests a single plan file (caller must hold rwmu write lock).
func (s *Store) ingestPlanFileLocked(path, repo, planningDir string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.db.Exec("DELETE FROM plans WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	// Derive phase from the relative path under .planning/.
	// e.g. .planning/phase-3/PLAN.md → "3"
	//      .planning/milestone-v2/phase-1/PLAN.md → "v2/1"
	//      .planning/PLAN.md → ""
	rel, err := filepath.Rel(planningDir, path)
	if err != nil {
		rel = ""
	}
	phase := extractPlanPhase(rel)

	now := time.Now().Format(time.RFC3339)
	_, err = s.db.Exec(`
		INSERT INTO plans (repo, file_path, phase, content, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			repo = excluded.repo,
			phase = excluded.phase,
			content = excluded.content,
			updated_at = excluded.updated_at
	`, repo, path, phase, string(data), now)
	return err
}

// extractPlanPhase derives a short phase identifier from a path relative to .planning/.
// Examples:
//
//	"PLAN.md"                        → ""
//	"phase-3/PLAN.md"                → "3"
//	"milestone-v2/phase-1/PLAN.md"   → "v2/1"
//	"codebase/overview.md"           → "codebase"
func extractPlanPhase(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	// Drop the filename component.
	if len(parts) <= 1 {
		return ""
	}
	dirs := parts[:len(parts)-1]

	var segments []string
	for _, d := range dirs {
		// Strip common prefixes: "phase-", "milestone-" to get the identifier.
		lower := strings.ToLower(d)
		for _, prefix := range []string{"phase-", "milestone-"} {
			if strings.HasPrefix(lower, prefix) {
				d = d[len(prefix):]
				break
			}
		}
		segments = append(segments, d)
	}
	return strings.Join(segments, "/")
}

// SearchPlans searches across all indexed plan files.
func (s *Store) SearchPlans(query string, repo string, limit int) ([]PlanInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	if query == "" {
		q := `SELECT id, repo, file_path, phase, content, updated_at FROM plans`
		var args []any
		if repo != "" {
			q += ` WHERE repo LIKE ?`
			args = append(args, "%"+repo+"%")
		}
		q += ` ORDER BY updated_at DESC LIMIT ?`
		args = append(args, limit)
		return s.queryPlans(q, args...)
	}

	ftsQuery := relaxQuery(query)
	q := `SELECT p.id, p.repo, p.file_path, p.phase, p.content, p.updated_at
		FROM plans p
		JOIN plans_fts f ON f.rowid = p.id
		WHERE plans_fts MATCH ?`
	args := []any{ftsQuery}
	if repo != "" {
		q += ` AND p.repo LIKE ?`
		args = append(args, "%"+repo+"%")
	}
	q += ` ORDER BY rank LIMIT ?`
	args = append(args, limit)
	return s.queryPlans(q, args...)
}

func (s *Store) queryPlans(q string, args ...any) ([]PlanInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []PlanInfo
	for rows.Next() {
		var p PlanInfo
		if err := rows.Scan(&p.ID, &p.Repo, &p.FilePath, &p.Phase, &p.Content, &p.UpdatedAt); err != nil {
			continue
		}
		results = append(results, p)
	}
	return results, nil
}

// parsedMessage is a single content block ready for insertion.
type parsedMessage struct {
	entryIdx    int // index into parsedFile.entries
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

// parsedRawEntry is a raw JSONL line ready for insertion into the entries table.
type parsedRawEntry struct {
	entryType string
	timestamp string
	raw       []byte // full JSONL line
}

// parsedFile is the result of parsing a single JSONL file.
type parsedFile struct {
	path      string
	sessionID string
	project   string
	entries   []parsedRawEntry
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
	if err := s.runWriter(parsedCh); err != nil {
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

const (
	entryInsertSQL = `INSERT INTO entries
		(session_id, project, type, timestamp, raw)
		VALUES (?, ?, ?, ?, jsonb(?))`
	messageInsertSQL = `INSERT INTO messages
		(entry_id, session_id, project, role, text, timestamp, type, is_noise,
		 content_type, tool_name, tool_use_id, tool_input, is_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, jsonb(?), ?)`
)

// writerState holds prepared statements for the two-table insert.
type writerState struct {
	tx       *sql.Tx
	entryStmt *sql.Stmt
	msgStmt   *sql.Stmt
}

func newWriterState(db *sql.DB) (*writerState, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	entryStmt, err := tx.Prepare(entryInsertSQL)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	msgStmt, err := tx.Prepare(messageInsertSQL)
	if err != nil {
		entryStmt.Close()
		tx.Rollback()
		return nil, err
	}
	return &writerState{tx: tx, entryStmt: entryStmt, msgStmt: msgStmt}, nil
}

func (ws *writerState) Close() {
	ws.entryStmt.Close()
	ws.msgStmt.Close()
}

// runWriter is the single-goroutine writer for the parallel ingest pipeline.
// It consumes parsed files from the channel and inserts them in batched
// transactions, yielding the write lock every 200ms for readers.
func (s *Store) runWriter(parsedCh <-chan parsedFile) error {
	const commitInterval = 200 * time.Millisecond

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	ws, err := newWriterState(s.db)
	if err != nil {
		return err
	}
	defer func() { ws.tx.Rollback() }()
	defer ws.Close()

	lastCommit := time.Now()

	commitBatch := func() error {
		ws.Close()
		if err := ws.tx.Commit(); err != nil {
			return err
		}
		// Yield write lock for readers.
		s.rwmu.Unlock()
		s.rwmu.Lock()
		ws, err = newWriterState(s.db)
		if err != nil {
			return err
		}
		lastCommit = time.Now()
		return nil
	}

	for pf := range parsedCh {
		// Insert all raw entries and build entryIdx→entryID map.
		entryIDs := make(map[int]int64, len(pf.entries))
		for i, e := range pf.entries {
			result, err := ws.entryStmt.Exec(pf.sessionID, pf.project, e.entryType, e.timestamp, string(e.raw))
			if err != nil {
				slog.Warn("entry insert failed", "session", pf.sessionID, "err", err)
				continue
			}
			if id, err := result.LastInsertId(); err == nil {
				entryIDs[i] = id
			}
		}

		// Insert content block messages linked to their entries.
		for _, m := range pf.messages {
			var toolInput any
			if m.toolInput != nil {
				toolInput = string(m.toolInput)
			}
			entryID := entryIDs[m.entryIdx]
			ws.msgStmt.Exec(entryID, pf.sessionID, pf.project, m.role, m.text, m.timestamp, m.typ, m.isNoise,
				m.contentType, m.toolName, m.toolUseID, toolInput, m.isError)

			// Detect self-identification nonces.
			if m.contentType == "text" && strings.HasPrefix(m.text, NoncePrefix) {
				ws.tx.Exec("INSERT OR IGNORE INTO session_nonces (nonce, session_id) VALUES (?, ?)",
					strings.TrimSpace(m.text), pf.sessionID)
			}
		}

		// Upsert session metadata.
		if pf.cwd != "" || pf.branch != "" || pf.topic != "" {
			repo := extractRepo(pf.cwd)
			workType := classifyWorkType(pf.branch)
			ws.tx.Exec(`INSERT INTO session_meta (session_id, repo, cwd, git_branch, work_type, topic)
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
		ws.tx.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", pf.path, pf.newOffset)
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
	ws.Close()
	return ws.tx.Commit()
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

		ts := entry.Timestamp
		if ts == "" {
			ts = time.Now().Format(time.RFC3339)
		}

		// Store every JSONL line in the entries table.
		rawCopy := make([]byte, len(line))
		copy(rawCopy, line)
		entryIdx := len(pf.entries)
		pf.entries = append(pf.entries, parsedRawEntry{
			entryType: entry.Type,
			timestamp: ts,
			raw:       rawCopy,
		})

		// Only extract content blocks for user/assistant messages.
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}

		blocks := extractBlocks(entry.Message)
		if len(blocks) == 0 {
			continue
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
				entryIdx:    entryIdx,
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

	// Also watch the skills directory for .md changes.
	if sdir, err := skillsDir(); err == nil {
		if wErr := watcher.Add(sdir); wErr != nil {
			slog.Warn("failed to watch skills directory", "path", sdir, "err", wErr)
		}
	}

	slog.Info("watching for transcript changes", "dir", s.projectDir)
	// NOTE: Realtime watching of repo CLAUDE.md files is deferred.
	// CLAUDE.md files live in repo roots (not under projectDir), and they change
	// rarely enough that restart-based refresh (via IngestClaudeConfigs at startup)
	// is acceptable for now.

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
			// Watch memory file changes.
			if strings.HasSuffix(event.Name, ".md") && strings.Contains(event.Name, "/memory/") {
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					if err := s.ingestMemoryFile(event.Name); err != nil {
						slog.Error("ingest memory failed", "file", event.Name, "err", err)
					}
				}
				if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					s.deleteMemoryFile(event.Name)
				}
			}
			// Watch skill file changes.
			if strings.HasSuffix(event.Name, ".md") && strings.Contains(event.Name, "/.claude/skills/") {
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					if err := s.ingestSkillFile(event.Name); err != nil {
						slog.Error("ingest skill failed", "file", event.Name, "err", err)
					}
				}
				if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					s.deleteSkillFile(event.Name)
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

	// Rewrite plain word lists to OR queries so partial matches surface.
	// Explicit FTS5 operators (OR, NOT, AND, NEAR, quotes) are preserved.
	ftsQuery := relaxQuery(query)

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
	`, ftsQuery, fetchLimit)
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

// Usage returns aggregated token usage statistics from the entries table.
// groupBy can be "day" (default), "model", or "repo".
func (s *Store) Usage(days int, repoFilter, model, groupBy string) (*UsageResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if days <= 0 {
		days = 30
	}
	if groupBy == "" {
		groupBy = "day"
	}

	// Build GROUP BY expression.
	var groupExpr, periodExpr string
	switch groupBy {
	case "model":
		groupExpr = "e.model"
		periodExpr = "e.model"
	case "repo":
		groupExpr = "CASE WHEN sm.repo != '' THEN sm.repo ELSE sm.cwd END"
		periodExpr = groupExpr
	default: // "day"
		groupExpr = "date(e.timestamp)"
		periodExpr = "date(e.timestamp)"
	}

	where := []string{
		"e.type = 'assistant'",
		"e.timestamp >= datetime('now', ?)",
	}
	args := []any{fmt.Sprintf("-%d days", days)}

	if repoFilter != "" {
		where = append(where, "(sm.repo LIKE ? OR sm.cwd LIKE ?)")
		pattern := "%" + repoFilter + "%"
		args = append(args, pattern, pattern)
	}
	if model != "" {
		where = append(where, "e.model LIKE ?")
		args = append(args, model+"%")
	}

	needJoin := repoFilter != "" || groupBy == "repo"
	joinClause := ""
	if needJoin {
		joinClause = "LEFT JOIN session_meta sm ON sm.session_id = e.session_id"
	}

	// Always group by model too, so cost estimation is accurate.
	// Re-aggregate in Go when the requested groupBy isn't "model".
	q := fmt.Sprintf(`
		SELECT
			%s AS period,
			COALESCE(e.model, '') AS model,
			COALESCE(SUM(e.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(e.output_tokens), 0) AS output_tokens,
			COALESCE(SUM(e.cache_read_tokens), 0) AS cache_read_tokens,
			COALESCE(SUM(e.cache_creation_tokens), 0) AS cache_creation_tokens,
			COUNT(*) AS messages
		FROM entries e
		%s
		WHERE %s
		GROUP BY %s, e.model
		ORDER BY period DESC
	`, periodExpr, joinClause, strings.Join(where, " AND "), groupExpr)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Accumulate per-(period, model) rows, computing accurate costs.
	merged := map[string]*UsageRow{} // period → aggregated row
	var order []string

	result := &UsageResult{Days: days}
	for rows.Next() {
		var period, rowModel string
		var input, output, cacheRead, cacheCreate int64
		var msgs int
		if err := rows.Scan(&period, &rowModel, &input, &output,
			&cacheRead, &cacheCreate, &msgs); err != nil {
			continue
		}
		cost := estimateCost(rowModel, input, output, cacheRead, cacheCreate)

		if groupBy == "model" {
			// Each model is its own row — no merging needed.
			result.Rows = append(result.Rows, UsageRow{
				Period: period, Model: period,
				InputTokens: input, OutputTokens: output,
				CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreate,
				Messages: msgs, CostUSD: cost,
			})
		} else {
			r, ok := merged[period]
			if !ok {
				r = &UsageRow{Period: period}
				if groupBy == "repo" {
					r.Repo = period
				}
				merged[period] = r
				order = append(order, period)
			}
			r.InputTokens += input
			r.OutputTokens += output
			r.CacheReadTokens += cacheRead
			r.CacheCreationTokens += cacheCreate
			r.Messages += msgs
			r.CostUSD += cost
		}

		result.Total.InputTokens += input
		result.Total.OutputTokens += output
		result.Total.CacheReadTokens += cacheRead
		result.Total.CacheCreationTokens += cacheCreate
		result.Total.Messages += msgs
		result.Total.CostUSD += cost
	}

	if groupBy != "model" {
		for _, k := range order {
			result.Rows = append(result.Rows, *merged[k])
		}
	}
	result.Total.Period = "total"
	return result, nil
}

// Status returns a rich status report: repos → sessions → truncated message excerpts.
func (s *Store) Status(days int, repoFilter string, maxSessions int, maxExcerpts int, truncateLen int) (*StatusResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if days <= 0 {
		days = 7
	}
	if maxSessions <= 0 {
		maxSessions = 3
	}
	if maxExcerpts <= 0 {
		maxExcerpts = 20
	}
	if truncateLen <= 0 {
		truncateLen = 200
	}

	// Step 1: Find repos with recent activity.
	repoWhere := []string{
		"ss.session_type = 'interactive'",
		"ss.last_msg >= datetime('now', ?)",
	}
	repoArgs := []any{fmt.Sprintf("-%d days", days)}

	if repoFilter != "" {
		repoWhere = append(repoWhere, "(sm.repo LIKE ? OR sm.cwd LIKE ?)")
		pattern := "%" + repoFilter + "%"
		repoArgs = append(repoArgs, pattern, pattern)
	}

	repoQ := `
		SELECT
			CASE WHEN sm.repo != '' THEN sm.repo ELSE sm.cwd END AS display_repo,
			MAX(sm.cwd) AS path,
			MAX(ss.last_msg) AS last_activity
		FROM session_summary ss
		JOIN session_meta sm ON sm.session_id = ss.session_id
		WHERE ` + strings.Join(repoWhere, " AND ") + `
		GROUP BY display_repo
		HAVING display_repo != ''
		ORDER BY last_activity DESC
	`

	repoRows, err := s.db.Query(repoQ, repoArgs...)
	if err != nil {
		return nil, err
	}
	defer repoRows.Close()

	var repos []RepoStatus
	for repoRows.Next() {
		var r RepoStatus
		if err := repoRows.Scan(&r.Repo, &r.Path, &r.LastActivity); err != nil {
			continue
		}
		repos = append(repos, r)
	}

	// Step 2: For each repo, find recent sessions.
	for i := range repos {
		sessQ := `
			SELECT ss.session_id, ss.last_msg, ss.substantive_msgs,
				COALESCE(sm.work_type, ''), COALESCE(sm.topic, '')
			FROM session_summary ss
			JOIN session_meta sm ON sm.session_id = ss.session_id
			WHERE ss.session_type = 'interactive'
				AND ss.last_msg >= datetime('now', ?)
				AND (sm.repo = ? OR sm.cwd = ?)
			ORDER BY ss.last_msg DESC
			LIMIT ?
		`
		sessRows, err := s.db.Query(sessQ,
			fmt.Sprintf("-%d days", days), repos[i].Repo, repos[i].Path, maxSessions)
		if err != nil {
			continue
		}

		for sessRows.Next() {
			var ss SessionStatus
			if err := sessRows.Scan(&ss.SessionID, &ss.LastMsg, &ss.Messages,
				&ss.WorkType, &ss.Topic); err != nil {
				continue
			}
			repos[i].Sessions = append(repos[i].Sessions, ss)
		}
		sessRows.Close()

		// Step 3: For each session, pull substantive messages.
		for j := range repos[i].Sessions {
			sid := repos[i].Sessions[j].SessionID
			msgQ := `
				SELECT id, role, text, timestamp FROM messages
				WHERE session_id = ? AND is_noise = 0
					AND role IN ('user', 'assistant')
				ORDER BY id ASC
			`
			msgRows, err := s.db.Query(msgQ, sid)
			if err != nil {
				continue
			}

			var excerpts []MessageExcerpt
			for msgRows.Next() {
				var m MessageExcerpt
				if err := msgRows.Scan(&m.ID, &m.Role, &m.Text, &m.Timestamp); err != nil {
					continue
				}
				// Truncate assistant messages.
				if m.Role == "assistant" && len(m.Text) > truncateLen {
					m.Text = m.Text[:truncateLen] + "..."
					m.Truncated = true
				}
				excerpts = append(excerpts, m)
			}
			msgRows.Close()

			// Keep last N excerpts if there are too many.
			if len(excerpts) > maxExcerpts {
				excerpts = excerpts[len(excerpts)-maxExcerpts:]
			}
			repos[i].Sessions[j].Excerpts = excerpts
		}
	}

	return &StatusResult{Days: days, Repos: repos}, nil
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

// isSqldeep returns true if the query uses sqldeep nested syntax.
func isSqldeep(upper string) bool {
	return strings.HasPrefix(upper, "FROM") || strings.Contains(upper, "SELECT {")
}

// Query runs a read-only SQL query and returns rows as maps.
// Accepts both plain SQL (SELECT/WITH) and sqldeep nested syntax
// (FROM ... SELECT { ... }). sqldeep queries are transparently
// transpiled to SQL before execution.
func (s *Store) Query(query string) ([]map[string]any, error) {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)

	execSQL := query
	if isSqldeep(upper) {
		sql, err := sqldeep.Transpile(q)
		if err != nil {
			return nil, fmt.Errorf("sqldeep transpile: %w", err)
		}
		execSQL = sql
	} else if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return nil, fmt.Errorf("only SELECT, WITH, and sqldeep (FROM ... SELECT { }) queries are allowed")
	}

	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	rows, err := s.db.Query(execSQL)
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

	ws, err := newWriterState(s.db)
	if err != nil {
		return err
	}
	defer func() { ws.tx.Rollback() }()
	defer ws.Close()

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

		ts := entry.Timestamp
		if ts == "" {
			ts = time.Now().Format(time.RFC3339)
		}

		// Insert every JSONL line into entries table.
		var entryID int64
		result, entryErr := ws.entryStmt.Exec(sessionID, project, entry.Type, ts, string(line))
		if entryErr == nil {
			entryID, _ = result.LastInsertId()
		}

		// Only extract content blocks for user/assistant messages.
		if entry.Type != "user" && entry.Type != "assistant" {
			goto yieldCheck
		}

		{
			blocks := extractBlocks(entry.Message)
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

				ws.msgStmt.Exec(entryID, sessionID, project, entry.Type, b.Text, ts, entry.Type, noise,
					b.ContentType, b.ToolName, b.ToolUseID, toolInput, isErr)
				count++

				// Detect self-identification nonces.
				if b.ContentType == "text" && strings.HasPrefix(b.Text, NoncePrefix) {
					nonce := strings.TrimSpace(b.Text)
					ws.tx.Exec("INSERT OR IGNORE INTO session_nonces (nonce, session_id) VALUES (?, ?)", nonce, sessionID)
				}
			}
		}

	yieldCheck:
		// Periodically yield the write lock so readers aren't starved.
		linesSinceLockCheck++
		if linesSinceLockCheck >= lineCheckInterval {
			linesSinceLockCheck = 0
			if time.Since(lockAcquired) >= yieldInterval {
				// Commit current transaction with offset update.
				curOffset, _ := f.Seek(0, 1)
				ws.tx.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", path, curOffset)

				ws.Close()
				if err := ws.tx.Commit(); err != nil {
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
				ws, err = newWriterState(s.db)
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
		ws.tx.Exec(`INSERT INTO session_meta (session_id, repo, cwd, git_branch, work_type, topic)
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
	ws.tx.Exec("INSERT OR REPLACE INTO ingest_state (path, offset) VALUES (?, ?)", path, newOffset)

	ws.Close()
	if err := ws.tx.Commit(); err != nil {
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
