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
	"os/exec"
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

	// workspaceRoots is the set of filesystem roots under which repo-level
	// streams discover repos. Mutated only via SetWorkspaceRoots, read
	// under rwmu.RLock by the repoRoots walker.
	workspaceRoots []string

	// extraProjectDirs are additional Claude Code project directories
	// to ingest beyond projectDir. Used for cross-platform transcript
	// ingest (🎯T15) — e.g. a Windows VM's Claude projects exposed via
	// SMB mount. Mutated only via SetExtraProjectDirs.
	extraProjectDirs []string

	// liveness cache
	liveMu        sync.Mutex
	liveCache     map[string]int // sessionID → PID
	liveCacheTime time.Time

	// imageSem caps the total number of image-sidecar goroutines
	// (OCR + description + embedding, across all images) running at
	// once. Sized at runtime.NumCPU(). A burst of images fans out
	// goroutines freely; the semaphore absorbs them without overrunning
	// the machine with concurrent claude-p / Python subprocesses.
	imageSem chan struct{}
}

// SetWorkspaceRoots configures the filesystem roots under which repo-
// level ingest streams discover repositories. Call this once after
// Store.New and before any Ingest* call. Tests inject a temp directory;
// production loads from ~/.mnemo/config.json.
func (s *Store) SetWorkspaceRoots(roots []string) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	// Copy to detach from caller slice.
	if len(roots) == 0 {
		s.workspaceRoots = nil
		return
	}
	s.workspaceRoots = append(s.workspaceRoots[:0:0], roots...)
}

// SetExtraProjectDirs configures additional Claude Code project
// directories beyond the primary projectDir. These are walked at
// IngestAll and Watch time alongside the primary dir. Missing or
// unavailable extras (e.g. an unmounted SMB share) are skipped with
// a warn rather than failing. Call once after Store.New.
func (s *Store) SetExtraProjectDirs(dirs []string) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	if len(dirs) == 0 {
		s.extraProjectDirs = nil
		return
	}
	s.extraProjectDirs = append(s.extraProjectDirs[:0:0], dirs...)
}

// projectDirs returns the full list of project directories to scan:
// the primary projectDir followed by any extras configured via
// SetExtraProjectDirs. The returned slice is a defensive copy.
func (s *Store) projectDirs() []string {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	dirs := make([]string, 0, 1+len(s.extraProjectDirs))
	dirs = append(dirs, s.projectDir)
	dirs = append(dirs, s.extraProjectDirs...)
	return dirs
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
	Days    int              `json:"days"`
	Repos   []RepoStatus     `json:"repos"`
	Streams []BackfillStatus `json:"streams,omitempty"`
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
	SessionID string           `json:"session_id"`
	LastMsg   string           `json:"last_msg"`
	Messages  int              `json:"messages"`
	WorkType  string           `json:"work_type,omitempty"`
	Topic     string           `json:"topic,omitempty"`
	Excerpts  []MessageExcerpt `json:"excerpts"`
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
	TotalSessions int              `json:"total_sessions"`
	TotalMessages int              `json:"total_messages"`
	ByType        []TypeStats      `json:"by_type"`
	Streams       []BackfillStatus `json:"streams,omitempty"`
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

// HourlyRate shows token and cost velocity over the queried period.
type HourlyRate struct {
	ActiveHours     float64 `json:"active_hours"`
	InputPerHour    float64 `json:"input_per_hour"`
	OutputPerHour   float64 `json:"output_per_hour"`
	CostPerHour     float64 `json:"cost_per_hour"`
	MessagesPerHour float64 `json:"messages_per_hour"`
}

// WhatsupTranscript holds metadata about a candidate transcript file for a live session.
type WhatsupTranscript struct {
	Path  string    `json:"path"`
	MTime time.Time `json:"mtime"`
	Size  int64     `json:"size"`
}

// WhatsupSession holds per-session process metrics alongside session metadata.
type WhatsupSession struct {
	SessionID   string              `json:"session_id"`
	PID         int                 `json:"pid"`
	Cwd         string              `json:"cwd,omitempty"`
	Transcripts []WhatsupTranscript `json:"transcripts,omitempty"`
	Repo        string              `json:"repo,omitempty"`
	Topic       string              `json:"topic,omitempty"`
	WorkType    string              `json:"work_type,omitempty"`
	CPUPct      float64             `json:"cpu_pct"`
	RSSBytes    int64               `json:"rss_bytes"`
	CPUTime     string              `json:"cpu_time"`
}

// WhatsupPostmortemEntry is a cwd that had recent claude activity but no live process.
type WhatsupPostmortemEntry struct {
	Cwd         string              `json:"cwd"`
	Transcripts []WhatsupTranscript `json:"transcripts"`
}

// SystemMetrics holds system-wide resource metrics.
type SystemMetrics struct {
	MemPagesFree     int64   `json:"mem_pages_free"`
	MemPagesActive   int64   `json:"mem_pages_active"`
	MemPagesInactive int64   `json:"mem_pages_inactive"`
	MemPagesWired    int64   `json:"mem_pages_wired"`
	MemPressurePct   float64 `json:"mem_pressure_pct"` // (active+wired)/(active+inactive+wired+free)
}

// WhatsupResult holds the combined per-session and system metrics.
type WhatsupResult struct {
	Sessions   []WhatsupSession         `json:"sessions"`
	Postmortem []WhatsupPostmortemEntry `json:"postmortem,omitempty"`
	System     SystemMetrics            `json:"system"`
}

// QueryTemplate is a named, parameterised query template stored in the database.
type QueryTemplate struct {
	ID          int      `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	QueryText   string   `json:"query_text"`
	ParamNames  []string `json:"param_names"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// UsageResult holds aggregated token usage with totals.
type UsageResult struct {
	Days       int         `json:"days"`
	Rows       []UsageRow  `json:"rows"`
	Total      UsageRow    `json:"total"`
	HourlyRate *HourlyRate `json:"hourly_rate,omitempty"`
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
const schemaVersion = 20

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
		CREATE TABLE IF NOT EXISTS ingest_status (
			stream TEXT PRIMARY KEY,
			last_backfill TEXT NOT NULL,
			files_indexed INTEGER NOT NULL,
			files_on_disk INTEGER NOT NULL
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
		CREATE TABLE IF NOT EXISTS ci_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			run_id INTEGER NOT NULL UNIQUE,
			workflow TEXT NOT NULL,
			branch TEXT,
			commit_sha TEXT,
			status TEXT NOT NULL,
			conclusion TEXT,
			started_at TEXT,
			completed_at TEXT,
			log_summary TEXT,
			url TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS session_chains (
			successor_id TEXT PRIMARY KEY,
			predecessor_id TEXT NOT NULL,
			boundary TEXT NOT NULL DEFAULT 'clear',
			gap_ms INTEGER NOT NULL,
			confidence TEXT NOT NULL CHECK(confidence IN ('high', 'medium')),
			mechanism TEXT NOT NULL DEFAULT 'time_heuristic',
			detected_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			proposal_msg_id INTEGER REFERENCES messages(id),
			confirmation_msg_id INTEGER REFERENCES messages(id),
			proposal_text TEXT NOT NULL,
			confirmation_text TEXT NOT NULL,
			repo TEXT NOT NULL DEFAULT '',
			timestamp TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS compactions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			connection_id TEXT,
			generated_at TEXT NOT NULL DEFAULT (datetime('now')),
			model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			entry_id_from INTEGER NOT NULL DEFAULT 0,
			entry_id_to INTEGER NOT NULL DEFAULT 0,
			payload_json TEXT NOT NULL DEFAULT '{}',
			summary TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS daemon_connections (
			connection_id TEXT PRIMARY KEY,
			pid INTEGER NOT NULL,
			accepted_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			closed_at TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_daemon_connections_pid ON daemon_connections(pid);
		CREATE INDEX IF NOT EXISTS idx_daemon_connections_open ON daemon_connections(closed_at) WHERE closed_at IS NULL;
		CREATE TABLE IF NOT EXISTS connection_sessions (
			connection_id TEXT NOT NULL,
			session_id   TEXT NOT NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			PRIMARY KEY (connection_id, session_id)
		);
		CREATE INDEX IF NOT EXISTS idx_connection_sessions_session ON connection_sessions(session_id);
		CREATE INDEX IF NOT EXISTS idx_connection_sessions_connection_last ON connection_sessions(connection_id, last_seen_at DESC);
		CREATE TABLE IF NOT EXISTS query_templates (
			id INTEGER PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			description TEXT,
			query_text TEXT NOT NULL,
			param_names TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS git_commits (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			commit_hash TEXT NOT NULL,
			author_name TEXT NOT NULL,
			author_email TEXT NOT NULL,
			commit_date TEXT NOT NULL,
			subject TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			UNIQUE(repo, commit_hash)
		);
		CREATE TABLE IF NOT EXISTS github_prs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			author TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			merged_at TEXT,
			url TEXT NOT NULL,
			UNIQUE(repo, pr_number)
		);
		CREATE TABLE IF NOT EXISTS github_issues (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			author TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			url TEXT NOT NULL,
			labels TEXT NOT NULL DEFAULT '[]',
			UNIQUE(repo, issue_number)
		);
		CREATE TABLE IF NOT EXISTS images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content_hash TEXT UNIQUE NOT NULL,
			bytes BLOB NOT NULL,
			original_path TEXT,
			mime_type TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			pixel_format TEXT NOT NULL,
			byte_size INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS image_occurrences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			entry_id INTEGER REFERENCES entries(id),
			message_id INTEGER REFERENCES messages(id),
			session_id TEXT NOT NULL,
			source_type TEXT NOT NULL CHECK(source_type IN ('inline','path')),
			occurred_at TEXT NOT NULL,
			UNIQUE(image_id, entry_id, message_id, source_type)
		);
		CREATE TABLE IF NOT EXISTS image_descriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(image_id)
		);
		CREATE TABLE IF NOT EXISTS image_ocr (
			image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
			text TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL,
			confidence REAL,
			error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS image_embeddings (
			image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			dim INTEGER NOT NULL,
			vector BLOB NOT NULL,
			error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS docs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL DEFAULT 'md',
			title TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			mtime TEXT NOT NULL DEFAULT '',
			indexed_at TEXT NOT NULL
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
		CREATE INDEX IF NOT EXISTS idx_ci_runs_repo ON ci_runs(repo);
		CREATE INDEX IF NOT EXISTS idx_ci_runs_status ON ci_runs(status);
		CREATE INDEX IF NOT EXISTS idx_ci_runs_conclusion ON ci_runs(conclusion);
		CREATE INDEX IF NOT EXISTS idx_ci_runs_started ON ci_runs(started_at);
		CREATE INDEX IF NOT EXISTS idx_session_chains_predecessor ON session_chains(predecessor_id);
		CREATE INDEX IF NOT EXISTS idx_decisions_session ON decisions(session_id);
		CREATE INDEX IF NOT EXISTS idx_decisions_repo ON decisions(repo);
		CREATE INDEX IF NOT EXISTS idx_decisions_timestamp ON decisions(timestamp);
		CREATE INDEX IF NOT EXISTS idx_compactions_session_generated ON compactions(session_id, generated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_git_commits_repo ON git_commits(repo);
		CREATE INDEX IF NOT EXISTS idx_git_commits_date ON git_commits(commit_date);
		CREATE INDEX IF NOT EXISTS idx_git_commits_hash ON git_commits(commit_hash);
		CREATE INDEX IF NOT EXISTS idx_github_prs_repo ON github_prs(repo);
		CREATE INDEX IF NOT EXISTS idx_github_prs_state ON github_prs(state);
		CREATE INDEX IF NOT EXISTS idx_github_prs_created ON github_prs(created_at);
		CREATE INDEX IF NOT EXISTS idx_github_prs_updated ON github_prs(updated_at);
		CREATE INDEX IF NOT EXISTS idx_github_issues_repo ON github_issues(repo);
		CREATE INDEX IF NOT EXISTS idx_github_issues_state ON github_issues(state);
		CREATE INDEX IF NOT EXISTS idx_github_issues_created ON github_issues(created_at);
		CREATE INDEX IF NOT EXISTS idx_github_issues_updated ON github_issues(updated_at);
		CREATE INDEX IF NOT EXISTS idx_images_content_hash ON images(content_hash);
		CREATE INDEX IF NOT EXISTS idx_image_occurrences_session ON image_occurrences(session_id);
		CREATE INDEX IF NOT EXISTS idx_image_occurrences_image ON image_occurrences(image_id);
		CREATE INDEX IF NOT EXISTS idx_image_descriptions_image ON image_descriptions(image_id);
		CREATE INDEX IF NOT EXISTS idx_image_ocr_backend ON image_ocr(backend);
		CREATE INDEX IF NOT EXISTS idx_image_embeddings_model ON image_embeddings(model);
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
		CREATE VIRTUAL TABLE IF NOT EXISTS ci_runs_fts USING fts5(
			repo, workflow, branch, log_summary, conclusion,
			content='ci_runs', content_rowid='id'
		);
		DROP TRIGGER IF EXISTS ci_runs_ai;
		CREATE TRIGGER ci_runs_ai AFTER INSERT ON ci_runs
		BEGIN
			INSERT INTO ci_runs_fts(rowid, repo, workflow, branch, log_summary, conclusion)
			VALUES (new.id, new.repo, new.workflow, COALESCE(new.branch, ''), COALESCE(new.log_summary, ''), COALESCE(new.conclusion, ''));
		END;
		DROP TRIGGER IF EXISTS ci_runs_au;
		CREATE TRIGGER ci_runs_au AFTER UPDATE ON ci_runs
		BEGIN
			INSERT INTO ci_runs_fts(ci_runs_fts, rowid, repo, workflow, branch, log_summary, conclusion)
			VALUES ('delete', old.id, old.repo, old.workflow, COALESCE(old.branch, ''), COALESCE(old.log_summary, ''), COALESCE(old.conclusion, ''));
			INSERT INTO ci_runs_fts(rowid, repo, workflow, branch, log_summary, conclusion)
			VALUES (new.id, new.repo, new.workflow, COALESCE(new.branch, ''), COALESCE(new.log_summary, ''), COALESCE(new.conclusion, ''));
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS decisions_fts USING fts5(
			proposal_text, confirmation_text, repo,
			content=decisions,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS decisions_ai;
		CREATE TRIGGER decisions_ai AFTER INSERT ON decisions
		BEGIN
			INSERT INTO decisions_fts(rowid, proposal_text, confirmation_text, repo)
			VALUES (new.id, new.proposal_text, new.confirmation_text, new.repo);
		END;
		DROP TRIGGER IF EXISTS decisions_au;
		CREATE TRIGGER decisions_au AFTER UPDATE ON decisions
		BEGIN
			INSERT INTO decisions_fts(decisions_fts, rowid, proposal_text, confirmation_text, repo)
			VALUES ('delete', old.id, old.proposal_text, old.confirmation_text, old.repo);
			INSERT INTO decisions_fts(rowid, proposal_text, confirmation_text, repo)
			VALUES (new.id, new.proposal_text, new.confirmation_text, new.repo);
		END;
		DROP TRIGGER IF EXISTS decisions_ad;
		CREATE TRIGGER decisions_ad AFTER DELETE ON decisions
		BEGIN
			INSERT INTO decisions_fts(decisions_fts, rowid, proposal_text, confirmation_text, repo)
			VALUES ('delete', old.id, old.proposal_text, old.confirmation_text, old.repo);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS git_commits_fts USING fts5(
			subject, body, repo, author_name,
			content=git_commits, content_rowid=id
		);
		DROP TRIGGER IF EXISTS git_commits_ai;
		CREATE TRIGGER git_commits_ai AFTER INSERT ON git_commits
		BEGIN
			INSERT INTO git_commits_fts(rowid, subject, body, repo, author_name)
			VALUES (new.id, new.subject, new.body, new.repo, new.author_name);
		END;
		DROP TRIGGER IF EXISTS git_commits_au;
		CREATE TRIGGER git_commits_au AFTER UPDATE ON git_commits
		BEGIN
			INSERT INTO git_commits_fts(git_commits_fts, rowid, subject, body, repo, author_name)
			VALUES ('delete', old.id, old.subject, old.body, old.repo, old.author_name);
			INSERT INTO git_commits_fts(rowid, subject, body, repo, author_name)
			VALUES (new.id, new.subject, new.body, new.repo, new.author_name);
		END;
		DROP TRIGGER IF EXISTS git_commits_ad;
		CREATE TRIGGER git_commits_ad AFTER DELETE ON git_commits
		BEGIN
			INSERT INTO git_commits_fts(git_commits_fts, rowid, subject, body, repo, author_name)
			VALUES ('delete', old.id, old.subject, old.body, old.repo, old.author_name);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS github_prs_fts USING fts5(
			title, body, repo, author,
			content=github_prs, content_rowid=id
		);
		DROP TRIGGER IF EXISTS github_prs_ai;
		CREATE TRIGGER github_prs_ai AFTER INSERT ON github_prs
		BEGIN
			INSERT INTO github_prs_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;
		DROP TRIGGER IF EXISTS github_prs_au;
		CREATE TRIGGER github_prs_au AFTER UPDATE ON github_prs
		BEGIN
			INSERT INTO github_prs_fts(github_prs_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
			INSERT INTO github_prs_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;
		DROP TRIGGER IF EXISTS github_prs_ad;
		CREATE TRIGGER github_prs_ad AFTER DELETE ON github_prs
		BEGIN
			INSERT INTO github_prs_fts(github_prs_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS github_issues_fts USING fts5(
			title, body, repo, author,
			content=github_issues, content_rowid=id
		);
		DROP TRIGGER IF EXISTS github_issues_ai;
		CREATE TRIGGER github_issues_ai AFTER INSERT ON github_issues
		BEGIN
			INSERT INTO github_issues_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;
		DROP TRIGGER IF EXISTS github_issues_au;
		CREATE TRIGGER github_issues_au AFTER UPDATE ON github_issues
		BEGIN
			INSERT INTO github_issues_fts(github_issues_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
			INSERT INTO github_issues_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;
		DROP TRIGGER IF EXISTS github_issues_ad;
		CREATE TRIGGER github_issues_ad AFTER DELETE ON github_issues
		BEGIN
			INSERT INTO github_issues_fts(github_issues_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS image_descriptions_fts USING fts5(
			name,
			description,
			content=image_descriptions,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS image_descriptions_ai;
		CREATE TRIGGER image_descriptions_ai AFTER INSERT ON image_descriptions
		BEGIN
			INSERT INTO image_descriptions_fts(rowid, name, description)
			VALUES (new.id, new.name, new.description);
		END;
		DROP TRIGGER IF EXISTS image_descriptions_au;
		CREATE TRIGGER image_descriptions_au AFTER UPDATE ON image_descriptions
		BEGIN
			INSERT INTO image_descriptions_fts(image_descriptions_fts, rowid, name, description)
			VALUES ('delete', old.id, old.name, old.description);
			INSERT INTO image_descriptions_fts(rowid, name, description)
			VALUES (new.id, new.name, new.description);
		END;
		DROP TRIGGER IF EXISTS image_descriptions_ad;
		CREATE TRIGGER image_descriptions_ad AFTER DELETE ON image_descriptions
		BEGIN
			INSERT INTO image_descriptions_fts(image_descriptions_fts, rowid, name, description)
			VALUES ('delete', old.id, old.name, old.description);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS image_ocr_fts USING fts5(
			text,
			content=image_ocr,
			content_rowid=image_id
		);
		DROP TRIGGER IF EXISTS image_ocr_ai;
		CREATE TRIGGER image_ocr_ai AFTER INSERT ON image_ocr
		BEGIN
			INSERT INTO image_ocr_fts(rowid, text)
			VALUES (new.image_id, new.text);
		END;
		DROP TRIGGER IF EXISTS image_ocr_au;
		CREATE TRIGGER image_ocr_au AFTER UPDATE ON image_ocr
		BEGIN
			INSERT INTO image_ocr_fts(image_ocr_fts, rowid, text)
			VALUES ('delete', old.image_id, old.text);
			INSERT INTO image_ocr_fts(rowid, text)
			VALUES (new.image_id, new.text);
		END;
		DROP TRIGGER IF EXISTS image_ocr_ad;
		CREATE TRIGGER image_ocr_ad AFTER DELETE ON image_ocr
		BEGIN
			INSERT INTO image_ocr_fts(image_ocr_fts, rowid, text)
			VALUES ('delete', old.image_id, old.text);
		END;
		CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
			title, content, repo, kind,
			content=docs,
			content_rowid=id
		);
		DROP TRIGGER IF EXISTS docs_ai;
		CREATE TRIGGER docs_ai AFTER INSERT ON docs
		BEGIN
			INSERT INTO docs_fts(rowid, title, content, repo, kind)
			VALUES (new.id, new.title, new.content, new.repo, new.kind);
		END;
		DROP TRIGGER IF EXISTS docs_au;
		CREATE TRIGGER docs_au AFTER UPDATE ON docs
		BEGIN
			INSERT INTO docs_fts(docs_fts, rowid, title, content, repo, kind)
			VALUES ('delete', old.id, old.title, old.content, old.repo, old.kind);
			INSERT INTO docs_fts(rowid, title, content, repo, kind)
			VALUES (new.id, new.title, new.content, new.repo, new.kind);
		END;
		DROP TRIGGER IF EXISTS docs_ad;
		CREATE TRIGGER docs_ad AFTER DELETE ON docs
		BEGIN
			INSERT INTO docs_fts(docs_fts, rowid, title, content, repo, kind)
			VALUES ('delete', old.id, old.title, old.content, old.repo, old.kind);
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

	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	s := &Store{
		db:         db,
		projectDir: projectDir,
		offsets:    make(map[string]int64),
		imageSem:   make(chan struct{}, n),
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

// IngestClaudeConfigs discovers every repo under the configured
// workspace roots (and session_meta) and ingests its CLAUDE.md file.
// Also checks ~/.claude/CLAUDE.md and ~/CLAUDE.md.
func (s *Store) IngestClaudeConfigs() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	roots := s.knownRepoRootsLocked()
	indexed, onDisk := 0, 0
	for _, rr := range roots {
		claudePath := filepath.Join(rr.root, "CLAUDE.md")
		if _, err := os.Stat(claudePath); err != nil {
			continue
		}
		onDisk++
		if err := s.ingestClaudeConfigFileLocked(claudePath, rr.repo); err != nil && !os.IsNotExist(err) {
			slog.Error("ingest claude config failed", "file", claudePath, "err", err)
			continue
		}
		indexed++
	}

	// Also check ~/.claude/CLAUDE.md and ~/CLAUDE.md.
	if homeDir, err := os.UserHomeDir(); err == nil {
		for _, extra := range []struct{ path, repo string }{
			{filepath.Join(homeDir, ".claude", "CLAUDE.md"), "global"},
			{filepath.Join(homeDir, "CLAUDE.md"), "home"},
		} {
			if _, err := os.Stat(extra.path); err != nil {
				continue
			}
			onDisk++
			if err := s.ingestClaudeConfigFileLocked(extra.path, extra.repo); err != nil && !os.IsNotExist(err) {
				slog.Error("ingest claude config failed", "file", extra.path, "err", err)
				continue
			}
			indexed++
		}
	}

	s.recordBackfillStatusLocked("claude_configs", indexed, onDisk)
	slog.Info("ingested claude configs", "indexed", indexed, "on_disk", onDisk)
	return nil
}

type repoRoot struct {
	root string
	repo string
}

// BackfillStatus summarises the most recent backfill pass for a single
// repo-level stream. files_on_disk counts artefacts discovered on disk
// across all workspace roots; files_indexed counts how many of those
// actually landed in the index. A non-zero drift (on_disk - indexed)
// indicates partial coverage — typically an unreadable file, a parse
// error, or an empty source.
type BackfillStatus struct {
	Stream       string `json:"stream"`
	LastBackfill string `json:"last_backfill"`
	FilesIndexed int    `json:"files_indexed"`
	FilesOnDisk  int    `json:"files_on_disk"`
}

// Drift returns files_on_disk - files_indexed. Zero means full coverage.
func (b BackfillStatus) Drift() int { return b.FilesOnDisk - b.FilesIndexed }

// recordBackfillStatusLocked upserts a row into ingest_status. Caller
// must hold s.rwmu.Lock.
func (s *Store) recordBackfillStatusLocked(stream string, indexed, onDisk int) {
	_, err := s.db.Exec(`
		INSERT INTO ingest_status (stream, last_backfill, files_indexed, files_on_disk)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(stream) DO UPDATE SET
			last_backfill = excluded.last_backfill,
			files_indexed = excluded.files_indexed,
			files_on_disk = excluded.files_on_disk
	`, stream, time.Now().UTC().Format(time.RFC3339), indexed, onDisk)
	if err != nil {
		slog.Warn("record backfill status failed", "stream", stream, "err", err)
	}
}

// BackfillStatuses returns the latest backfill status for every
// repo-level stream, ordered by stream name. Streams that have never
// run a backfill are omitted.
func (s *Store) BackfillStatuses() []BackfillStatus {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	rows, err := s.db.Query(`
		SELECT stream, last_backfill, files_indexed, files_on_disk
		FROM ingest_status
		ORDER BY stream
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []BackfillStatus
	for rows.Next() {
		var b BackfillStatus
		if err := rows.Scan(&b.Stream, &b.LastBackfill, &b.FilesIndexed, &b.FilesOnDisk); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// knownRepoRoots returns deduplicated repo roots from the union of
// (a) the configured workspace roots walked for .git entries, and
// (b) session_meta.cwd resolved via findRepoRoot. Either source alone
// would be incomplete: workspace-walk misses repos that live outside
// the configured roots, session_meta misses repos that haven't been
// touched by a Claude Code session. The union self-heals both.
//
// This is the single choke point for repo-level ingest discovery —
// every repo-level stream (targets, audit logs, plans, CLAUDE.md, CI
// watchers) flows through here, so extending coverage cascades to
// every stream at once.
func (s *Store) knownRepoRoots() []repoRoot {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	return s.knownRepoRootsLocked()
}

// knownRepoRootsLocked is the shared implementation. Caller must hold
// s.rwmu for read or write.
func (s *Store) knownRepoRootsLocked() []repoRoot {
	seen := map[string]bool{}
	var roots []repoRoot

	// 1. Workspace-root discovery via filesystem walk. Workspace roots
	// must be configured explicitly via SetWorkspaceRoots (production
	// does this from ~/.mnemo/config.json with a sensible default);
	// no implicit walk, so tests stay isolated.
	for _, root := range discoverRepos(s.workspaceRoots) {
		if seen[root] {
			continue
		}
		seen[root] = true
		repo := extractRepo(root)
		if repo == "" {
			repo = repoNameFromPath(root)
		}
		roots = append(roots, repoRoot{root: root, repo: repo})
	}

	// 2. session_meta.cwd → findRepoRoot. Captures repos outside any
	// configured workspace root (e.g., transient clones in /tmp).
	rows, err := s.db.Query("SELECT DISTINCT cwd FROM session_meta WHERE cwd != ''")
	if err != nil {
		return roots
	}
	defer rows.Close()

	for rows.Next() {
		var cwd string
		if rows.Scan(&cwd) != nil {
			continue
		}
		root := findRepoRoot(cwd)
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		repo := extractRepo(cwd)
		if repo == "" {
			repo = repoNameFromPath(root)
		}
		roots = append(roots, repoRoot{root: root, repo: repo})
	}
	return roots
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

// ingestClaudeConfigFile ingests a single CLAUDE.md file (acquires write lock).
func (s *Store) ingestClaudeConfigFile(path, repo string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	return s.ingestClaudeConfigFileLocked(path, repo)
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

	roots := s.knownRepoRootsLocked()
	indexed, onDisk := 0, 0
	for _, rr := range roots {
		auditPath := filepath.Join(rr.root, "docs", "audit-log.md")
		if _, err := os.Stat(auditPath); err != nil {
			continue
		}
		onDisk++

		// Prefer the two-segment repoNameFromPath for audit logs — the
		// existing audit_entries schema uses "org/repo" rather than the
		// bare basename that extractRepo returns.
		repo := repoNameFromPath(rr.root)
		if err := s.ingestAuditLogFileLocked(auditPath, repo); err != nil {
			slog.Error("ingest audit log failed", "file", auditPath, "err", err)
			continue
		}
		indexed++
	}
	s.recordBackfillStatusLocked("audit", indexed, onDisk)
	slog.Info("ingested audit logs", "indexed", indexed, "on_disk", onDisk)
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

// IngestTargets discovers every repo under the configured workspace
// roots (and session_meta) and ingests its docs/targets.md file. Runs
// at startup; realtime updates flow through Watch().
func (s *Store) IngestTargets() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	roots := s.knownRepoRootsLocked()
	indexed, onDisk, targetCount := 0, 0, 0
	for _, rr := range roots {
		targetsPath := filepath.Join(rr.root, "docs", "targets.md")
		data, err := os.ReadFile(targetsPath)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("cannot read targets.md", "path", targetsPath, "err", err)
			}
			continue
		}
		onDisk++

		repo := rr.repo
		if repo == "" {
			repo = filepath.Base(rr.root)
		}

		parsed := parseTargetsFile(repo, targetsPath, data)
		if len(parsed) == 0 {
			continue
		}

		// Delete existing targets for this file and re-insert.
		if _, err := s.db.Exec("DELETE FROM targets WHERE file_path = ?", targetsPath); err != nil {
			slog.Warn("delete targets failed", "path", targetsPath, "err", err)
			continue
		}
		inserted := false
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
			targetCount++
			inserted = true
		}
		if inserted {
			indexed++
		}
	}
	s.recordBackfillStatusLocked("targets", indexed, onDisk)
	slog.Info("ingested targets", "files", indexed, "on_disk", onDisk, "rows", targetCount)
	return nil
}

// ingestTargetFile ingests a single targets.md file (acquires write lock).
func (s *Store) ingestTargetFile(path, repo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.rwmu.Lock()
			defer s.rwmu.Unlock()
			s.db.Exec("DELETE FROM targets WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	parsed := parseTargetsFile(repo, path, data)

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	if _, err := s.db.Exec("DELETE FROM targets WHERE file_path = ?", path); err != nil {
		return fmt.Errorf("delete targets: %w", err)
	}
	for _, t := range parsed {
		if _, err := s.db.Exec(`
			INSERT INTO targets (repo, file_path, target_id, name, status, weight, description, raw_text)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(file_path, target_id) DO UPDATE SET
				repo = excluded.repo,
				name = excluded.name,
				status = excluded.status,
				weight = excluded.weight,
				description = excluded.description,
				raw_text = excluded.raw_text
		`, t.Repo, t.FilePath, t.TargetID, t.Name, t.Status, t.Weight, t.Description, t.RawText); err != nil {
			slog.Warn("insert target failed", "target_id", t.TargetID, "err", err)
		}
	}
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

	roots := s.knownRepoRootsLocked()
	indexed, onDisk := 0, 0
	reposWithPlans := 0
	for _, rr := range roots {
		planningDir := filepath.Join(rr.root, ".planning")
		if _, err := os.Stat(planningDir); err != nil {
			continue
		}
		reposWithPlans++
		repo := rr.repo
		if repo == "" {
			repo = extractRepo(rr.root)
		}

		// Walk all .md files under .planning/
		if err := filepath.Walk(planningDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if strings.ToLower(filepath.Ext(path)) != ".md" {
				return nil
			}
			onDisk++
			if err2 := s.ingestPlanFileLocked(path, repo, planningDir); err2 != nil {
				slog.Error("ingest plan failed", "file", path, "err", err2)
				return nil
			}
			indexed++
			return nil
		}); err != nil {
			slog.Error("walk planning dir failed", "dir", planningDir, "err", err)
		}
	}
	s.recordBackfillStatusLocked("plans", indexed, onDisk)
	slog.Info("ingested plans", "files", indexed, "on_disk", onDisk, "repos", reposWithPlans)
	return nil
}

// ingestPlanFile ingests a single plan file (acquires write lock).
func (s *Store) ingestPlanFile(path, repo, planningDir string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	return s.ingestPlanFileLocked(path, repo, planningDir)
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

// WhoRanResult holds a single result from a WhoRan query.
type WhoRanResult struct {
	SessionID string `json:"session_id"`
	Repo      string `json:"repo"`
	Command   string `json:"command"`
	Timestamp string `json:"timestamp"`
}

// WhoRan returns sessions and timestamps for Bash tool_use entries matching pattern.
func (s *Store) WhoRan(pattern string, days int, repoFilter string, limit int) ([]WhoRanResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if days <= 0 {
		days = 30
	}
	if limit <= 0 {
		limit = 20
	}

	q := `SELECT m.session_id, COALESCE(sm.repo, ''), m.tool_command, m.timestamp
		FROM messages m
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name = 'Bash'
		  AND m.tool_command LIKE ?
		  AND m.timestamp >= datetime('now', ? || ' days')`
	args := []any{"%" + pattern + "%", fmt.Sprintf("-%d", days)}

	if repoFilter != "" {
		q += ` AND sm.repo LIKE ?`
		args = append(args, "%"+repoFilter+"%")
	}
	q += ` ORDER BY m.timestamp DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []WhoRanResult
	for rows.Next() {
		var r WhoRanResult
		if err := rows.Scan(&r.SessionID, &r.Repo, &r.Command, &r.Timestamp); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

// CIRun holds a single CI run record from the index.
type CIRun struct {
	ID          int    `json:"id"`
	Repo        string `json:"repo"`
	RunID       int64  `json:"run_id"`
	Workflow    string `json:"workflow"`
	Branch      string `json:"branch,omitempty"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	LogSummary  string `json:"log_summary,omitempty"`
	URL         string `json:"url,omitempty"`
}

// ChainLink holds a single entry in a session chain.
type ChainLink struct {
	SessionID  string `json:"session_id"`
	Project    string `json:"project"`
	FirstMsg   string `json:"first_msg"`
	LastMsg    string `json:"last_msg"`
	Topic      string `json:"topic,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Confidence string `json:"confidence,omitempty"` // "high", "medium", or "" for the tail
	GapMs      int64  `json:"gap_ms,omitempty"`
}

// DecisionInfo holds a single detected decision record.
type DecisionInfo struct {
	ID               int    `json:"id"`
	SessionID        string `json:"session_id"`
	ProposalText     string `json:"proposal_text"`
	ConfirmationText string `json:"confirmation_text"`
	Repo             string `json:"repo,omitempty"`
	Timestamp        string `json:"timestamp"`
}

// SearchDecisions searches the decisions table by keyword with optional repo and days filters.
func (s *Store) SearchDecisions(query string, repo string, days int, limit int) ([]DecisionInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 30
	}

	var q string
	var args []any
	cutoff := fmt.Sprintf("datetime('now', '-%d days')", days)

	if query != "" {
		ftsQuery := relaxQuery(query)
		q = `SELECT d.id, d.session_id, d.proposal_text, d.confirmation_text, d.repo, d.timestamp
			FROM decisions d
			JOIN decisions_fts f ON f.rowid = d.id
			WHERE decisions_fts MATCH ?`
		args = append(args, ftsQuery)
		q += ` AND d.timestamp >= ` + cutoff
		if repo != "" {
			q += ` AND d.repo LIKE ?`
			args = append(args, "%"+repo+"%")
		}
		q += ` ORDER BY rank LIMIT ?`
	} else {
		q = `SELECT id, session_id, proposal_text, confirmation_text, repo, timestamp
			FROM decisions WHERE timestamp >= ` + cutoff
		if repo != "" {
			q += ` AND repo LIKE ?`
			args = append(args, "%"+repo+"%")
		}
		q += ` ORDER BY timestamp DESC LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []DecisionInfo
	for rows.Next() {
		var d DecisionInfo
		if err := rows.Scan(&d.ID, &d.SessionID, &d.ProposalText, &d.ConfirmationText, &d.Repo, &d.Timestamp); err != nil {
			continue
		}
		results = append(results, d)
	}
	return results, nil
}

// proposalPhrases are patterns that indicate an assistant is proposing a course of action.
var proposalPhrases = []string{
	"i'll ", "i will ", "let me ", "i propose ", "i suggest ", "should we ",
	"i recommend ", "the approach is ", "i'm going to ", "i am going to ",
	"my plan is ", "here's what i'll ", "here's my plan", "the plan is ",
	"i intend to ", "we should ", "i'd like to ",
}

// confirmationPhrases are patterns that indicate a user is confirming a proposal.
var confirmationPhrases = []string{
	"yes", "yeah", "yep", "go ahead", "sounds good", "perfect", "do it",
	"approved", "lgtm", "looks good", "that works", "correct", "right",
	"exactly", "proceed", "ship it", "merge it", "ok", "okay", "sure",
	"great", "good", "please do", "please proceed", "make it so",
	"sounds right", "agreed", "agree", "done", "let's do it", "let's go",
}

// isProposal returns true if the assistant text contains a substantive proposal phrase.
func isProposal(text string) bool {
	lower := strings.ToLower(text)
	// Must be reasonably long to be substantive (not just a greeting).
	if len(text) < 50 {
		return false
	}
	for _, phrase := range proposalPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// isConfirmation returns true if the user text is a clear confirmation.
// Conservative: require the confirmation to be short and match closely.
func isConfirmation(text string) bool {
	// Strip common punctuation and whitespace for matching.
	trimmed := strings.TrimRight(strings.TrimSpace(strings.ToLower(text)), ".!?,")
	// Confirmation messages should be short (under 60 chars after trim).
	if len(trimmed) > 60 {
		return false
	}
	for _, phrase := range confirmationPhrases {
		if trimmed == phrase || strings.HasPrefix(trimmed, phrase+" ") || strings.HasSuffix(trimmed, " "+phrase) {
			return true
		}
	}
	return false
}

// detectDecisions scans consecutive assistant→user message pairs in a session
// and inserts detected proposal+confirmation pairs into the decisions table.
// Uses INSERT OR IGNORE so re-running on the same session is safe.
func detectDecisions(db *sql.DB, sessionID string, repo string) {
	// Load all text-content messages for this session in order.
	rows, err := db.Query(`
		SELECT id, role, text, timestamp
		FROM messages
		WHERE session_id = ?
		  AND content_type = 'text'
		  AND is_noise = 0
		ORDER BY id ASC`, sessionID)
	if err != nil {
		return
	}
	defer rows.Close()

	type msg struct {
		id        int
		role      string
		text      string
		timestamp string
	}

	var msgs []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.role, &m.text, &m.timestamp); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	rows.Close()

	// Scan for assistant→user consecutive pairs.
	for i := 0; i+1 < len(msgs); i++ {
		a := msgs[i]
		u := msgs[i+1]
		if a.role != "assistant" || u.role != "user" {
			continue
		}
		if !isProposal(a.text) || !isConfirmation(u.text) {
			continue
		}
		// Truncate proposal text to avoid huge rows.
		proposal := a.text
		if len(proposal) > 2000 {
			proposal = proposal[:1997] + "..."
		}
		db.Exec(`
			INSERT OR IGNORE INTO decisions
				(session_id, proposal_msg_id, confirmation_msg_id, proposal_text, confirmation_text, repo, timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sessionID, a.id, u.id, proposal, u.text, repo, u.timestamp)
	}
}

// ciRepos returns "org/repo" identifiers for every GitHub repository
// reachable through knownRepoRoots. This is the union of
// workspace-walked repos (default ~/work) and session_meta-known repos,
// so CI polling works even for projects mnemo hasn't seen through a
// session yet. Non-GitHub paths are filtered out.
func (s *Store) ciRepos() ([]string, error) {
	seen := map[string]bool{}
	var repos []string

	// Workspace + session_meta union via the central choke point.
	// knownRepoRoots acquires rwmu.RLock internally.
	for _, rr := range s.knownRepoRoots() {
		repo := extractRepo(rr.root)
		if repo == "" || !strings.Contains(repo, "/") || seen[repo] {
			continue
		}
		seen[repo] = true
		repos = append(repos, repo)
	}

	// Fallback: session_meta.repo column may carry a normalised
	// "org/repo" for repos outside any workspace root (e.g., clones in
	// /tmp). Include anything we haven't already captured.
	rows, err := s.db.Query(
		`SELECT DISTINCT repo FROM session_meta WHERE repo != '' AND repo LIKE '%/%'`,
	)
	if err != nil {
		return repos, nil
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			continue
		}
		if !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	return repos, nil
}

// SearchCI searches CI runs with optional FTS query, repo filter, conclusion filter, and recency window.
func (s *Store) SearchCI(query string, repo string, conclusion string, days int, limit int) ([]CIRun, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 30
	}

	var q string
	var args []any

	if query == "" {
		q = `SELECT id, repo, run_id, workflow, COALESCE(branch,''), COALESCE(commit_sha,''),
		            status, COALESCE(conclusion,''), COALESCE(started_at,''), COALESCE(completed_at,''),
		            COALESCE(log_summary,''), COALESCE(url,'')
		     FROM ci_runs WHERE 1=1`
	} else {
		ftsQuery := relaxQuery(query)
		q = `SELECT c.id, c.repo, c.run_id, c.workflow, COALESCE(c.branch,''), COALESCE(c.commit_sha,''),
		            c.status, COALESCE(c.conclusion,''), COALESCE(c.started_at,''), COALESCE(c.completed_at,''),
		            COALESCE(c.log_summary,''), COALESCE(c.url,'')
		     FROM ci_runs c JOIN ci_runs_fts f ON f.rowid = c.id
		     WHERE ci_runs_fts MATCH ?`
		args = append(args, ftsQuery)
	}

	if repo != "" {
		if query == "" {
			q += ` AND repo LIKE ?`
		} else {
			q += ` AND c.repo LIKE ?`
		}
		args = append(args, "%"+repo+"%")
	}
	if conclusion != "" {
		if query == "" {
			q += ` AND conclusion = ?`
		} else {
			q += ` AND c.conclusion = ?`
		}
		args = append(args, conclusion)
	}
	if days > 0 {
		cutoff := fmt.Sprintf("datetime('now', '-%d days')", days)
		if query == "" {
			q += ` AND (started_at IS NULL OR started_at >= ` + cutoff + `)`
		} else {
			q += ` AND (c.started_at IS NULL OR c.started_at >= ` + cutoff + `)`
		}
	}
	if query == "" {
		q += ` ORDER BY started_at DESC LIMIT ?`
	} else {
		q += ` ORDER BY rank LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []CIRun
	for rows.Next() {
		var r CIRun
		if err := rows.Scan(&r.ID, &r.Repo, &r.RunID, &r.Workflow, &r.Branch, &r.CommitSHA,
			&r.Status, &r.Conclusion, &r.StartedAt, &r.CompletedAt, &r.LogSummary, &r.URL); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

// ghRunJSON matches the JSON output of `gh run list`.
type ghRunJSON struct {
	DatabaseID   int64  `json:"databaseId"`
	WorkflowName string `json:"workflowName"`
	HeadBranch   string `json:"headBranch"`
	HeadSHA      string `json:"headSha"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
	URL          string `json:"url"`
}

// PollCI fetches recent CI runs from GitHub Actions for all repos seen in session_meta
// and upserts them into ci_runs. Failed runs have their logs indexed.
// Silently skips if gh is not installed.
func (s *Store) PollCI() error {
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		// gh not installed — skip silently.
		return nil
	}

	repos, err := s.ciRepos()
	if err != nil {
		return fmt.Errorf("ciRepos: %w", err)
	}

	for _, repo := range repos {
		if err := s.pollCIForRepo(ghPath, repo); err != nil {
			slog.Warn("CI poll failed", "repo", repo, "err", err)
		}
	}
	return nil
}

// pollCIForRepo fetches and upserts CI runs for a single repo.
func (s *Store) pollCIForRepo(ghPath, repo string) error {
	out, err := exec.Command(ghPath, "run", "list",
		"--repo", repo,
		"--json", "databaseId,workflowName,headBranch,headSha,status,conclusion,createdAt,updatedAt,url",
		"--limit", "20",
	).Output()
	if err != nil {
		return fmt.Errorf("gh run list: %w", err)
	}

	var runs []ghRunJSON
	if err := json.Unmarshal(out, &runs); err != nil {
		return fmt.Errorf("parse gh output: %w", err)
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	for _, run := range runs {
		var logSummary string
		if run.Conclusion == "failure" {
			logSummary = s.fetchRunLog(ghPath, repo, run.DatabaseID)
		}

		_, err := s.db.Exec(`
			INSERT INTO ci_runs (repo, run_id, workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(run_id) DO UPDATE SET
				status = excluded.status,
				conclusion = excluded.conclusion,
				completed_at = excluded.completed_at,
				log_summary = COALESCE(excluded.log_summary, ci_runs.log_summary),
				updated_at = datetime('now')
		`, repo, run.DatabaseID, run.WorkflowName, run.HeadBranch, run.HeadSHA,
			run.Status, run.Conclusion, run.CreatedAt, run.UpdatedAt, logSummary, run.URL)
		if err != nil {
			slog.Warn("upsert ci_run failed", "run_id", run.DatabaseID, "err", err)
		}
	}
	return nil
}

// fetchRunLog retrieves the last 50 lines of a failed run's log.
func (s *Store) fetchRunLog(ghPath, repo string, runID int64) string {
	out, err := exec.Command(ghPath, "run", "view",
		fmt.Sprintf("%d", runID),
		"--repo", repo,
		"--log",
	).CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	// Keep last 50 lines.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 50 {
		lines = lines[len(lines)-50:]
	}
	return strings.Join(lines, "\n")
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
	for _, dir := range s.projectDirs() {
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				slog.Warn("project dir unavailable, skipping", "dir", dir)
			} else {
				slog.Warn("project dir stat failed, skipping", "dir", dir, "err", err)
			}
			continue
		}
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
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
	}

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

	// Detect /clear-bounded chain links for any newly ingested sessions.
	// INSERT OR IGNORE makes this idempotent across restarts.
	backfillSessionChains(s.db)

	// Detect decision pairs (proposal + confirmation) in sessions that
	// haven't been scanned yet (e.g. sessions ingested before decisions
	// table existed).
	backfillDecisions(s.db)

	// Extract and store images from all ingested entries and messages.
	// Runs synchronously (fast — no API calls). Description generation
	// happens separately in the background worker started by StartImageDescriber.
	backfillImages(s)

	// Index git commit history from all known repos.
	backfillGitCommits(s)

	// Index GitHub PRs and issues from all known repos.
	// Runs in a goroutine so it doesn't block startup (API calls are slow).
	go backfillGitHubActivity(s)

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
	tx        *sql.Tx
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

	projectDirs := s.projectDirs()
	for _, dir := range projectDirs {
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				slog.Warn("project dir unavailable, skipping watch", "dir", dir)
			} else {
				slog.Warn("project dir stat failed, skipping watch", "dir", dir, "err", err)
			}
			continue
		}
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				if wErr := watcher.Add(path); wErr != nil {
					slog.Warn("failed to watch directory", "path", path, "err", wErr)
				}
			}
			return nil
		})
	}

	// Also watch the skills directory for .md changes.
	if sdir, err := skillsDir(); err == nil {
		if wErr := watcher.Add(sdir); wErr != nil {
			slog.Warn("failed to watch skills directory", "path", sdir, "err", wErr)
		}
	}

	// Watch repo-level context source files (CLAUDE.md, docs/, .planning/).
	repoRoots := s.knownRepoRoots()
	repoForRoot := map[string]string{} // root path → repo name
	watchedDirs := map[string]bool{}
	for _, rr := range repoRoots {
		repoForRoot[rr.root] = rr.repo
		// Watch the repo root itself (for CLAUDE.md changes).
		if !watchedDirs[rr.root] {
			watchedDirs[rr.root] = true
			if wErr := watcher.Add(rr.root); wErr != nil {
				slog.Warn("failed to watch repo root", "path", rr.root, "err", wErr)
			}
		}
		// Watch docs/ for audit-log.md and targets.md.
		docsDir := filepath.Join(rr.root, "docs")
		if info, err := os.Stat(docsDir); err == nil && info.IsDir() && !watchedDirs[docsDir] {
			watchedDirs[docsDir] = true
			if wErr := watcher.Add(docsDir); wErr != nil {
				slog.Warn("failed to watch docs dir", "path", docsDir, "err", wErr)
			}
		}
		// Watch .planning/ recursively for plan files.
		planDir := filepath.Join(rr.root, ".planning")
		if info, err := os.Stat(planDir); err == nil && info.IsDir() {
			filepath.Walk(planDir, func(path string, fi os.FileInfo, err error) error {
				if err == nil && fi.IsDir() && !watchedDirs[path] {
					watchedDirs[path] = true
					if wErr := watcher.Add(path); wErr != nil {
						slog.Warn("failed to watch planning dir", "path", path, "err", wErr)
					}
				}
				return nil
			})
		}
	}
	slog.Info("watching for changes", "transcripts", projectDirs, "repos", len(repoRoots))

	// debounce coalesces burst events (editor saves, formatter rewrites, git
	// operations) for the same path into a single re-index after 300ms of quiet.
	// Heavy ingest work runs in the timer goroutine, not on the event goroutine.
	db := newDebouncer(300 * time.Millisecond)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			name := event.Name
			if strings.HasSuffix(name, ".jsonl") &&
				(event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
				db.enqueue(name, func() {
					if err := s.ingestFile(name); err != nil {
						slog.Error("ingest failed", "file", name, "err", err)
					}
				})
			}
			// Watch memory file changes.
			if strings.HasSuffix(name, ".md") && strings.Contains(name, "/memory/") {
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					db.enqueue(name, func() {
						if err := s.ingestMemoryFile(name); err != nil {
							slog.Error("ingest memory failed", "file", name, "err", err)
						}
					})
				}
				if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					// Deletions are not debounced: the file is already gone.
					s.deleteMemoryFile(name)
				}
			}
			// Watch repo-level context source changes.
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				base := filepath.Base(name)
				dir := filepath.Dir(name)

				// CLAUDE.md at repo root.
				if base == "CLAUDE.md" {
					if repo, ok := repoForRoot[dir]; ok {
						db.enqueue(name, func() {
							if err := s.ingestClaudeConfigFile(name, repo); err != nil {
								slog.Error("ingest claude config failed", "file", name, "err", err)
							}
						})
					}
				}

				// docs/audit-log.md
				if base == "audit-log.md" && filepath.Base(dir) == "docs" {
					repoRoot := filepath.Dir(dir)
					if repo, ok := repoForRoot[repoRoot]; ok {
						db.enqueue(name, func() {
							if err := s.ingestAuditLogFile(name, repo); err != nil {
								slog.Error("ingest audit log failed", "file", name, "err", err)
							}
						})
					}
				}

				// docs/targets.md
				if base == "targets.md" && filepath.Base(dir) == "docs" {
					repoRoot := filepath.Dir(dir)
					if repo, ok := repoForRoot[repoRoot]; ok {
						db.enqueue(name, func() {
							if err := s.ingestTargetFile(name, repo); err != nil {
								slog.Error("ingest targets failed", "file", name, "err", err)
							}
						})
					}
				}

				// .planning/**/*.md
				if strings.HasSuffix(name, ".md") && strings.Contains(name, "/.planning/") {
					for root, repo := range repoForRoot {
						planDir := filepath.Join(root, ".planning")
						if strings.HasPrefix(name, planDir+"/") {
							// Capture loop variables for the closure.
							capturedRepo := repo
							capturedPlanDir := planDir
							db.enqueue(name, func() {
								if err := s.ingestPlanFile(name, capturedRepo, capturedPlanDir); err != nil {
									slog.Error("ingest plan failed", "file", name, "err", err)
								}
							})
							break
						}
					}
				}
			}
			// Watch skill file changes.
			if strings.HasSuffix(name, ".md") && strings.Contains(name, "/.claude/skills/") {
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					db.enqueue(name, func() {
						if err := s.ingestSkillFile(name); err != nil {
							slog.Error("ingest skill failed", "file", name, "err", err)
						}
					})
				}
				if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
					s.deleteSkillFile(name)
				}
			}
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(name); err == nil && info.IsDir() {
					if wErr := watcher.Add(name); wErr != nil {
						slog.Warn("failed to watch new directory", "path", name, "err", wErr)
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

	// Per-stream backfill status — inlined while rwmu.RLock is held.
	if strRows, strErr := s.db.Query(`
		SELECT stream, last_backfill, files_indexed, files_on_disk
		FROM ingest_status
		ORDER BY stream
	`); strErr == nil {
		for strRows.Next() {
			var b BackfillStatus
			if scanErr := strRows.Scan(&b.Stream, &b.LastBackfill, &b.FilesIndexed, &b.FilesOnDisk); scanErr == nil {
				result.Streams = append(result.Streams, b)
			}
		}
		strRows.Close()
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

	// Compute hourly rate from the actual time span of assistant messages.
	activeHours, err := s.activeHours(days, repoFilter, model)
	if err == nil && activeHours > 0 {
		result.HourlyRate = &HourlyRate{
			ActiveHours:     activeHours,
			InputPerHour:    float64(result.Total.InputTokens) / activeHours,
			OutputPerHour:   float64(result.Total.OutputTokens) / activeHours,
			CostPerHour:     result.Total.CostUSD / activeHours,
			MessagesPerHour: float64(result.Total.Messages) / activeHours,
		}
	}

	return result, nil
}

// activeHours estimates the number of active hours of assistant usage within
// the given period. It sums intra-session time spans, treating any gap between
// consecutive messages > 30 minutes as idle time.
func (s *Store) activeHours(days int, repoFilter, model string) (float64, error) {
	where := []string{
		"e.type = 'assistant'",
		"e.timestamp >= datetime('now', ?)",
	}
	args := []any{fmt.Sprintf("-%d days", days)}

	needJoin := repoFilter != ""
	if repoFilter != "" {
		where = append(where, "(sm.repo LIKE ? OR sm.cwd LIKE ?)")
		pattern := "%" + repoFilter + "%"
		args = append(args, pattern, pattern)
	}
	if model != "" {
		where = append(where, "e.model LIKE ?")
		args = append(args, model+"%")
	}

	joinClause := ""
	if needJoin {
		joinClause = "LEFT JOIN session_meta sm ON sm.session_id = e.session_id"
	}

	q := fmt.Sprintf(`
		SELECT e.session_id, e.timestamp
		FROM entries e
		%s
		WHERE %s
		ORDER BY e.session_id, e.timestamp
	`, joinClause, strings.Join(where, " AND "))

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	const idleThreshold = 30 * time.Minute
	var totalActive time.Duration
	var prevSession string
	var prevTime time.Time

	for rows.Next() {
		var sessionID, ts string
		if err := rows.Scan(&sessionID, &ts); err != nil {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", ts)
		if err != nil {
			t, err = time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
		}

		if sessionID == prevSession && !prevTime.IsZero() {
			gap := t.Sub(prevTime)
			if gap > 0 && gap <= idleThreshold {
				totalActive += gap
			}
		}
		prevSession = sessionID
		prevTime = t
	}

	hours := totalActive.Hours()
	// If there's data but all within a single message per session, return a
	// minimum of the number of distinct sessions × 1 minute as a floor.
	if hours == 0 {
		return 0, nil
	}
	return hours, nil
}

// PermissionSuggestion is a single tool or command pattern with usage count and allowedTools rule.
type PermissionSuggestion struct {
	ToolName   string `json:"tool_name"`
	Count      int    `json:"count"`
	Suggestion string `json:"suggestion"`
}

// PermissionsResult holds tool usage analysis with suggested allowedTools rules.
type PermissionsResult struct {
	Days         int                    `json:"days"`
	TopTools     []PermissionSuggestion `json:"top_tools"`
	BashCommands []PermissionSuggestion `json:"bash_commands,omitempty"`
}

// Permissions analyzes tool_use patterns to suggest allowedTools rules for settings.json.
func (s *Store) Permissions(days int, repoFilter string, limit int) (*PermissionsResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if days <= 0 {
		days = 30
	}
	if limit <= 0 {
		limit = 20
	}

	daysArg := fmt.Sprintf("-%d days", days)

	// Build optional repo filter clause.
	repoJoin := ""
	repoWhere := ""
	var repoArgs []any
	if repoFilter != "" {
		repoJoin = "JOIN session_meta sm ON sm.session_id = m.session_id"
		repoWhere = "AND (sm.repo LIKE ? OR sm.cwd LIKE ?)"
		pattern := "%" + repoFilter + "%"
		repoArgs = []any{pattern, pattern}
	}

	// Query 1: top tools by usage count.
	topQuery := fmt.Sprintf(`
		SELECT m.tool_name, COUNT(*) AS cnt
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		%s
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name IS NOT NULL
		  AND e.timestamp >= datetime('now', ?)
		  %s
		GROUP BY m.tool_name
		ORDER BY cnt DESC
		LIMIT ?
	`, repoJoin, repoWhere)

	topArgs := append([]any{daysArg}, repoArgs...)
	topArgs = append(topArgs, limit)

	rows, err := s.db.Query(topQuery, topArgs...)
	if err != nil {
		return nil, fmt.Errorf("top tools query: %w", err)
	}
	defer rows.Close()

	result := &PermissionsResult{Days: days}
	for rows.Next() {
		var toolName string
		var cnt int
		if err := rows.Scan(&toolName, &cnt); err != nil {
			continue
		}
		result.TopTools = append(result.TopTools, PermissionSuggestion{
			ToolName:   toolName,
			Count:      cnt,
			Suggestion: toolName,
		})
	}
	rows.Close()

	// Query 2: Bash command prefix patterns.
	bashQuery := fmt.Sprintf(`
		SELECT
		  CASE
		    WHEN m.tool_command LIKE 'go %%' THEN 'go'
		    WHEN m.tool_command LIKE 'git %%' THEN 'git'
		    WHEN m.tool_command LIKE 'make%%' THEN 'make'
		    WHEN m.tool_command LIKE 'npm %%' THEN 'npm'
		    WHEN m.tool_command LIKE 'cargo %%' THEN 'cargo'
		    ELSE substr(m.tool_command, 1, instr(m.tool_command || ' ', ' ') - 1)
		  END AS cmd_prefix,
		  COUNT(*) AS cnt
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		%s
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name = 'Bash'
		  AND m.tool_command IS NOT NULL
		  AND e.timestamp >= datetime('now', ?)
		  %s
		GROUP BY cmd_prefix
		ORDER BY cnt DESC
		LIMIT ?
	`, repoJoin, repoWhere)

	bashArgs := append([]any{daysArg}, repoArgs...)
	bashArgs = append(bashArgs, limit)

	bashRows, err := s.db.Query(bashQuery, bashArgs...)
	if err != nil {
		return nil, fmt.Errorf("bash commands query: %w", err)
	}
	defer bashRows.Close()

	for bashRows.Next() {
		var cmdPrefix string
		var cnt int
		if err := bashRows.Scan(&cmdPrefix, &cnt); err != nil {
			continue
		}
		if cmdPrefix == "" {
			continue
		}
		result.BashCommands = append(result.BashCommands, PermissionSuggestion{
			ToolName:   cmdPrefix,
			Count:      cnt,
			Suggestion: fmt.Sprintf("Bash(%s *)", cmdPrefix),
		})
	}

	return result, nil
}

// PatternCandidate is a detected workaround pattern suggesting a missing feature.
type PatternCandidate struct {
	PatternType string   `json:"pattern_type"` // "direct_jsonl_read", "transcript_grep", "repeated_query", "repeated_search"
	Description string   `json:"description"`
	Occurrences int      `json:"occurrences"`
	Sessions    []string `json:"sessions"`   // session IDs truncated to 8 chars
	Evidence    string   `json:"evidence"`   // example command or query
	Suggestion  string   `json:"suggestion"` // what to build
}

// DiscoverPatterns mines the transcript index for workaround patterns that
// suggest missing mnemo features. It runs entirely at query time — no new
// tables are required.
func (s *Store) DiscoverPatterns(days int, repoFilter string, minOccurrences int) ([]PatternCandidate, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if days <= 0 {
		days = 90
	}
	if minOccurrences <= 0 {
		minOccurrences = 3
	}

	daysArg := fmt.Sprintf("-%d days", days)

	// Build optional repo filter.
	repoJoin := ""
	repoWhere := ""
	var repoBaseArgs []any
	if repoFilter != "" {
		repoJoin = "JOIN session_meta sm ON sm.session_id = m.session_id"
		repoWhere = "AND (sm.repo LIKE ? OR sm.cwd LIKE ?)"
		pat := "%" + repoFilter + "%"
		repoBaseArgs = []any{pat, pat}
	}

	var candidates []PatternCandidate

	// --- Pattern 1: Direct JSONL reads via Bash ---
	{
		q := fmt.Sprintf(`
			SELECT m.session_id, m.tool_command
			FROM messages m
			JOIN entries e ON e.id = m.entry_id
			%s
			WHERE m.content_type = 'tool_use'
			  AND m.tool_name = 'Bash'
			  AND m.tool_command IS NOT NULL
			  AND (m.tool_command LIKE '%%/.claude/projects/%%' OR m.tool_command LIKE '%%/.claude/sessions/%%')
			  AND m.tool_command LIKE '%%.jsonl%%'
			  AND e.timestamp >= datetime('now', ?)
			  %s
		`, repoJoin, repoWhere)

		args := append([]any{daysArg}, repoBaseArgs...)
		rows, err := s.db.Query(q, args...)
		if err == nil {
			sessions, evidence := discoverCollectRows(rows)
			rows.Close()
			if len(sessions) >= minOccurrences {
				candidates = append(candidates, PatternCandidate{
					PatternType: "direct_jsonl_read",
					Description: "Bash commands reading JSONL transcript files directly instead of using mnemo tools",
					Occurrences: len(sessions),
					Sessions:    sessions,
					Evidence:    evidence,
					Suggestion:  "Use mnemo_search or mnemo_read_session instead of reading JSONL files directly.",
				})
			}
		}
	}

	// --- Pattern 2: Grep/rg over transcript directories ---
	{
		q := fmt.Sprintf(`
			SELECT m.session_id,
			       COALESCE(m.tool_command, m.tool_pattern) AS cmd
			FROM messages m
			JOIN entries e ON e.id = m.entry_id
			%s
			WHERE m.content_type = 'tool_use'
			  AND m.tool_name IN ('Bash', 'Grep')
			  AND (
			    m.tool_command LIKE '%%/.claude/projects/%%'
			    OR m.tool_command LIKE '%%/.claude/sessions/%%'
			    OR m.tool_pattern LIKE '%%/.claude/projects/%%'
			    OR m.tool_pattern LIKE '%%/.claude/sessions/%%'
			  )
			  AND e.timestamp >= datetime('now', ?)
			  %s
		`, repoJoin, repoWhere)

		args := append([]any{daysArg}, repoBaseArgs...)
		rows, err := s.db.Query(q, args...)
		if err == nil {
			sessions, evidence := discoverCollectRows(rows)
			rows.Close()
			if len(sessions) >= minOccurrences {
				candidates = append(candidates, PatternCandidate{
					PatternType: "transcript_grep",
					Description: "Grep/rg commands targeting transcript directories instead of using mnemo_search",
					Occurrences: len(sessions),
					Sessions:    sessions,
					Evidence:    evidence,
					Suggestion:  "Use mnemo_search with appropriate query terms instead of grep over transcript dirs.",
				})
			}
		}
	}

	// --- Pattern 3: Repeated mnemo_query shapes ---
	{
		q := fmt.Sprintf(`
			SELECT m.session_id, m.tool_query AS query
			FROM messages m
			JOIN entries e ON e.id = m.entry_id
			%s
			WHERE m.content_type = 'tool_use'
			  AND m.tool_name = 'mnemo_query'
			  AND m.tool_query IS NOT NULL
			  AND e.timestamp >= datetime('now', ?)
			  %s
		`, repoJoin, repoWhere)

		args := append([]any{daysArg}, repoBaseArgs...)
		rows, err := s.db.Query(q, args...)
		if err == nil {
			type qrow struct {
				sessionID string
				query     string
			}
			var allRows []qrow
			for rows.Next() {
				var r qrow
				if rows.Scan(&r.sessionID, &r.query) == nil {
					allRows = append(allRows, r)
				}
			}
			rows.Close()

			type shapeGroup struct {
				sessions map[string]struct{}
				example  string
			}
			shapes := map[string]*shapeGroup{}
			for _, r := range allRows {
				shape := discoverNormalizeSQL(r.query)
				sg, ok := shapes[shape]
				if !ok {
					sg = &shapeGroup{sessions: map[string]struct{}{}, example: r.query}
					shapes[shape] = sg
				}
				sg.sessions[r.sessionID] = struct{}{}
			}

			for _, sg := range shapes {
				if len(sg.sessions) >= minOccurrences {
					sessions := discoverSessionSet(sg.sessions)
					evidence := sg.example
					if len(evidence) > 200 {
						evidence = evidence[:200] + "..."
					}
					candidates = append(candidates, PatternCandidate{
						PatternType: "repeated_query",
						Description: fmt.Sprintf("The same mnemo_query shape was run across %d sessions — candidate for a template", len(sessions)),
						Occurrences: len(sessions),
						Sessions:    sessions,
						Evidence:    evidence,
						Suggestion:  "Save this query as a template with mnemo_define for reuse.",
					})
				}
			}
		}
	}

	// --- Pattern 4: Repeated mnemo_search patterns ---
	{
		q := fmt.Sprintf(`
			SELECT m.session_id, m.tool_query AS query
			FROM messages m
			JOIN entries e ON e.id = m.entry_id
			%s
			WHERE m.content_type = 'tool_use'
			  AND m.tool_name = 'mnemo_search'
			  AND m.tool_query IS NOT NULL
			  AND e.timestamp >= datetime('now', ?)
			  %s
		`, repoJoin, repoWhere)

		args := append([]any{daysArg}, repoBaseArgs...)
		rows, err := s.db.Query(q, args...)
		if err == nil {
			type srow struct {
				sessionID string
				query     string
			}
			var allRows []srow
			for rows.Next() {
				var r srow
				if rows.Scan(&r.sessionID, &r.query) == nil {
					allRows = append(allRows, r)
				}
			}
			rows.Close()

			type searchGroup struct {
				sessions map[string]struct{}
				example  string
			}
			groups := map[string]*searchGroup{}
			for _, r := range allRows {
				norm := discoverNormalizeSearch(r.query)
				sg, ok := groups[norm]
				if !ok {
					sg = &searchGroup{sessions: map[string]struct{}{}, example: r.query}
					groups[norm] = sg
				}
				sg.sessions[r.sessionID] = struct{}{}
			}

			for norm, sg := range groups {
				if len(sg.sessions) >= minOccurrences {
					sessions := discoverSessionSet(sg.sessions)
					candidates = append(candidates, PatternCandidate{
						PatternType: "repeated_search",
						Description: fmt.Sprintf("Search pattern %q repeated across %d sessions — may warrant a dedicated tool", norm, len(sessions)),
						Occurrences: len(sessions),
						Sessions:    sessions,
						Evidence:    sg.example,
						Suggestion:  "Consider adding a dedicated mnemo tool for this recurring search need.",
					})
				}
			}
		}
	}

	return candidates, nil
}

// discoverCollectRows scans rows of (session_id, cmd) and returns
// a deduplicated slice of session IDs (truncated to 8 chars) and the first evidence value.
func discoverCollectRows(rows interface {
	Next() bool
	Scan(...any) error
}) ([]string, string) {
	seen := map[string]struct{}{}
	evidence := ""
	for rows.Next() {
		var sid, cmd string
		if rows.Scan(&sid, &cmd) != nil {
			continue
		}
		if evidence == "" && cmd != "" {
			if len(cmd) > 200 {
				cmd = cmd[:200] + "..."
			}
			evidence = cmd
		}
		key := sid
		if len(key) > 8 {
			key = key[:8]
		}
		seen[key] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out, evidence
}

// discoverSessionSet converts a set of session IDs to a slice, truncating each to 8 chars.
func discoverSessionSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for sid := range m {
		key := sid
		if len(key) > 8 {
			key = key[:8]
		}
		out = append(out, key)
	}
	return out
}

// discoverNormalizeSQL strips string literals and numbers from a SQL query
// and collapses whitespace to produce a structural shape for grouping.
func discoverNormalizeSQL(q string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			if !inStr {
				inStr = true
			} else {
				inStr = false
				b.WriteString("?")
			}
			continue
		}
		if inStr {
			continue
		}
		b.WriteByte(c)
	}
	s := b.String()

	result := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			result = append(result, '?')
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
		} else {
			result = append(result, s[i])
			i++
		}
	}

	return strings.Join(strings.Fields(strings.ToLower(string(result))), " ")
}

// discoverNormalizeSearch lowercases and sorts words for canonical grouping.
func discoverNormalizeSearch(q string) string {
	words := strings.Fields(strings.ToLower(q))
	for i := 0; i < len(words)-1; i++ {
		for j := i + 1; j < len(words); j++ {
			if words[j] < words[i] {
				words[i], words[j] = words[j], words[i]
			}
		}
	}
	return strings.Join(words, " ")
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

	// Per-stream backfill status. Inlined rather than calling the
	// public BackfillStatuses() because we already hold rwmu.RLock.
	var streams []BackfillStatus
	if strRows, strErr := s.db.Query(`
		SELECT stream, last_backfill, files_indexed, files_on_disk
		FROM ingest_status
		ORDER BY stream
	`); strErr == nil {
		for strRows.Next() {
			var b BackfillStatus
			if scanErr := strRows.Scan(&b.Stream, &b.LastBackfill, &b.FilesIndexed, &b.FilesOnDisk); scanErr == nil {
				streams = append(streams, b)
			}
		}
		strRows.Close()
	}

	return &StatusResult{Days: days, Repos: repos, Streams: streams}, nil
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

// liveSessionsTTL is how long to cache lsof results.
const liveSessionsTTL = 5 * time.Second

// LiveSessions returns a map of session ID → PID for all Claude Code sessions
// that currently have their transcript JSONL file open. Uses lsof for liveness
// detection and caches results for liveSessionsTTL to avoid hammering the OS.
func (s *Store) LiveSessions() map[string]int {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if time.Since(s.liveCacheTime) < liveSessionsTTL {
		return s.liveCache
	}
	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")
	result := parseLsofOutput(runLsof(projectsDir))
	s.liveCache = result
	s.liveCacheTime = time.Now()
	return result
}

// SessionCWD returns the working directory recorded for the session in
// session_meta, or "" if not known. Used by the compaction watcher for
// self-exclusion (summariser sessions have the mnemo repo as their cwd).
func (s *Store) SessionCWD(sessionID string) string {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	var cwd string
	s.db.QueryRow("SELECT cwd FROM session_meta WHERE session_id = ? LIMIT 1", sessionID).Scan(&cwd)
	return cwd
}

// runLsof runs lsof to find JSONL files open by any claude process under dir.
// Returns the raw output lines. Exported for testing via a seam — in tests,
// the actual lsof binary is not required; parseLsofOutput is tested directly.
func runLsof(projectsDir string) []byte {
	out, _ := exec.Command("lsof", "-c", "claude", "-a", "+D", projectsDir).Output()
	return out
}

// parseLsofOutput parses lsof output into a sessionID → PID map.
// Each JSONL filename stem (without .jsonl) is treated as a session ID.
func parseLsofOutput(data []byte) map[string]int {
	result := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		// lsof output columns: COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME
		// We need at least PID (index 1) and NAME (last field, index >= 8).
		if len(fields) < 9 {
			continue
		}
		name := fields[len(fields)-1]
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		pid := 0
		for _, ch := range fields[1] {
			if ch < '0' || ch > '9' {
				pid = -1
				break
			}
			pid = pid*10 + int(ch-'0')
		}
		if pid <= 0 {
			continue
		}
		base := filepath.Base(name)
		sessionID := strings.TrimSuffix(base, ".jsonl")
		if sessionID != "" {
			result[sessionID] = pid
		}
	}
	return result
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

	// Detect chain link for this session (no-op if not a /clear rollover).
	detectChainForSession(s.db, sessionID)

	// Detect decision pairs (proposal + confirmation) in this session.
	repo := extractRepo(metaCwd)
	detectDecisions(s.db, sessionID, repo)

	// Extract and store any images from newly ingested entries.
	// Uses a targeted query so only new entries need scanning.
	go func() {
		rows, err := s.db.Query(`
			SELECT e.id, e.raw, COALESCE(e.timestamp, datetime('now'))
			FROM entries e
			WHERE e.session_id = ? AND e.raw LIKE '%"type":"image"%'
			ORDER BY e.id DESC LIMIT 100`, sessionID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var raw []byte
			var ts string
			if rows.Scan(&id, &raw, &ts) == nil {
				ingestImagesForEntry(s, id, sessionID, raw, ts)
			}
		}
	}()

	return nil
}

// clearMarkerRE matches the /clear command marker in user messages.
var clearMarkerRE = regexp.MustCompile(`<command-name>/clear</command-name>`)

// detectChainForSession checks if sessionID is the successor in a /clear
// rollover chain and inserts a session_chains row if so.
// It is idempotent — uses INSERT OR IGNORE.
func detectChainForSession(db *sql.DB, sessionID string) {
	// Check if this session's first user message contains the /clear marker.
	// The /clear message is flagged as noise by isNoise, so we must include
	// is_noise = 1 here, but we specifically look for the command-name marker.
	var firstText string
	var firstTS string
	err := db.QueryRow(`
		SELECT m.text, m.timestamp
		FROM messages m
		WHERE m.session_id = ?
		  AND m.role = 'user'
		ORDER BY m.id ASC
		LIMIT 1`, sessionID).Scan(&firstText, &firstTS)
	if err != nil {
		return // no user messages yet
	}
	if !clearMarkerRE.MatchString(firstText) {
		return // not a /clear rollover
	}

	// Parse successor's first event timestamp.
	succTime, err := time.Parse(time.RFC3339, firstTS)
	if err != nil {
		return
	}

	// Find this session's project (cwd) for scoping predecessor search.
	var cwd string
	db.QueryRow("SELECT cwd FROM session_meta WHERE session_id = ?", sessionID).Scan(&cwd)
	if cwd == "" {
		return // can't scope without a cwd
	}

	// Find the predecessor: the most recent session in the same cwd
	// that ended before this session's first event. No time window —
	// under the working assumption of one Claude Code session per repo
	// at a time, the user might legitimately /clear hours or days
	// later to continue the same work, and any fixed window would
	// drop those legitimate chains. Future work: relax the
	// single-session assumption by checking for concurrent tabs.
	row := db.QueryRow(`
		SELECT ss.session_id, ss.last_msg
		FROM session_summary ss
		JOIN session_meta sm ON sm.session_id = ss.session_id
		WHERE sm.cwd = ?
		  AND ss.session_id != ?
		  AND ss.last_msg <= ?
		ORDER BY ss.last_msg DESC
		LIMIT 1`,
		cwd, sessionID, firstTS)

	var bestPredID, predLastMsg string
	if err := row.Scan(&bestPredID, &predLastMsg); err != nil {
		return
	}

	var gapMs int64
	if predTime, err := time.Parse(time.RFC3339, predLastMsg); err == nil {
		gapMs = succTime.Sub(predTime).Milliseconds()
	}

	confidence := "high"

	db.Exec(`INSERT OR IGNORE INTO session_chains
		(successor_id, predecessor_id, boundary, gap_ms, confidence, mechanism)
		VALUES (?, ?, 'clear', ?, ?, 'cwd_most_recent')`,
		sessionID, bestPredID, gapMs, confidence)
}

// backfillSessionChains runs detectChainForSession for all ingested sessions
// that have a /clear marker but no chain entry yet. Safe to call on every startup.
func backfillSessionChains(db *sql.DB) {
	// Find sessions whose first user message (including noise) contains the /clear
	// marker and that don't yet have a session_chains entry.
	rows, err := db.Query(`
		SELECT m.session_id
		FROM messages m
		WHERE m.role = 'user'
		  AND m.text LIKE '%<command-name>/clear</command-name>%'
		  AND m.id = (
			SELECT MIN(m2.id) FROM messages m2
			WHERE m2.session_id = m.session_id AND m2.role = 'user'
		  )
		  AND NOT EXISTS (SELECT 1 FROM session_chains sc WHERE sc.successor_id = m.session_id)`)
	if err != nil {
		slog.Warn("backfill session chains query failed", "err", err)
		return
	}
	defer rows.Close()

	var sessionIDs []string
	for rows.Next() {
		var sid string
		if rows.Scan(&sid) == nil {
			sessionIDs = append(sessionIDs, sid)
		}
	}
	rows.Close()

	for _, sid := range sessionIDs {
		detectChainForSession(db, sid)
	}
	if len(sessionIDs) > 0 {
		slog.Info("backfilled session chains", "count", len(sessionIDs))
	}
}

// Predecessor returns the predecessor session ID for the given session,
// or "" if none exists.
func (s *Store) Predecessor(sessionID string) (string, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	var predID string
	err := s.db.QueryRow(
		"SELECT predecessor_id FROM session_chains WHERE successor_id = ?", sessionID,
	).Scan(&predID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return predID, err
}

// Successor returns the successor session ID for the given session,
// or "" if none exists.
func (s *Store) Successor(sessionID string) (string, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	var succID string
	err := s.db.QueryRow(
		"SELECT successor_id FROM session_chains WHERE predecessor_id = ?", sessionID,
	).Scan(&succID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return succID, err
}

// Chain returns the full ordered chain of ChainLinks from oldest to newest,
// given any session ID in the chain. If the session has no chain links,
// returns a single-element slice with that session's info.
func (s *Store) Chain(sessionID string) ([]ChainLink, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	return s.chainLocked(sessionID)
}

func (s *Store) chainLocked(sessionID string) ([]ChainLink, error) {
	// Walk backwards to find the head (oldest) of the chain.
	head := sessionID
	visited := map[string]bool{head: true}
	for {
		var pred string
		err := s.db.QueryRow(
			"SELECT predecessor_id FROM session_chains WHERE successor_id = ?", head,
		).Scan(&pred)
		if errors.Is(err, sql.ErrNoRows) {
			break // head found
		}
		if err != nil {
			return nil, err
		}
		if visited[pred] {
			break // cycle guard
		}
		visited[pred] = true
		head = pred
	}

	// Walk forwards to build the ordered chain, annotating each link with the
	// gap/confidence of its connection to the next session.
	var chain []ChainLink
	cur := head
	for cur != "" {
		link, err := s.sessionChainLink(cur)
		if err != nil {
			return nil, err
		}
		chain = append(chain, link)

		var succ string
		var gapMs int64
		var confidence string
		err = s.db.QueryRow(
			"SELECT successor_id, gap_ms, confidence FROM session_chains WHERE predecessor_id = ?", cur,
		).Scan(&succ, &gapMs, &confidence)
		if errors.Is(err, sql.ErrNoRows) {
			break // end of chain
		}
		if err != nil {
			return nil, err
		}
		// Annotate current link with its connection info to the next session.
		chain[len(chain)-1].GapMs = gapMs
		chain[len(chain)-1].Confidence = confidence
		cur = succ
	}
	return chain, nil
}

// runPsEnv is a test seam: in production it invokes ps to get PID→env output.
// Tests replace it with a function that returns synthetic output.
var runPsEnv = func(pids []string) []byte {
	// ps -wwEo pid,command shows env vars appended after the command line.
	args := append([]string{"-wwEo", "pid,command", "-p"}, strings.Join(pids, ","))
	out, _ := exec.Command("ps", args...).Output()
	return out
}

// parsePsEnvOutput parses the output of `ps -wwEo pid,command -p <pids>`
// and returns a map of PID → PWD value. Lines that lack a PWD entry are
// silently skipped (graceful degradation per AC5).
func parsePsEnvOutput(data []byte) map[int]string {
	result := make(map[int]string)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid := 0
		for _, ch := range fields[0] {
			if ch < '0' || ch > '9' {
				pid = -1
				break
			}
			pid = pid*10 + int(ch-'0')
		}
		if pid <= 0 {
			continue
		}
		// Environment variables follow the command in the same line,
		// separated by spaces. Find the PWD=... entry.
		rest := line[strings.Index(line, fields[0])+len(fields[0]):]
		for _, tok := range strings.Fields(rest) {
			if strings.HasPrefix(tok, "PWD=") {
				result[pid] = tok[len("PWD="):]
				break
			}
		}
	}
	return result
}

// cwdToTranscripts maps a working directory path to candidate transcript files
// under ~/.claude/projects/<encoded-cwd>/.  The encoded name replaces each '/'
// with '-'.  Files are returned sorted newest-mtime first.
func cwdToTranscripts(cwd string) []WhatsupTranscript {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// Encode cwd: replace all '/' with '-'.
	encoded := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []WhatsupTranscript
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, WhatsupTranscript{
			Path:  filepath.Join(dir, e.Name()),
			MTime: info.ModTime(),
			Size:  info.Size(),
		})
	}
	// Sort newest-mtime first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].MTime.After(out[j].MTime)
	})
	return out
}

// Whatsup returns per-session process metrics for all live Claude sessions,
// alongside system-wide memory pressure. On non-macOS platforms it returns
// best-effort data (RSS/CPU via ps where available, zeroed system metrics).
//
// When postmortem is true and no live sessions are found, it scans recent
// transcript files (modified within postmortemWindow) grouped by cwd and
// returns them as WhatsupPostmortemEntry values.
func (s *Store) Whatsup(postmortem bool) (*WhatsupResult, error) {
	const postmortemWindow = 24 * time.Hour

	sessions := s.LiveSessions()
	result := &WhatsupResult{}

	if len(sessions) == 0 {
		if postmortem {
			result.Postmortem = collectPostmortem(postmortemWindow)
		}
		return result, nil
	}

	// Build PID list for a single batched ps invocation.
	pidList := make([]string, 0, len(sessions))
	pidToSession := make(map[int]string, len(sessions))
	for sid, pid := range sessions {
		pidList = append(pidList, fmt.Sprintf("%d", pid))
		pidToSession[pid] = sid
	}

	type psRow struct {
		rss     int64
		cpuPct  float64
		cpuTime string
	}
	pidMetrics := make(map[int]psRow)

	// ps -o pid=,rss=,%cpu=,time= -p <pid,...>
	args := append([]string{"-o", "pid=,rss=,%cpu=,time=", "-p"}, strings.Join(pidList, ","))
	out, err := exec.Command("ps", args...).Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			pid := 0
			for _, ch := range fields[0] {
				if ch < '0' || ch > '9' {
					pid = -1
					break
				}
				pid = pid*10 + int(ch-'0')
			}
			if pid <= 0 {
				continue
			}
			rss := int64(0)
			for _, ch := range fields[1] {
				if ch >= '0' && ch <= '9' {
					rss = rss*10 + int64(ch-'0')
				}
			}
			cpuPct := 0.0
			fmt.Sscanf(fields[2], "%f", &cpuPct)
			pidMetrics[pid] = psRow{
				rss:     rss * 1024, // ps reports RSS in KB
				cpuPct:  cpuPct,
				cpuTime: fields[3],
			}
		}
	}

	// Collect cwd for each PID via ps -E (graceful: missing PWD is skipped).
	pidCwd := parsePsEnvOutput(runPsEnv(pidList))

	// Query session_meta for each session to get repo/topic/work_type.
	type metaRow struct {
		repo     string
		topic    string
		workType string
	}
	sessionMeta := make(map[string]metaRow, len(sessions))
	rows, err := s.db.Query(`
		SELECT session_id, COALESCE(repo, ''), COALESCE(topic, ''), COALESCE(work_type, '')
		FROM session_meta
		WHERE session_id IN (`+placeholders(len(pidList))+`)`,
		stringsToAny(func() []string {
			ids := make([]string, 0, len(sessions))
			for id := range sessions {
				ids = append(ids, id)
			}
			return ids
		}())...,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var sid, repo, topic, workType string
			if rows.Scan(&sid, &repo, &topic, &workType) == nil {
				sessionMeta[sid] = metaRow{repo: repo, topic: topic, workType: workType}
			}
		}
	}

	for sid, pid := range sessions {
		m := pidMetrics[pid]
		meta := sessionMeta[sid]
		cwd := pidCwd[pid]
		var transcripts []WhatsupTranscript
		if cwd != "" {
			transcripts = cwdToTranscripts(cwd)
		}
		result.Sessions = append(result.Sessions, WhatsupSession{
			SessionID:   sid,
			PID:         pid,
			Cwd:         cwd,
			Transcripts: transcripts,
			Repo:        meta.repo,
			Topic:       meta.topic,
			WorkType:    meta.workType,
			CPUPct:      m.cpuPct,
			RSSBytes:    m.rss,
			CPUTime:     m.cpuTime,
		})
	}

	// Sort by CPU% descending so the busiest session is first.
	sort.Slice(result.Sessions, func(i, j int) bool {
		if result.Sessions[i].CPUPct != result.Sessions[j].CPUPct {
			return result.Sessions[i].CPUPct > result.Sessions[j].CPUPct
		}
		return result.Sessions[i].RSSBytes > result.Sessions[j].RSSBytes
	})

	// Collect system memory pressure (macOS only).
	if runtime.GOOS == "darwin" {
		result.System = collectVMStat()
	}

	return result, nil
}

// collectPostmortem scans ~/.claude/projects/ for transcript files modified
// within the recency window and groups them by decoded cwd.
func collectPostmortem(window time.Duration) []WhatsupPostmortemEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	dirEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-window)
	// Map encoded-cwd → candidates.
	byDir := make(map[string][]WhatsupTranscript)
	for _, de := range dirEntries {
		if !de.IsDir() {
			continue
		}
		subdir := filepath.Join(projectsDir, de.Name())
		files, err := os.ReadDir(subdir)
		if err != nil {
			continue
		}
		for _, fe := range files {
			if fe.IsDir() || !strings.HasSuffix(fe.Name(), ".jsonl") {
				continue
			}
			info, err := fe.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
			byDir[de.Name()] = append(byDir[de.Name()], WhatsupTranscript{
				Path:  filepath.Join(subdir, fe.Name()),
				MTime: info.ModTime(),
				Size:  info.Size(),
			})
		}
	}
	var out []WhatsupPostmortemEntry
	for encoded, transcripts := range byDir {
		// Decode: replace '-' with '/'. The encoded name starts with '-' because
		// absolute paths begin with '/'.
		cwd := strings.ReplaceAll(encoded, "-", "/")
		sort.Slice(transcripts, func(i, j int) bool {
			return transcripts[i].MTime.After(transcripts[j].MTime)
		})
		out = append(out, WhatsupPostmortemEntry{Cwd: cwd, Transcripts: transcripts})
	}
	// Sort by most-recent transcript mtime descending.
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Transcripts) == 0 {
			return false
		}
		if len(out[j].Transcripts) == 0 {
			return true
		}
		return out[i].Transcripts[0].MTime.After(out[j].Transcripts[0].MTime)
	})
	return out
}

// collectVMStat parses vm_stat output to compute macOS memory pressure.
func collectVMStat() SystemMetrics {
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return SystemMetrics{}
	}
	vals := make(map[string]int64)
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like: "Pages free:                               12345."
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[1]), "."))
		var v int64
		fmt.Sscanf(valStr, "%d", &v)
		vals[key] = v
	}
	free := vals["Pages free"]
	active := vals["Pages active"]
	inactive := vals["Pages inactive"]
	wired := vals["Pages wired down"]
	total := free + active + inactive + wired
	pressure := 0.0
	if total > 0 {
		pressure = float64(active+wired) / float64(total) * 100
	}
	return SystemMetrics{
		MemPagesFree:     free,
		MemPagesActive:   active,
		MemPagesInactive: inactive,
		MemPagesWired:    wired,
		MemPressurePct:   pressure,
	}
}

// placeholders returns a comma-separated list of n SQL placeholder '?'s.
func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',', '?')
		} else {
			b = append(b, '?')
		}
	}
	return string(b)
}

// stringsToAny converts a []string to []any for use as variadic SQL args.
func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func (s *Store) sessionChainLink(sessionID string) (ChainLink, error) {
	var link ChainLink
	link.SessionID = sessionID
	s.db.QueryRow(`
		SELECT ss.project, COALESCE(ss.first_msg, ''), COALESCE(ss.last_msg, ''),
		       COALESCE(sm.topic, ''), COALESCE(sm.repo, '')
		FROM session_summary ss
		LEFT JOIN session_meta sm ON sm.session_id = ss.session_id
		WHERE ss.session_id = ?`, sessionID,
	).Scan(&link.Project, &link.FirstMsg, &link.LastMsg, &link.Topic, &link.Repo)
	return link, nil
}

// DefineTemplate upserts a named query template.
func (s *Store) DefineTemplate(name, description, queryText string, paramNames []string) error {
	if paramNames == nil {
		paramNames = []string{}
	}
	paramJSON, err := json.Marshal(paramNames)
	if err != nil {
		return fmt.Errorf("marshal param_names: %w", err)
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	_, err = s.db.Exec(`
		INSERT INTO query_templates (name, description, query_text, param_names, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(name) DO UPDATE SET
			description = excluded.description,
			query_text = excluded.query_text,
			param_names = excluded.param_names,
			updated_at = datetime('now')
	`, name, description, queryText, string(paramJSON))
	return err
}

// EvaluateTemplate looks up a template by name, substitutes parameters, and executes it.
func (s *Store) EvaluateTemplate(name string, params map[string]string) ([]map[string]any, error) {
	s.rwmu.RLock()
	var paramNamesJSON, queryText string
	err := s.db.QueryRow(
		`SELECT query_text, param_names FROM query_templates WHERE name = ?`, name,
	).Scan(&queryText, &paramNamesJSON)
	s.rwmu.RUnlock()
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("template %q not found", name)
		}
		return nil, err
	}

	var paramNames []string
	if err := json.Unmarshal([]byte(paramNamesJSON), &paramNames); err != nil {
		return nil, fmt.Errorf("parse param_names: %w", err)
	}

	// Validate all required params are provided.
	for _, p := range paramNames {
		if _, ok := params[p]; !ok {
			return nil, fmt.Errorf("missing parameter %q", p)
		}
	}

	// Substitute {{param}} placeholders.
	q := queryText
	for k, v := range params {
		q = strings.ReplaceAll(q, "{{"+k+"}}", v)
	}

	return s.Query(q)
}

// ListTemplates returns all stored query templates.
func (s *Store) ListTemplates() ([]QueryTemplate, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	rows, err := s.db.Query(`
		SELECT id, name, COALESCE(description, ''), query_text, param_names, created_at, updated_at
		FROM query_templates
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []QueryTemplate
	for rows.Next() {
		var t QueryTemplate
		var paramNamesJSON string
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.QueryText, &paramNamesJSON, &t.CreatedAt, &t.UpdatedAt); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(paramNamesJSON), &t.ParamNames); err != nil {
			t.ParamNames = []string{}
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// GitHubActivityResult holds a single PR or issue record for MCP tool output.
type GitHubActivityResult struct {
	Type      string `json:"type"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	State     string `json:"state"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	MergedAt  string `json:"merged_at,omitempty"`
	URL       string `json:"url"`
}

// ghPRJSON matches the JSON output of `gh pr list`.
type ghPRJSON struct {
	Number    int                    `json:"number"`
	Title     string                 `json:"title"`
	Body      string                 `json:"body"`
	State     string                 `json:"state"`
	Author    struct{ Login string } `json:"author"`
	CreatedAt string                 `json:"createdAt"`
	UpdatedAt string                 `json:"updatedAt"`
	MergedAt  string                 `json:"mergedAt"`
	URL       string                 `json:"url"`
}

// ghIssueJSON matches the JSON output of `gh issue list`.
type ghIssueJSON struct {
	Number    int                     `json:"number"`
	Title     string                  `json:"title"`
	Body      string                  `json:"body"`
	State     string                  `json:"state"`
	Author    struct{ Login string }  `json:"author"`
	CreatedAt string                  `json:"createdAt"`
	UpdatedAt string                  `json:"updatedAt"`
	URL       string                  `json:"url"`
	Labels    []struct{ Name string } `json:"labels"`
}

// SearchGitHubActivity searches GitHub PRs and issues with optional filters.
func (s *Store) SearchGitHubActivity(query string, repo string, state string, author string, activityType string, days int, limit int) ([]GitHubActivityResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 30
	}

	cutoff := fmt.Sprintf("datetime('now', '-%d days')", days)
	var results []GitHubActivityResult

	// Helper to build and execute query for one table.
	fetchTable := func(table, ftsTable, itemType string, cols string) error {
		// state filtering: "merged" only applies to PRs.
		if itemType == "issue" && state == "merged" {
			return nil
		}

		var q string
		var args []any

		if query != "" {
			ftsQuery := relaxQuery(query)
			q = fmt.Sprintf(`SELECT %s FROM %s t JOIN %s f ON f.rowid = t.id WHERE %s MATCH ?`,
				cols, table, ftsTable, ftsTable)
			args = append(args, ftsQuery)
			q += ` AND t.updated_at >= ` + cutoff
		} else {
			q = fmt.Sprintf(`SELECT %s FROM %s t WHERE t.updated_at >= `+cutoff, cols, table)
		}

		if repo != "" {
			q += ` AND t.repo LIKE ?`
			args = append(args, "%"+repo+"%")
		}
		if author != "" {
			q += ` AND t.author LIKE ?`
			args = append(args, "%"+author+"%")
		}
		if state != "" && state != "all" {
			if itemType == "pr" && state == "merged" {
				q += ` AND t.merged_at IS NOT NULL AND t.merged_at != ''`
			} else if state != "merged" {
				q += ` AND t.state = ?`
				args = append(args, state)
			}
		}

		if query != "" {
			q += ` ORDER BY rank`
		} else {
			q += ` ORDER BY t.updated_at DESC`
		}
		q += ` LIMIT ?`
		args = append(args, limit)

		rows, err := s.db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r GitHubActivityResult
			r.Type = itemType
			var mergedAt sql.NullString
			if itemType == "pr" {
				if err := rows.Scan(&r.Repo, &r.Number, &r.Title, &r.Body, &r.State,
					&r.Author, &r.CreatedAt, &r.UpdatedAt, &mergedAt, &r.URL); err != nil {
					continue
				}
				if mergedAt.Valid {
					r.MergedAt = mergedAt.String
				}
			} else {
				if err := rows.Scan(&r.Repo, &r.Number, &r.Title, &r.Body, &r.State,
					&r.Author, &r.CreatedAt, &r.UpdatedAt, &r.URL); err != nil {
					continue
				}
			}
			results = append(results, r)
		}
		return nil
	}

	prCols := `t.repo, t.pr_number, t.title, t.body, t.state, t.author, t.created_at, t.updated_at, t.merged_at, t.url`
	issueCols := `t.repo, t.issue_number, t.title, t.body, t.state, t.author, t.created_at, t.updated_at, t.url`

	if activityType == "" || activityType == "all" || activityType == "pr" {
		if err := fetchTable("github_prs", "github_prs_fts", "pr", prCols); err != nil {
			slog.Warn("github_prs search failed", "err", err)
		}
	}
	if activityType == "" || activityType == "all" || activityType == "issue" {
		if err := fetchTable("github_issues", "github_issues_fts", "issue", issueCols); err != nil {
			slog.Warn("github_issues search failed", "err", err)
		}
	}

	// Sort merged results by updated_at descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].UpdatedAt > results[j].UpdatedAt
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// PollGitHubActivity fetches recent PRs and issues for all known repos.
// Silently skips if gh is not installed.
func (s *Store) PollGitHubActivity() error {
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return nil // gh not installed
	}

	repos, err := s.ciRepos()
	if err != nil {
		return fmt.Errorf("ciRepos: %w", err)
	}

	for _, repo := range repos {
		if err := s.pollGitHubForRepo(ghPath, repo); err != nil {
			slog.Warn("GitHub activity poll failed", "repo", repo, "err", err)
		}
	}
	return nil
}

// pollGitHubForRepo fetches and upserts PRs and issues for a single repo.
func (s *Store) pollGitHubForRepo(ghPath, repo string) error {
	// Find the most recent updated_at for incremental fetches.
	s.rwmu.RLock()
	var lastPR, lastIssue string
	s.db.QueryRow(`SELECT MAX(updated_at) FROM github_prs WHERE repo = ?`, repo).Scan(&lastPR)
	s.db.QueryRow(`SELECT MAX(updated_at) FROM github_issues WHERE repo = ?`, repo).Scan(&lastIssue)
	s.rwmu.RUnlock()

	if err := s.fetchAndUpsertPRs(ghPath, repo, lastPR); err != nil {
		slog.Warn("PR fetch failed", "repo", repo, "err", err)
	}
	if err := s.fetchAndUpsertIssues(ghPath, repo, lastIssue); err != nil {
		slog.Warn("issue fetch failed", "repo", repo, "err", err)
	}
	return nil
}

// fetchAndUpsertPRs fetches PRs from GitHub and upserts into github_prs.
func (s *Store) fetchAndUpsertPRs(ghPath, repo, lastUpdated string) error {
	out, err := exec.Command(ghPath, "pr", "list",
		"--repo", repo,
		"--state", "all",
		"--json", "number,title,body,state,author,createdAt,updatedAt,mergedAt,url",
		"--limit", "100",
	).Output()
	if err != nil {
		return fmt.Errorf("gh pr list: %w", err)
	}

	var prs []ghPRJSON
	if err := json.Unmarshal(out, &prs); err != nil {
		return fmt.Errorf("parse gh pr output: %w", err)
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	for _, pr := range prs {
		// Skip if not newer than our last known update (incremental).
		if lastUpdated != "" && pr.UpdatedAt <= lastUpdated {
			continue
		}
		body := pr.Body
		if len(body) > 5000 {
			body = body[:4997] + "..."
		}
		state := strings.ToLower(pr.State)
		if pr.MergedAt != "" {
			state = "merged"
		}
		var mergedAt *string
		if pr.MergedAt != "" {
			mergedAt = &pr.MergedAt
		}
		_, err := s.db.Exec(`
			INSERT INTO github_prs (repo, pr_number, title, body, state, author, created_at, updated_at, merged_at, url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, pr_number) DO UPDATE SET
				title = excluded.title,
				body = excluded.body,
				state = excluded.state,
				merged_at = excluded.merged_at,
				updated_at = excluded.updated_at
		`, repo, pr.Number, pr.Title, body, state, pr.Author.Login,
			pr.CreatedAt, pr.UpdatedAt, mergedAt, pr.URL)
		if err != nil {
			slog.Warn("upsert github_pr failed", "repo", repo, "pr", pr.Number, "err", err)
		}
	}
	return nil
}

// fetchAndUpsertIssues fetches issues from GitHub and upserts into github_issues.
func (s *Store) fetchAndUpsertIssues(ghPath, repo, lastUpdated string) error {
	out, err := exec.Command(ghPath, "issue", "list",
		"--repo", repo,
		"--state", "all",
		"--json", "number,title,body,state,author,createdAt,updatedAt,url,labels",
		"--limit", "100",
	).Output()
	if err != nil {
		return fmt.Errorf("gh issue list: %w", err)
	}

	var issues []ghIssueJSON
	if err := json.Unmarshal(out, &issues); err != nil {
		return fmt.Errorf("parse gh issue output: %w", err)
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	for _, issue := range issues {
		// Skip if not newer than our last known update (incremental).
		if lastUpdated != "" && issue.UpdatedAt <= lastUpdated {
			continue
		}
		body := issue.Body
		if len(body) > 5000 {
			body = body[:4997] + "..."
		}
		// Build labels JSON array.
		labelNames := make([]string, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			labelNames = append(labelNames, l.Name)
		}
		labelsJSON, _ := json.Marshal(labelNames)

		_, err := s.db.Exec(`
			INSERT INTO github_issues (repo, issue_number, title, body, state, author, created_at, updated_at, url, labels)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, issue_number) DO UPDATE SET
				title = excluded.title,
				body = excluded.body,
				state = excluded.state,
				updated_at = excluded.updated_at,
				labels = excluded.labels
		`, repo, issue.Number, issue.Title, body, strings.ToLower(issue.State),
			issue.Author.Login, issue.CreatedAt, issue.UpdatedAt, issue.URL, string(labelsJSON))
		if err != nil {
			slog.Warn("upsert github_issue failed", "repo", repo, "issue", issue.Number, "err", err)
		}
	}
	return nil
}

// backfillGitHubActivity polls PRs and issues for all known repos at startup.
// Designed to run in a goroutine — does not block ingest.
func backfillGitHubActivity(s *Store) {
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		slog.Info("gh not found; skipping GitHub activity backfill")
		return
	}

	repos, err := s.ciRepos()
	if err != nil {
		slog.Warn("backfillGitHubActivity: ciRepos failed", "err", err)
		return
	}

	slog.Info("backfilling GitHub activity", "repos", len(repos))
	for _, repo := range repos {
		if err := s.pollGitHubForRepo(ghPath, repo); err != nil {
			slog.Warn("GitHub backfill failed", "repo", repo, "err", err)
		}
	}
	slog.Info("GitHub activity backfill complete", "repos", len(repos))
}

// backfillDecisions runs detectDecisions for all ingested sessions
// that don't yet have any decisions entries. Safe to call on every startup.
func backfillDecisions(db *sql.DB) {
	rows, err := db.Query(`
		SELECT DISTINCT sm.session_id, COALESCE(sm.repo, '')
		FROM session_meta sm
		WHERE NOT EXISTS (SELECT 1 FROM decisions d WHERE d.session_id = sm.session_id)`)
	if err != nil {
		slog.Warn("backfill decisions query failed", "err", err)
		return
	}
	defer rows.Close()

	type sessionRepo struct {
		id   string
		repo string
	}
	var sessions []sessionRepo
	for rows.Next() {
		var sr sessionRepo
		if rows.Scan(&sr.id, &sr.repo) == nil {
			sessions = append(sessions, sr)
		}
	}
	rows.Close()

	found := 0
	for _, sr := range sessions {
		detectDecisions(db, sr.id, sr.repo)
	}
	// Count total decisions found.
	db.QueryRow("SELECT COUNT(*) FROM decisions").Scan(&found)
	if found > 0 {
		slog.Info("backfilled decisions", "sessions_scanned", len(sessions), "decisions_found", found)
	}
}

// GitCommit holds a single indexed git commit.
type GitCommit struct {
	ID          int    `json:"id"`
	Repo        string `json:"repo"`
	CommitHash  string `json:"commit_hash"`
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	CommitDate  string `json:"commit_date"`
	Subject     string `json:"subject"`
	Body        string `json:"body,omitempty"`
}

// SearchCommits searches indexed git commits by keyword with optional repo, author, and days filters.
func (s *Store) SearchCommits(query string, repo string, author string, days int, limit int) ([]GitCommit, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 30
	}

	var q string
	var args []any
	cutoff := fmt.Sprintf("datetime('now', '-%d days')", days)

	if query != "" {
		ftsQuery := relaxQuery(query)
		q = `SELECT c.id, c.repo, c.commit_hash, c.author_name, c.author_email, c.commit_date, c.subject, c.body
			FROM git_commits c
			JOIN git_commits_fts f ON f.rowid = c.id
			WHERE git_commits_fts MATCH ?`
		args = append(args, ftsQuery)
		q += ` AND c.commit_date >= ` + cutoff
		if repo != "" {
			q += ` AND c.repo LIKE ?`
			args = append(args, "%"+repo+"%")
		}
		if author != "" {
			q += ` AND (c.author_name LIKE ? OR c.author_email LIKE ?)`
			args = append(args, "%"+author+"%", "%"+author+"%")
		}
		q += ` ORDER BY rank LIMIT ?`
	} else {
		q = `SELECT id, repo, commit_hash, author_name, author_email, commit_date, subject, body
			FROM git_commits WHERE commit_date >= ` + cutoff
		if repo != "" {
			q += ` AND repo LIKE ?`
			args = append(args, "%"+repo+"%")
		}
		if author != "" {
			q += ` AND (author_name LIKE ? OR author_email LIKE ?)`
			args = append(args, "%"+author+"%", "%"+author+"%")
		}
		q += ` ORDER BY commit_date DESC LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []GitCommit
	for rows.Next() {
		var c GitCommit
		if err := rows.Scan(&c.ID, &c.Repo, &c.CommitHash, &c.AuthorName, &c.AuthorEmail, &c.CommitDate, &c.Subject, &c.Body); err != nil {
			continue
		}
		results = append(results, c)
	}
	return results, nil
}

// ingestGitCommits fetches and indexes commits for a single repo root.
// If afterDate is non-empty, only commits after that date are fetched (incremental).
// If afterDate is empty, fetches the last 365 days (initial backfill).
func ingestGitCommits(db *sql.DB, repoPath, repoName string, afterDate string) int {
	// Verify this is actually a git repo.
	checkCmd := exec.Command("git", "-C", repoPath, "rev-parse", "--git-dir")
	if err := checkCmd.Run(); err != nil {
		return 0
	}

	var after string
	if afterDate != "" {
		after = afterDate
	} else {
		after = time.Now().AddDate(-1, 0, 0).Format(time.RFC3339)
	}

	// Use NUL as field separator, RS (0x1e) as record separator.
	// Format: hash NUL author_name NUL author_email NUL iso_date NUL subject NUL body RS
	gitArgs := []string{
		"-C", repoPath, "log",
		"--format=%H%x00%an%x00%ae%x00%aI%x00%s%x00%b%x1e",
		"--after=" + after,
	}
	cmd := exec.Command("git", gitArgs...)
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("git log failed", "repo", repoName, "err", err)
		return 0
	}

	if len(out) == 0 {
		return 0
	}

	tx, err := db.Begin()
	if err != nil {
		slog.Warn("git commits tx begin failed", "repo", repoName, "err", err)
		return 0
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO git_commits
		(repo, commit_hash, author_name, author_email, commit_date, subject, body)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		slog.Warn("git commits prepare failed", "repo", repoName, "err", err)
		return 0
	}
	defer stmt.Close()

	count := 0
	// Split on record separator (0x1e).
	records := strings.Split(string(out), "\x1e")
	for _, record := range records {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\x00", 6)
		if len(fields) < 5 {
			continue
		}
		hash := strings.TrimSpace(fields[0])
		authorName := strings.TrimSpace(fields[1])
		authorEmail := strings.TrimSpace(fields[2])
		commitDate := strings.TrimSpace(fields[3])
		subject := strings.TrimSpace(fields[4])
		body := ""
		if len(fields) == 6 {
			body = strings.TrimSpace(fields[5])
		}
		if hash == "" || subject == "" {
			continue
		}
		if _, err := stmt.Exec(repoName, hash, authorName, authorEmail, commitDate, subject, body); err != nil {
			slog.Warn("git commit insert failed", "repo", repoName, "hash", hash, "err", err)
			continue
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("git commits tx commit failed", "repo", repoName, "err", err)
		return 0
	}
	return count
}

// backfillGitCommits indexes git commit history for all known repos.
// For repos already partially indexed, it does an incremental fetch.
// For new repos, it fetches the last 365 days.
func backfillGitCommits(s *Store) {
	roots := s.knownRepoRoots()
	if len(roots) == 0 {
		return
	}

	totalNew := 0
	for _, rr := range roots {
		// Look up the most recent commit already indexed for this repo.
		var lastDate string
		s.db.QueryRow(
			`SELECT MAX(commit_date) FROM git_commits WHERE repo = ?`,
			rr.repo,
		).Scan(&lastDate) //nolint:errcheck

		// For incremental runs, pass the last indexed date.
		// For initial backfill, pass empty (ingestGitCommits will use 365 days ago).
		n := ingestGitCommits(s.db, rr.root, rr.repo, lastDate)
		totalNew += n
	}

	if totalNew > 0 {
		slog.Info("backfilled git commits", "new_commits", totalNew)
	}
}

// --- Image indexing ---

// ImageInfo holds metadata for a stored image.
type ImageInfo struct {
	ID           int    `json:"id"`
	ContentHash  string `json:"content_hash"`
	OriginalPath string `json:"original_path,omitempty"`
	MimeType     string `json:"mime_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	PixelFormat  string `json:"pixel_format"`
	ByteSize     int64  `json:"byte_size"`
	CreatedAt    string `json:"created_at"`
}

// ImageOccurrence links an image to a specific session/entry.
type ImageOccurrence struct {
	SessionID  string `json:"session_id"`
	SourceType string `json:"source_type"`
	OccurredAt string `json:"occurred_at"`
}

// ImageSearchResult is a single image search hit.
type ImageSearchResult struct {
	Image       ImageInfo         `json:"image"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	OCRText     string            `json:"ocr_text,omitempty"`
	MatchSource string            `json:"match_source,omitempty"` // "description", "ocr", "both", "semantic", "similar"
	Score       float64           `json:"score,omitempty"`        // cosine similarity for semantic/similar modes
	Occurrences []ImageOccurrence `json:"occurrences,omitempty"`
}

// SearchImages searches image descriptions and OCR text using FTS5, returning
// matching images with metadata and occurrence info.
// searchFields controls which indexes to query: "both" (default), "description", or "ocr".
func (s *Store) SearchImages(query string, repo string, session string, days int, limit int) ([]ImageSearchResult, error) {
	return s.SearchImagesFiltered(query, repo, session, days, limit, "both")
}

// SearchImagesFiltered is SearchImages with an explicit searchFields parameter.
func (s *Store) SearchImagesFiltered(query string, repo string, session string, days int, limit int, searchFields string) ([]ImageSearchResult, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 90
	}
	if searchFields == "" {
		searchFields = "both"
	}

	cutoff := fmt.Sprintf("datetime('now', '-%d days')", days)

	var repoFilter, sessionFilter string
	if repo != "" {
		repoFilter = `AND EXISTS (
			SELECT 1 FROM image_occurrences io2
			JOIN session_meta sm ON sm.session_id = io2.session_id
			WHERE io2.image_id = img.id AND sm.repo LIKE ?
		)`
	}
	if session != "" {
		sessionFilter = `AND EXISTS (
			SELECT 1 FROM image_occurrences io3
			WHERE io3.image_id = img.id AND io3.session_id LIKE ?
		)`
	}

	type imageHit struct {
		id       int64
		fromDesc bool
		fromOCR  bool
	}

	hitMap := make(map[int64]*imageHit)
	var orderedIDs []int64

	// Helper to collect hits from a given FTS table.
	runFTS := func(ftsTable, ftsCol, joinTable, joinCol string, fromDesc, fromOCR bool) error {
		if query == "" {
			return nil
		}
		ftsQuery := relaxQuery(query)
		q := fmt.Sprintf(`
			SELECT DISTINCT img.id
			FROM images img
			JOIN %s f ON f.rowid = (
				SELECT %s FROM %s WHERE %s = img.id LIMIT 1
			)
			JOIN image_occurrences io ON io.image_id = img.id
			WHERE %s MATCH ?
			AND io.occurred_at >= %s
			%s %s`,
			joinTable, joinCol, joinTable, "image_id",
			ftsTable,
			cutoff,
			repoFilter, sessionFilter,
		)
		var args []any
		args = append(args, ftsQuery)
		if repo != "" {
			args = append(args, "%"+repo+"%")
		}
		if session != "" {
			args = append(args, session+"%")
		}
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if rows.Scan(&id) != nil {
				continue
			}
			if h, ok := hitMap[id]; ok {
				if fromDesc {
					h.fromDesc = true
				}
				if fromOCR {
					h.fromOCR = true
				}
			} else {
				hitMap[id] = &imageHit{id: id, fromDesc: fromDesc, fromOCR: fromOCR}
				orderedIDs = append(orderedIDs, id)
			}
		}
		return nil
	}

	if query != "" {
		// Query each enabled FTS index.
		if searchFields == "both" || searchFields == "description" {
			if err := runFTS("image_descriptions_fts", "id", "image_descriptions", "image_descriptions_fts", true, false); err != nil {
				slog.Warn("image description FTS query failed", "err", err)
			}
		}
		if searchFields == "both" || searchFields == "ocr" {
			if err := runFTS("image_ocr_fts", "image_id", "image_ocr", "image_ocr_fts", false, true); err != nil {
				slog.Warn("image OCR FTS query failed", "err", err)
			}
		}
	} else {
		// No query — list recent images.
		q := `SELECT DISTINCT img.id FROM images img
			JOIN image_occurrences io ON io.image_id = img.id
			WHERE io.occurred_at >= ` + cutoff
		var args []any
		if repo != "" {
			q += " " + repoFilter
			args = append(args, "%"+repo+"%")
		}
		if session != "" {
			q += " " + sessionFilter
			args = append(args, session+"%")
		}
		q += " ORDER BY img.created_at DESC LIMIT ?"
		args = append(args, limit)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, fmt.Errorf("list images: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				hitMap[id] = &imageHit{id: id}
				orderedIDs = append(orderedIDs, id)
			}
		}
	}

	if len(orderedIDs) == 0 {
		return nil, nil
	}

	// Cap at limit.
	if len(orderedIDs) > limit {
		orderedIDs = orderedIDs[:limit]
	}

	// Fetch full image metadata + description + OCR text for each hit.
	var results []ImageSearchResult
	for _, id := range orderedIDs {
		hit := hitMap[id]
		var r ImageSearchResult
		var origPath sql.NullString
		if err := s.db.QueryRow(`
			SELECT img.id, img.content_hash, img.original_path, img.mime_type,
			       img.width, img.height, img.pixel_format, img.byte_size, img.created_at,
			       COALESCE(d.name,''), COALESCE(d.description,''), COALESCE(o.text,'')
			FROM images img
			LEFT JOIN image_descriptions d ON d.image_id = img.id
			LEFT JOIN image_ocr o ON o.image_id = img.id
			WHERE img.id = ?`, id).Scan(
			&r.Image.ID, &r.Image.ContentHash, &origPath, &r.Image.MimeType,
			&r.Image.Width, &r.Image.Height, &r.Image.PixelFormat, &r.Image.ByteSize,
			&r.Image.CreatedAt, &r.Name, &r.Description, &r.OCRText,
		); err != nil {
			continue
		}
		if origPath.Valid {
			r.Image.OriginalPath = origPath.String
		}
		// Determine match source.
		switch {
		case query == "":
			r.MatchSource = ""
		case hit.fromDesc && hit.fromOCR:
			r.MatchSource = "both"
		case hit.fromDesc:
			r.MatchSource = "description"
		case hit.fromOCR:
			r.MatchSource = "ocr"
		}
		results = append(results, r)
	}

	if len(results) == 0 {
		return nil, nil
	}

	// Fetch up to 3 occurrences per image.
	for i, r := range results {
		id := int64(r.Image.ID)
		occRows, err := s.db.Query(`
			SELECT io.session_id, io.source_type, io.occurred_at
			FROM image_occurrences io
			WHERE io.image_id = ?
			ORDER BY io.occurred_at DESC
			LIMIT 3`, id)
		if err != nil {
			continue
		}
		for occRows.Next() {
			var occ ImageOccurrence
			if occRows.Scan(&occ.SessionID, &occ.SourceType, &occ.OccurredAt) == nil {
				results[i].Occurrences = append(results[i].Occurrences, occ)
			}
		}
		occRows.Close()
	}

	return results, nil
}

// SearchImagesSemantic embeds the query text and runs k-NN against stored CLIP vectors,
// applying repo/session/days filters.
func (s *Store) SearchImagesSemantic(query string, repo string, session string, days int, limit int) ([]ImageSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 90
	}

	_, _, queryVec, err := runEmbedText(query)
	if err != nil {
		return nil, fmt.Errorf("embed query text: %w", err)
	}

	return s.knnImageSearch(queryVec, -1, repo, session, days, limit, "semantic")
}

// SearchImagesSimilar loads the embedding for the given imageID and finds visually similar images.
func (s *Store) SearchImagesSimilar(similarTo int, repo string, session string, days int, limit int) ([]ImageSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if days <= 0 {
		days = 90
	}

	s.rwmu.RLock()
	var blob []byte
	err := s.db.QueryRow(`SELECT vector FROM image_embeddings WHERE image_id = ? AND error IS NULL`, similarTo).Scan(&blob)
	s.rwmu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("load embedding for image %d: %w", similarTo, err)
	}

	refVec := decodeVector(blob)
	return s.knnImageSearch(refVec, int64(similarTo), repo, session, days, limit, "similar")
}

// knnImageSearch performs brute-force k-NN over stored embeddings with filters.
// excludeID of -1 means no exclusion; a positive value excludes that image ID from results.
func (s *Store) knnImageSearch(queryVec []float32, excludeID int64, repo string, session string, days int, limit int, matchSource string) ([]ImageSearchResult, error) {
	cutoff := fmt.Sprintf("datetime('now', '-%d days')", days)

	// First collect candidate image IDs via SQL filters.
	var filterArgs []any
	filterQ := `SELECT DISTINCT img.id FROM images img
		JOIN image_occurrences io ON io.image_id = img.id
		WHERE io.occurred_at >= ` + cutoff

	if repo != "" {
		filterQ += ` AND EXISTS (
			SELECT 1 FROM image_occurrences io2
			JOIN session_meta sm ON sm.session_id = io2.session_id
			WHERE io2.image_id = img.id AND sm.repo LIKE ?
		)`
		filterArgs = append(filterArgs, "%"+repo+"%")
	}
	if session != "" {
		filterQ += ` AND EXISTS (
			SELECT 1 FROM image_occurrences io3
			WHERE io3.image_id = img.id AND io3.session_id LIKE ?
		)`
		filterArgs = append(filterArgs, session+"%")
	}
	if excludeID > 0 {
		filterQ += ` AND img.id != ?`
		filterArgs = append(filterArgs, excludeID)
	}

	s.rwmu.RLock()
	rows, err := s.db.Query(filterQ, filterArgs...)
	if err != nil {
		s.rwmu.RUnlock()
		return nil, fmt.Errorf("filter images for k-NN: %w", err)
	}
	var candidateIDs []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			candidateIDs = append(candidateIDs, id)
		}
	}
	rows.Close()

	// Determine the model to use: pick the most common model in the embeddings table.
	var model string
	s.db.QueryRow(`SELECT model FROM image_embeddings WHERE error IS NULL GROUP BY model ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&model) //nolint:errcheck

	candidates, err := loadCandidateEmbeddings(s.db, model, candidateIDs)
	s.rwmu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// k-NN ranking.
	topIDs := knnSearch(queryVec, candidates, limit)

	// Build a score map for the results.
	scoreMap := make(map[int64]float32, len(candidates))
	for _, c := range candidates {
		scoreMap[c.imageID] = cosineSimilarity(queryVec, c.vector)
	}

	// Fetch full metadata for the top results.
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	var results []ImageSearchResult
	for _, id := range topIDs {
		var r ImageSearchResult
		var origPath sql.NullString
		if err := s.db.QueryRow(`
			SELECT img.id, img.content_hash, img.original_path, img.mime_type,
			       img.width, img.height, img.pixel_format, img.byte_size, img.created_at,
			       COALESCE(d.name,''), COALESCE(d.description,''), COALESCE(o.text,'')
			FROM images img
			LEFT JOIN image_descriptions d ON d.image_id = img.id
			LEFT JOIN image_ocr o ON o.image_id = img.id
			WHERE img.id = ?`, id).Scan(
			&r.Image.ID, &r.Image.ContentHash, &origPath, &r.Image.MimeType,
			&r.Image.Width, &r.Image.Height, &r.Image.PixelFormat, &r.Image.ByteSize,
			&r.Image.CreatedAt, &r.Name, &r.Description, &r.OCRText,
		); err != nil {
			continue
		}
		if origPath.Valid {
			r.Image.OriginalPath = origPath.String
		}
		r.MatchSource = matchSource
		r.Score = float64(scoreMap[id])

		// Fetch up to 3 occurrences.
		occRows, err := s.db.Query(`
			SELECT io.session_id, io.source_type, io.occurred_at
			FROM image_occurrences io
			WHERE io.image_id = ?
			ORDER BY io.occurred_at DESC
			LIMIT 3`, id)
		if err == nil {
			for occRows.Next() {
				var occ ImageOccurrence
				if occRows.Scan(&occ.SessionID, &occ.SourceType, &occ.OccurredAt) == nil {
					r.Occurrences = append(r.Occurrences, occ)
				}
			}
			occRows.Close()
		}

		results = append(results, r)
	}

	return results, nil
}
