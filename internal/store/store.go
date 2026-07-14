// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package store provides a searchable transcript index across all
// Claude Code sessions. It ingests JSONL files from ~/.claude/projects/
// and maintains a realtime FTS5 index in SQLite.
package store

import (
	"bufio"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/sqldeep/go/sqldeep"
	"github.com/marcelocantos/sqlift/go/sqlift"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// NoncePrefix is the prefix for self-identification nonces.
const NoncePrefix = "mnemo:self:"

// CompactorMarker prefixes the prompt of every claudia-spawned
// compaction run (🎯T72). It lands verbatim as the first user message
// in the spawned session's transcript, so ingest can flag the session
// (session_meta.compactor_internal = 1) and the candidate query excludes
// it. This is the precise recursion guard: unlike the old excludeCWD
// prefix check it never false-excludes a genuine dev session that
// happens to share the mnemo repo cwd — only sessions whose first
// message literally carries the marker are skipped.
const CompactorMarker = "[mnemo:compactor:v1]"

// LegacyCompactorSignature is the leading text of pre-🎯T72 compaction
// prompts, which had no explicit marker (the summariser SystemPrompt
// began with this sentence). Detecting it lets ingest flag the large
// backlog of historical compactor sessions so they are not swept into
// the candidate set the instant the excludeCWD prefix check is removed.
const LegacyCompactorSignature = "You are a session compactor."

// IsCompactorMarker reports whether a message's text marks it as the
// opening prompt of a mnemo compaction run — either the current marker
// or the legacy signature. Used at ingest to set compactor_internal.
func IsCompactorMarker(text string) bool {
	return strings.HasPrefix(text, CompactorMarker) ||
		strings.HasPrefix(text, LegacyCompactorSignature)
}

// Store is a searchable index of Claude Code transcripts.
type Store struct {
	writeDB    *sql.DB // SetMaxOpenConns(1) — Go-pool serialises writers
	readDB     *sql.DB // default pool — many concurrent readers
	dbPath     string
	projectDir string

	mu      sync.Mutex
	offsets map[string]int64 // file path → last read offset

	// vaultPath is the configured vault root, mirrored from
	// registry/config (🎯T68.6) so the vault divergence gatherer and
	// the vault GC can find it without re-reading config. "" when the
	// user has not configured a vault. Guarded by mu.
	vaultPath string

	rootsMu sync.RWMutex // protects in-memory state: workspaceRoots, extraProjectDirs, synthesisRoots

	// workspaceRoots is the set of filesystem roots under which repo-level
	// streams discover repos. Mutated only via SetWorkspaceRoots, read
	// under rootsMu.RLock by the repoRoots walker.
	workspaceRoots []string

	// extraProjectDirs are additional Claude Code project directories
	// to ingest beyond projectDir. Used for cross-platform transcript
	// ingest (🎯T15) — e.g. a Windows VM's Claude projects exposed via
	// SMB mount. Mutated only via SetExtraProjectDirs.
	extraProjectDirs []string

	// synthesisRoots are filesystem roots walked by IngestSynthesis to
	// index taxonomy-tagged synthesis docs (🎯T34). Unlike
	// workspaceRoots, entries here do not require a .git marker — suits
	// non-repo planning spaces. Mutated only via SetSynthesisRoots.
	synthesisRoots []string

	// codexRoots are the candidate Codex rollout roots (~/.codex/sessions,
	// ~/.codex/archived_sessions) ingested alongside the Claude project
	// dirs (🎯T99). Configured at construction (registry.ForUser) rather
	// than discovered globally, so New-based tests stay hermetic and
	// don't pull in the developer's real ~/.codex. Mutated only via
	// SetCodexRoots; existence is checked lazily in codexDirs.
	codexRoots []string

	// grokRoots are the candidate Grok session roots (~/.grok/sessions)
	// ingested alongside Claude/Codex (🎯T110). Only updates.jsonl under
	// these roots is indexed. Mutated only via SetGrokRoots; existence
	// is checked lazily in grokDirs.
	grokRoots []string

	// todoGlobs are extra repo-relative globs that IngestTodos matches
	// when discovering TODO files, beyond the default TODO.md / todos.md
	// names (🎯T78). Mutated only via SetTodoGlobs, read under
	// rootsMu.RLock.
	todoGlobs []string

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

	// exclusions are directory subtrees that ingest walkers must skip
	// — currently used to keep the configured vault_path out of the
	// docs / synthesis / workspace walkers, which would otherwise
	// re-ingest mnemo's own generated output and grow the index
	// without bound on every Sync cycle. Populated via
	// RegisterExcludedPath, queried via IsExcluded.
	exclusions *exclusionRegistry

	// lastWriteAt is the unix-nano timestamp of the most recent ingest
	// or backfill that committed rows. Updated by NoteActivity; read
	// by the backup worker via LastWriteAt() to detect a quiescent
	// window before taking a snapshot. Tracked in-memory only — a
	// daemon restart resets it to startup time, which is conservative
	// (worker will wait the full quiescence period before its first
	// backup attempt). Atomic so reads and writes don't need rootsMu.
	lastWriteAt atomic.Int64
}

// NoteActivity records that a write happened just now. The backup worker
// reads this via LastWriteAt to detect a quiescent window before snapshotting.
// Call from any ingest / backfill path that commits rows. Cheap (atomic
// store of an int64).
func (s *Store) NoteActivity() {
	s.lastWriteAt.Store(time.Now().UnixNano())
}

// LastWriteAt returns the wall-clock time of the most recent NoteActivity
// call, or the zero Time if nothing has been recorded since startup.
func (s *Store) LastWriteAt() time.Time {
	ns := s.lastWriteAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// DBPath returns the filesystem path of the underlying SQLite database.
// Used by the backup worker to point sqlift / VACUUM INTO at the right
// file without re-deriving the path.
func (s *Store) DBPath() string {
	return s.dbPath
}

// SetWorkspaceRoots configures the filesystem roots under which repo-
// level ingest streams discover repositories. Call this once after
// Store.New and before any Ingest* call. Tests inject a temp directory;
// production loads from ~/.mnemo/config.json.
func (s *Store) SetWorkspaceRoots(roots []string) {
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
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
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	if len(dirs) == 0 {
		s.extraProjectDirs = nil
		return
	}
	s.extraProjectDirs = append(s.extraProjectDirs[:0:0], dirs...)
}

// SetCodexRoots configures the Codex rollout roots ingested alongside
// the Claude project dirs (🎯T99). Pass the candidate directories
// (CodexRootsFor); existence is checked lazily by codexDirs so roots
// created after startup (Codex installed later) are still picked up.
// Call once after Store.New.
func (s *Store) SetCodexRoots(roots []string) {
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	if len(roots) == 0 {
		s.codexRoots = nil
		return
	}
	s.codexRoots = append(s.codexRoots[:0:0], roots...)
}

// codexDirs returns the configured Codex rollout roots that currently
// exist on disk. Empty when Codex roots weren't configured (e.g. New-
// based tests) or ~/.codex isn't present, making the feature a no-op.
func (s *Store) codexDirs() []string {
	s.rootsMu.RLock()
	roots := append([]string(nil), s.codexRoots...)
	s.rootsMu.RUnlock()
	var out []string
	for _, d := range roots {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// SetGrokRoots configures the Grok session roots ingested alongside
// the Claude project dirs (🎯T110). Pass the candidate directories
// (GrokRootsFor); existence is checked lazily by grokDirs so roots
// created after startup are still picked up. Call once after Store.New.
func (s *Store) SetGrokRoots(roots []string) {
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	if len(roots) == 0 {
		s.grokRoots = nil
		return
	}
	s.grokRoots = append(s.grokRoots[:0:0], roots...)
}

// grokDirs returns the configured Grok session roots that currently
// exist on disk. Empty when not configured or ~/.grok isn't present.
func (s *Store) grokDirs() []string {
	s.rootsMu.RLock()
	roots := append([]string(nil), s.grokRoots...)
	s.rootsMu.RUnlock()
	var out []string
	for _, d := range roots {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// SetSynthesisRoots configures the filesystem roots walked by
// IngestSynthesis to index taxonomy-tagged synthesis docs (🎯T34).
// Unlike SetWorkspaceRoots, these roots are walked without a .git
// requirement, so they may point at non-repo planning spaces such as
// ~/think. Call once after Store.New.
func (s *Store) SetSynthesisRoots(roots []string) {
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	if len(roots) == 0 {
		s.synthesisRoots = nil
		return
	}
	s.synthesisRoots = append(s.synthesisRoots[:0:0], roots...)
}

// SetTodoGlobs configures extra repo-relative globs that IngestTodos
// matches when discovering TODO files, beyond the default TODO.md /
// todos.md names (🎯T78). Call once after Store.New.
func (s *Store) SetTodoGlobs(globs []string) {
	s.rootsMu.Lock()
	defer s.rootsMu.Unlock()
	if len(globs) == 0 {
		s.todoGlobs = nil
		return
	}
	s.todoGlobs = append(s.todoGlobs[:0:0], globs...)
}

// projectDirs returns the full list of project directories to scan:
// the primary projectDir followed by any extras configured via
// SetExtraProjectDirs. The returned slice is a defensive copy.
func (s *Store) projectDirs() []string {
	s.rootsMu.RLock()
	defer s.rootsMu.RUnlock()
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
	// Summary is the first non-blank, non-heading sentence of the
	// repo's root CLAUDE.md, capped at ~120 chars. Empty when the
	// repo has no indexed CLAUDE.md. Auto-refreshes on next ingest
	// when the file changes.
	Summary string `json:"summary,omitempty"`
	// LastCommit is the timestamp of the most recent commit on any
	// branch indexed for this repo, in the same format as
	// LastActivity (UTC, second-precision). Empty when git history
	// hasn't been indexed for this repo.
	LastCommit string `json:"last_commit,omitempty"`
	// SummaryVerdict is the latest LLM-review verdict for this
	// repo's CLAUDE.md summary (🎯T41). One of "current", "stale",
	// "rewritten", or empty when no review has run. Populated by
	// the reviewer worker when its cheap-signal trigger fires.
	SummaryVerdict string `json:"summary_verdict,omitempty"`
	// SummaryReviewedAt is the timestamp of SummaryVerdict.
	SummaryReviewedAt string `json:"summary_reviewed_at,omitempty"`
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
	Days int `json:"days"`
	// Diagnostics is the transcript-ingest freshness/lag block (🎯T75):
	// now_utc, freshness lag, per-stream divergence, per-source coverage,
	// and (when a repo filter is supplied) a repo-specific section.
	// Additive — existing consumers can ignore it.
	Diagnostics *IngestDiagnostics `json:"diagnostics,omitempty"`
	Repos       []RepoStatus       `json:"repos"`
	Streams     []BackfillStatus   `json:"streams,omitempty"`
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

// UsageRow holds aggregated token usage for a single group (date, model, repo, session, or block).
type UsageRow struct {
	Period              string  `json:"period"`
	Model               string  `json:"model,omitempty"`
	Repo                string  `json:"repo,omitempty"`
	SessionID           string  `json:"session_id,omitempty"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	Messages            int     `json:"messages"`
	CostUSD             float64 `json:"cost_usd"`
	// Source indicates how cost was determined: "estimated" (from token
	// counts), "reconciled" (from Anthropic Admin API), or "mixed" (both
	// within this aggregation window).
	Source string `json:"source"`
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
	Days       int         `json:"days,omitempty"`
	Since      string      `json:"since,omitempty"`
	Until      string      `json:"until,omitempty"`
	Rows       []UsageRow  `json:"rows"`
	Total      UsageRow    `json:"total"`
	HourlyRate *HourlyRate `json:"hourly_rate,omitempty"`
	// Freshness is the timestamp of the most-recently ingested assistant
	// message (in RFC3339). Consumers polling for near-realtime data can
	// use this to bound indexer lag.
	Freshness string `json:"freshness,omitempty"`
}

// UsageParams gathers all filter and grouping parameters for Usage queries.
type UsageParams struct {
	Days       int    // Recency window in days (default 30). Ignored when Since/Until are set.
	Since      string // RFC3339 lower bound (inclusive). Overrides Days when set.
	Until      string // RFC3339 upper bound (inclusive). Defaults to now when only Since is set.
	RepoFilter string
	Model      string
	GroupBy    string // "day" | "model" | "repo" | "session" | "block"
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

func openDB(dbPath string, writer bool) (*sql.DB, error) {
	// The read pool uses a driver that installs a read-only authorizer
	// (see rodriver.go): mnemo_query runs arbitrary client SQL, and
	// PRAGMA query_only=1 alone is bypassable (🎯T103) and does not gate
	// ATTACH (🎯T106). The writer keeps the stock driver.
	driver := "sqlite3"
	if !writer {
		driver = readOnlyDriverName
	}
	db, err := sql.Open(driver, dbPath)
	if err != nil {
		return nil, err
	}
	if writer {
		db.SetMaxOpenConns(1)
	}
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

// applySchema brings the live DB at dbPath to the shape declared in
// schema.sql. Uses sqlift under ApplyOptions{} (= AllowNone): only pure
// additive changes are permitted (CREATE TABLE, ADD COLUMN, CREATE
// INDEX/VIEW/TRIGGER/VIRTUAL TABLE, and trigger body modifications).
// Anything else — drops, rebuilds, loosening, data-dependent changes —
// is rejected per the append-only schema policy in CLAUDE.md.
//
// The sqlift handle is opened exclusively for the migration and closed
// before returning so the caller can reopen with its own PRAGMA settings.
func applySchema(dbPath string) error {
	// Fast path: a brand-new / empty database needs no migration diffing.
	// sqlift's parse/extract/diff/apply only earns its keep when *upgrading*
	// an existing schema; for a fresh DB the desired schema can be created
	// by executing schema.sql directly. This avoids re-parsing and
	// re-diffing the full 42 KB schema on every store.New() — the common
	// case for tests (a fresh DB per test, 123+ in internal/store alone)
	// and for a first-run install — and is dramatically cheaper than the
	// cgo sqlift path, especially on Windows (🎯T90).
	if created, err := applyFreshSchema(dbPath); err != nil {
		return err
	} else if created {
		return nil
	}

	sdb, err := sqlift.Open(dbPath)
	if err != nil {
		return fmt.Errorf("sqlift open: %w", err)
	}
	defer sdb.Close()

	current, err := sqlift.Extract(sdb)
	if err != nil {
		return fmt.Errorf("sqlift extract: %w", err)
	}
	desired, err := sqlift.Parse(schemaSQL)
	if err != nil {
		return fmt.Errorf("sqlift parse schema.sql: %w", err)
	}
	plan, err := sqlift.Diff(current, desired)
	if err != nil {
		return fmt.Errorf("sqlift diff: %w", err)
	}
	if plan.Empty() {
		// No diff — nothing to back up before, nothing to apply.
		return nil
	}

	// Pre-migration backup. Cheap insurance even though AllowNone gates
	// reject everything destructive: if a future sqlift bug or an
	// unexpected interaction at apply time corrupts the live DB, this
	// snapshot is the rollback point. Tagged pre-migration so the daily
	// worker's retention GC can identify it; sharing the daily pool per
	// 🎯T61 design.
	//
	// Skipped on a fresh DB (no existing tables) — there's nothing to
	// protect, and every test using t.TempDir hits this path so we'd
	// pay backup.Backup's two short-lived sql.DB opens per test for no
	// benefit. On a real upgrade, current.Tables is populated and the
	// backup fires.
	//
	// Backup uses its own read-only sqlite connection; sdb stays open
	// throughout. Earlier versions of this hook closed sdb before the
	// backup and reopened after — on Windows that re-open deadlocked
	// because mattn/go-sqlite3's file handle release is asynchronous
	// (NTFS file-lock release lags Close() return). SQLite is fine with
	// the writer connection idle while a separate reader takes a
	// shared lock for VACUUM INTO, so the original close-and-reopen
	// was unnecessary on every platform — Linux/macOS just tolerated
	// it where Windows didn't.
	if len(current.Tables) > 0 {
		backupDir := filepath.Join(filepath.Dir(dbPath), "backups")
		if err := os.MkdirAll(backupDir, 0o755); err != nil {
			slog.Warn("backup dir create failed; proceeding without pre-migration backup",
				"dir", backupDir, "err", err)
		} else {
			destPath := filepath.Join(backupDir,
				backup.Filename(backup.TagPreMigration, time.Now()))
			res, berr := backup.Backup(dbPath, destPath)
			if berr != nil {
				slog.Warn("pre-migration backup failed; proceeding with migration anyway",
					"err", berr)
			} else {
				slog.Info("pre-migration backup written",
					"path", res.Path,
					"raw_mb", res.RawSize/(1<<20),
					"gz_mb", res.GzippedSize/(1<<20),
					"elapsed", res.Elapsed.Round(time.Second))
			}
		}
	}

	if err := sqlift.Apply(sdb, plan, sqlift.ApplyOptions{Allow: sqlift.AllowNone}); err != nil {
		return err
	}

	// 🎯T93: refresh planner statistics after a schema change. A migration
	// that adds an index leaves it without sqlite_stat1 data, and SQLite's
	// cost model will keep choosing the old (worse) index until ANALYZE
	// runs — e.g. the usage covering index is ignored, reverting to a full
	// assistant-table scan (~2.8s vs ~0.1s). Runs only here, on a real
	// migration (rare; this path already took a pre-migration backup), so
	// it is not a per-startup cost. Best-effort: a failure only costs
	// planner stats, never correctness.
	analyzeForPlanner(dbPath)
	return nil
}

// Optimize runs `PRAGMA optimize`, SQLite's lightweight, self-tuning
// statistics maintenance (🎯T93). It analyses only the tables whose
// statistics have drifted enough to matter (including tables that have
// never been analysed), so it is cheap to call periodically and on a
// long-running daemon keeps the planner's index choices correct as the
// DB grows. Complements the one-shot post-migration ANALYZE: that gives a
// newly-added index immediate stats; this keeps them fresh over time and
// covers fresh installs (which take the no-migration schema path). Runs on
// the writer connection (PRAGMA optimize records analysis results).
// Best-effort: a failure only affects planner quality.
func (s *Store) Optimize() {
	if s.writeDB == nil {
		return
	}
	if _, err := s.writeDB.Exec("PRAGMA optimize"); err != nil {
		slog.Warn("PRAGMA optimize failed; planner stats may drift", "err", err)
	}
}

// analyzeForPlanner runs ANALYZE so the query planner has up-to-date
// index statistics. Opens its own short-lived connection (mirroring the
// backup hook) and is best-effort. Called after a schema migration adds
// or changes indexes (🎯T93).
func analyzeForPlanner(dbPath string) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		slog.Warn("post-migration ANALYZE: open failed; planner stats may be stale", "err", err)
		return
	}
	defer db.Close()
	if _, err := db.Exec("ANALYZE"); err != nil {
		slog.Warn("post-migration ANALYZE failed; planner stats may be stale", "err", err)
		return
	}
	slog.Info("post-migration ANALYZE complete (planner statistics refreshed)")
}

// applyFreshSchema creates the full schema by executing schema.sql
// directly when dbPath has no user-defined objects yet, bypassing
// sqlift entirely (🎯T90). Returns (true, nil) when it created the
// schema, (false, nil) when the DB already has objects (the caller then
// runs the sqlift-mediated migration), or an error.
//
// schema.sql is pure additive DDL ordered so each CREATE's dependencies
// precede it (base tables before their FTS mirrors and triggers), so a
// single multi-statement Exec in file order reproduces exactly what
// sqlift would build for a fresh DB. The emptiness guard means we only
// take this path when nothing exists, so the absence of IF NOT EXISTS in
// schema.sql is not a problem. Existing DBs fall through to sqlift, which
// keeps owning the upgrade contract under AllowNone.
//
// One short-lived connection does both the probe and the create, to
// avoid the close-then-reopen file-lock churn that mattn/go-sqlite3
// exhibits on Windows (see the pre-migration backup note above).
func applyFreshSchema(dbPath string) (bool, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return false, fmt.Errorf("open for fresh-schema check: %w", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type IN ('table', 'view', 'trigger', 'index')
		   AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("inspect existing schema: %w", err)
	}
	if n > 0 {
		return false, nil // existing schema — defer to sqlift's migration
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		return false, fmt.Errorf("apply schema.sql directly: %w", err)
	}
	return true, nil
}

// New creates or opens a transcript store.
func New(dbPath, projectDir string) (*Store, error) {
	// Apply schema before opening the long-lived connection. Holding
	// sqlift's connection separately keeps PRAGMA setup on the mnemo
	// handle isolated from migration.
	if err := applySchema(dbPath); err != nil {
		// Acceptance criterion 6 of 🎯T49: an older binary against a
		// newer mnemo.db must read without crashing. sqlift rejects the
		// implied destructive/rebuild ops; we log and proceed. Writes
		// against unknown schema will fail at SQLite level, which is
		// the expected degraded-mode signal — reads continue to work.
		slog.Warn("schema apply rejected; continuing without migration (older binary vs newer DB?)",
			"db", dbPath, "err", err)
	}

	writeDB, err := openDB(dbPath, true)
	if err != nil {
		return nil, err
	}
	readDB, err := openDB(dbPath, false)
	if err != nil {
		writeDB.Close()
		return nil, err
	}

	// Backfill session_meta for sessions without metadata by
	// re-reading the first entry of each JSONL file.
	backfillSessionMeta(writeDB, projectDir)

	// 🎯T72: populate compactions_fts for compaction rows that predate
	// the FTS table (the compactions_ai trigger only fires on new
	// inserts). Idempotent — a no-op once the index and table counts
	// agree, so steady-state boots pay only two COUNT(*) queries.
	healCompactionsFTS(writeDB)

	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	s := &Store{
		writeDB:    writeDB,
		readDB:     readDB,
		dbPath:     dbPath,
		projectDir: projectDir,
		offsets:    make(map[string]int64),
		imageSem:   make(chan struct{}, n),
		exclusions: &exclusionRegistry{},
	}

	rows, err := readDB.Query("SELECT path, offset FROM ingest_state")
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

// Close closes the store.
func (s *Store) Close() error {
	// Drain order matters (🎯T97.1): quiesce the read pool first, then
	// checkpoint the writer, then close the writer. TRUNCATE only fully
	// resets the -wal when no other connection holds a WAL read lock, so
	// closing readDB before checkpointing gives the truncate the best
	// chance to leave a zero-length -wal behind. A busy or failed
	// checkpoint is logged but never fails Close — the DB still closes
	// cleanly and crash-only recovery replays any residual frames.
	rerr := s.readDB.Close()

	if res, err := s.Checkpoint(); err != nil {
		slog.Warn("wal checkpoint on close failed", "db", s.dbPath, "err", err)
	} else if res.Busy != 0 {
		slog.Warn("wal checkpoint on close busy; -wal left for crash recovery",
			"db", s.dbPath, "busy", res.Busy, "log", res.Log, "checkpointed", res.Checkpointed)
	} else {
		slog.Info("wal checkpoint on close",
			"db", s.dbPath, "busy", res.Busy, "log", res.Log, "checkpointed", res.Checkpointed)
	}

	werr := s.writeDB.Close()
	if rerr != nil {
		return rerr
	}
	return werr
}

// CheckpointResult holds the three integers PRAGMA wal_checkpoint returns.
// Busy is 1 when the checkpoint could not run to completion because another
// connection held a read lock on the WAL; Log is the number of frames in the
// write-ahead log; Checkpointed is the number of frames moved back into the
// main database. After a successful TRUNCATE (Busy == 0) the -wal file has
// been flushed into the db and truncated to zero bytes.
type CheckpointResult struct {
	Busy         int
	Log          int
	Checkpointed int
}

// Checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) on the writer connection,
// flushing the write-ahead log into the main database file and truncating the
// -wal to zero. It is the durable-shutdown primitive for 🎯T97.1: after a
// clean drain the next startup replays no WAL. Because TRUNCATE only resets
// the WAL when no other connection holds a read lock, callers that want a
// zero-length -wal should quiesce the read pool (close readDB) first; a
// non-zero Busy signals the reset was blocked and the WAL was left in place
// for crash recovery.
func (s *Store) Checkpoint() (CheckpointResult, error) {
	var r CheckpointResult
	if s.writeDB == nil {
		return r, nil
	}
	row := s.writeDB.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)")
	if err := row.Scan(&r.Busy, &r.Log, &r.Checkpointed); err != nil {
		return r, fmt.Errorf("wal_checkpoint(TRUNCATE): %w", err)
	}
	return r, nil
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
			if err := s.ingestMemoryFileImpl(f, project); err != nil {
				slog.Error("ingest memory failed", "file", f, "err", err)
				continue
			}
			count++
		}
	}
	slog.Info("ingested memories", "count", count)
	return nil
}

// ingestMemoryFile ingests a single memory file.
func (s *Store) ingestMemoryFile(path string) error {

	// Derive project from path: .../projects/<project>/memory/<file>.md
	dir := filepath.Dir(path)
	project := filepath.Base(filepath.Dir(dir))
	return s.ingestMemoryFileImpl(path, project)
}

// ingestMemoryFileImpl ingests a single memory file by path and project name.
func (s *Store) ingestMemoryFileImpl(path, project string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// File was deleted — remove from index.
			s.writeDB.Exec("DELETE FROM memories WHERE file_path = ?", path)
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

	_, err = s.writeDB.Exec(`
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

// deleteMemoryFile removes a memory file from the index.
func (s *Store) deleteMemoryFile(path string) {
	s.writeDB.Exec("DELETE FROM memories WHERE file_path = ?", path)
}

// SearchMemories searches across all indexed memory files.
func (s *Store) SearchMemories(query string, memType string, project string, limit int) ([]MemoryInfo, error) {

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
	rows, err := s.readDB.Query(q, args...)
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

// GetMemory retrieves a single named memory file for a project.
// project is matched as a substring of the stored project value (consistent
// with SearchMemories). name is matched case-insensitively as a substring
// of either the frontmatter name field or the file base name (without .md).
// Returns nil without error when the project or memory is not found.
func (s *Store) GetMemory(project, name string) (*MemoryInfo, error) {

	if project == "" {
		return nil, fmt.Errorf("project is required")
	}
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	// Retrieve all memories for the project, then match name in Go.
	q := `SELECT id, project, file_path, name, description, memory_type, content, updated_at
		FROM memories
		WHERE project LIKE ?
		ORDER BY updated_at DESC`
	candidates, err := s.queryMemories(q, "%"+project+"%")
	if err != nil {
		return nil, err
	}

	nameLower := strings.ToLower(name)
	for i := range candidates {
		m := &candidates[i]
		// Match against frontmatter name or file base name (stem).
		stem := strings.TrimSuffix(filepath.Base(m.FilePath), ".md")
		if strings.Contains(strings.ToLower(m.Name), nameLower) ||
			strings.Contains(strings.ToLower(stem), nameLower) {
			return m, nil
		}
	}
	return nil, nil
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
	home, err := EffectiveHome()
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

	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return err
	}

	count := 0
	for _, f := range files {
		if err := s.ingestSkillFile(f); err != nil {
			slog.Error("ingest skill failed", "file", f, "err", err)
			continue
		}
		count++
	}
	slog.Info("ingested skills", "count", count)
	return nil
}

// ingestSkillFile ingests a single skill file.
func (s *Store) ingestSkillFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeDB.Exec("DELETE FROM skills WHERE file_path = ?", path)
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
	_, err = s.writeDB.Exec(`
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

// deleteSkillFile removes a skill file from the index.
func (s *Store) deleteSkillFile(path string) {
	s.writeDB.Exec("DELETE FROM skills WHERE file_path = ?", path)
}

// SearchSkills searches across all indexed skill files.
func (s *Store) SearchSkills(query string, limit int) ([]SkillInfo, error) {

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
	rows, err := s.readDB.Query(q, args...)
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

	roots := s.knownRepoRootsLocked()
	indexed, onDisk := 0, 0
	for _, rr := range roots {
		claudePath := filepath.Join(rr.root, "CLAUDE.md")
		if _, err := os.Stat(claudePath); err != nil {
			continue
		}
		onDisk++
		if err := s.ingestClaudeConfigFile(claudePath, rr.repo); err != nil && !os.IsNotExist(err) {
			slog.Error("ingest claude config failed", "file", claudePath, "err", err)
			continue
		}
		indexed++
	}

	// Also check ~/.claude/CLAUDE.md and ~/CLAUDE.md.
	if homeDir, err := EffectiveHome(); err == nil {
		for _, extra := range []struct{ path, repo string }{
			{filepath.Join(homeDir, ".claude", "CLAUDE.md"), "global"},
			{filepath.Join(homeDir, "CLAUDE.md"), "home"},
		} {
			if _, err := os.Stat(extra.path); err != nil {
				continue
			}
			onDisk++
			if err := s.ingestClaudeConfigFile(extra.path, extra.repo); err != nil && !os.IsNotExist(err) {
				slog.Error("ingest claude config failed", "file", extra.path, "err", err)
				continue
			}
			indexed++
		}
	}

	s.recordBackfillStatus("claude_configs", indexed, onDisk)
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

// recordBackfillStatus upserts a row into ingest_status.
func (s *Store) recordBackfillStatus(stream string, indexed, onDisk int) {
	_, err := s.writeDB.Exec(`
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

	rows, err := s.readDB.Query(`
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
	s.rootsMu.RLock()
	defer s.rootsMu.RUnlock()
	return s.knownRepoRootsLocked()
}

// knownRepoRootsLocked is the shared implementation. Caller must hold
// s.rootsMu for read or write.
func (s *Store) knownRepoRootsLocked() []repoRoot {
	seen := map[string]bool{}
	var roots []repoRoot

	// 1. Workspace-root discovery via filesystem walk. Workspace roots
	// must be configured explicitly via SetWorkspaceRoots (production
	// does this from ~/.mnemo/config.json with a sensible default);
	// no implicit walk, so tests stay isolated.
	for _, root := range discoverRepos(s.workspaceRoots, s.IsExcluded) {
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
	rows, err := s.readDB.Query("SELECT DISTINCT cwd FROM session_meta WHERE cwd != ''")
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

// ingestClaudeConfigFile ingests a single CLAUDE.md file.
func (s *Store) ingestClaudeConfigFile(path, repo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeDB.Exec("DELETE FROM claude_configs WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	content := string(data)
	now := time.Now().Format(time.RFC3339)

	_, err = s.writeDB.Exec(`
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
	rows, err := s.readDB.Query(q, args...)
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
		if err := s.ingestAuditLogFile(auditPath, repo); err != nil {
			slog.Error("ingest audit log failed", "file", auditPath, "err", err)
			continue
		}
		indexed++
	}
	s.recordBackfillStatus("audit", indexed, onDisk)
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

// ingestAuditLogFile ingests a single audit log file.
func (s *Store) ingestAuditLogFile(path, repo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			_, _ = s.writeDB.Exec("DELETE FROM audit_entries WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	entries := parseAuditLogEntries(string(data))

	// Full replace: delete existing entries for this file, then insert fresh.
	if _, err := s.writeDB.Exec("DELETE FROM audit_entries WHERE file_path = ?", path); err != nil {
		return fmt.Errorf("delete old audit entries: %w", err)
	}

	for _, e := range entries {
		_, err := s.writeDB.Exec(`
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
	rows, err := s.readDB.Query(q, args...)
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
		if _, err := s.writeDB.Exec("DELETE FROM targets WHERE file_path = ?", targetsPath); err != nil {
			slog.Warn("delete targets failed", "path", targetsPath, "err", err)
			continue
		}
		inserted := false
		for _, t := range parsed {
			_, err := s.writeDB.Exec(`
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
	s.recordBackfillStatus("targets", indexed, onDisk)
	slog.Info("ingested targets", "files", indexed, "on_disk", onDisk, "rows", targetCount)
	return nil
}

// ingestTargetFile ingests a single targets.md file (acquires write lock).
func (s *Store) ingestTargetFile(path, repo string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeDB.Exec("DELETE FROM targets WHERE file_path = ?", path)
			return nil
		}
		return err
	}

	parsed := parseTargetsFile(repo, path, data)

	if _, err := s.writeDB.Exec("DELETE FROM targets WHERE file_path = ?", path); err != nil {
		return fmt.Errorf("delete targets: %w", err)
	}
	for _, t := range parsed {
		if _, err := s.writeDB.Exec(`
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
	rows, err := s.readDB.Query(q, args...)
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
			if err2 := s.ingestPlanFile(path, repo, planningDir); err2 != nil {
				slog.Error("ingest plan failed", "file", path, "err", err2)
				return nil
			}
			indexed++
			return nil
		}); err != nil {
			slog.Error("walk planning dir failed", "dir", planningDir, "err", err)
		}
	}
	s.recordBackfillStatus("plans", indexed, onDisk)
	slog.Info("ingested plans", "files", indexed, "on_disk", onDisk, "repos", reposWithPlans)
	return nil
}

// ingestPlanFile ingests a single plan file.
func (s *Store) ingestPlanFile(path, repo, planningDir string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeDB.Exec("DELETE FROM plans WHERE file_path = ?", path)
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
	_, err = s.writeDB.Exec(`
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
	rows, err := s.readDB.Query(q, args...)
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

	rows, err := s.readDB.Query(q, args...)
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

	rows, err := s.readDB.Query(q, args...)
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

// ReworkAttempt is one compaction span that touched a given target,
// indicating a prior work or rework cycle. Returned by ReworkHistory.
type ReworkAttempt struct {
	SessionID   string `json:"session_id"`
	GeneratedAt string `json:"generated_at"`
	Repo        string `json:"repo,omitempty"`
	// Progress is the targets_progressed note for this target, if present.
	Progress string `json:"progress,omitempty"`
	// Summary is the compaction's prose abstract.
	Summary string `json:"summary,omitempty"`
	// OpenThreads lists unresolved items recorded in the span.
	OpenThreads []string `json:"open_threads,omitempty"`
}

// ReworkHistory returns compaction spans that actively worked on targetID,
// ordered most-recent first. A span qualifies when targetID appears in
// targets_active or targets_progressed. Optional repo filter is a substring
// match against session_meta.repo.
func (s *Store) ReworkHistory(targetID string, repo string, limit int) ([]ReworkAttempt, error) {

	if limit <= 0 {
		limit = 20
	}

	// Use a LIKE pre-filter on payload_json for speed (target IDs are
	// short alphanumeric tokens), then verify membership precisely via
	// json_each in a CASE expression to avoid false positives from
	// substring collisions (e.g. "T1" matching "T10").
	like := `%"` + targetID + `"%`
	q := `
		SELECT c.session_id, c.generated_at, COALESCE(sm.repo, ''), c.payload_json, c.summary
		FROM compactions c
		LEFT JOIN session_meta sm ON sm.session_id = c.session_id
		WHERE c.payload_json LIKE ?`
	args := []any{like}
	if repo != "" {
		q += ` AND sm.repo LIKE ?`
		args = append(args, "%"+repo+"%")
	}
	q += ` ORDER BY c.generated_at DESC, c.id DESC LIMIT ?`
	args = append(args, limit*5) // over-fetch to allow post-filter

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("rework history query: %w", err)
	}
	defer rows.Close()

	var out []ReworkAttempt
	for rows.Next() && len(out) < limit {
		var sessionID, generatedAt, repoVal, payloadJSON, summary string
		if err := rows.Scan(&sessionID, &generatedAt, &repoVal, &payloadJSON, &summary); err != nil {
			continue
		}
		// Decode payload to verify precise membership and extract fields.
		var payload struct {
			TargetsActive     []string          `json:"targets_active"`
			Targets           []string          `json:"targets"`
			TargetsProgressed map[string]string `json:"targets_progressed"`
			OpenThreads       []string          `json:"open_threads"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			continue
		}
		active := payload.TargetsActive
		if len(active) == 0 {
			active = payload.Targets
		}
		found := false
		for _, id := range active {
			if id == targetID {
				found = true
				break
			}
		}
		if !found {
			if _, ok := payload.TargetsProgressed[targetID]; ok {
				found = true
			}
		}
		if !found {
			continue
		}
		attempt := ReworkAttempt{
			SessionID:   sessionID,
			GeneratedAt: generatedAt,
			Repo:        repoVal,
			Summary:     summary,
			OpenThreads: payload.OpenThreads,
		}
		if note, ok := payload.TargetsProgressed[targetID]; ok {
			attempt.Progress = note
		}
		out = append(out, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rework history scan: %w", err)
	}
	return out, nil
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
	// 🎯T92 incremental scan: the watcher calls this on every transcript
	// append, so re-scanning the whole session each time is O(all history)
	// per append — a multi-core busy-loop across many active sessions on a
	// large DB. Instead, resume from the per-session watermark and scan
	// only new messages. The watermark is the highest message id already
	// scanned; we start from it (inclusive) so a proposal that sat at the
	// boundary can still pair with a confirmation that just arrived. A
	// missing row (watermark 0) means "never scanned" → full first scan.
	var fromID int64
	_ = db.QueryRow(
		`SELECT scanned_through_id FROM decision_scan_state WHERE session_id = ?`,
		sessionID).Scan(&fromID)

	rows, err := db.Query(`
		SELECT id, role, text, timestamp
		FROM messages
		WHERE session_id = ?
		  AND content_type = 'text'
		  AND is_noise = 0
		  AND id >= ?
		ORDER BY id ASC`, sessionID, fromID)
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
	var maxID int64
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.role, &m.text, &m.timestamp); err != nil {
			continue
		}
		msgs = append(msgs, m)
		if int64(m.id) > maxID {
			maxID = int64(m.id)
		}
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

	// Advance the watermark so the next append rescans only new messages.
	// Record even when no pair was found — that is exactly the case the old
	// code mis-handled (a decision-less session was rescanned forever). We
	// store the last message id seen; the next scan resumes inclusively
	// from it (one-message overlap) to catch a pair spanning the boundary.
	if maxID >= fromID && len(msgs) > 0 {
		db.Exec(`
			INSERT INTO decision_scan_state (session_id, scanned_through_id, scanned_at)
			VALUES (?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				scanned_through_id = excluded.scanned_through_id,
				scanned_at = excluded.scanned_at`,
			sessionID, maxID, time.Now().UTC().Format(time.RFC3339Nano))
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
	// knownRepoRoots acquires rootsMu.RLock to read workspaceRoots.
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
	rows, err := s.readDB.Query(
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

	rows, err := s.readDB.Query(q, args...)
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

// pollCIForRepo fetches and upserts CI runs for a single repo.
//
// 🎯T67: the gh subprocess calls (both `gh run list` and the per-
// failed-run `gh run view --log`) happen before the DB upsert section.
// Previously the function took a write lock before iterating runs
// and called fetchRunLog inside the loop, holding the lock across
// seconds of subprocess + HTTP latency per repo. Now logs are
// fetched first, then a short upsert-only section completes per repo.
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

	// Fetch logs for failed runs BEFORE taking the write lock so the
	// compactor (and other readers) can make progress while we wait on
	// the gh subprocess. Order preserved so the subsequent upsert loop
	// keeps the runs/logSummaries arrays in step.
	logSummaries := make([]string, len(runs))
	for i, run := range runs {
		if run.Conclusion == "failure" {
			logSummaries[i] = s.fetchRunLog(ghPath, repo, run.DatabaseID)
		}
	}

	for i, run := range runs {
		_, err := s.writeDB.Exec(`
			INSERT INTO ci_runs (repo, run_id, workflow, branch, commit_sha, status, conclusion, started_at, completed_at, log_summary, url)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(run_id) DO UPDATE SET
				status = excluded.status,
				conclusion = excluded.conclusion,
				completed_at = excluded.completed_at,
				log_summary = COALESCE(excluded.log_summary, ci_runs.log_summary),
				updated_at = datetime('now')
		`, repo, run.DatabaseID, run.WorkflowName, run.HeadBranch, run.HeadSHA,
			run.Status, run.Conclusion, run.CreatedAt, run.UpdatedAt, logSummaries[i], run.URL)
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
	// source is the producing agent ('claude', 'codex', or 'grok').
	// Empty means 'claude' (the default for ~/.claude/projects transcripts).
	source string
	// parentSessionID + chainMechanism, when set, cause writeParsedFile
	// to record a session_chains edge (Grok forks/subagents, 🎯T111).
	parentSessionID string
	chainMechanism  string
	// model is the primary model stamp (Grok summary / signals); also
	// embedded in entry raw for generated columns.
	model string
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
	// Codex rollout roots (~/.codex/sessions, archived_sessions) are
	// walked alongside the Claude project dirs; the worker routes each
	// file to the right parser by path (🎯T99).
	for _, dir := range append(s.projectDirs(), s.codexDirs()...) {
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
	// Grok session roots (🎯T110): only updates.jsonl — siblings
	// (events/chat_history/rewind_points) must not hit the Claude parser.
	for _, dir := range s.grokDirs() {
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				slog.Warn("grok dir unavailable, skipping", "dir", dir)
			} else {
				slog.Warn("grok dir stat failed, skipping", "dir", dir, "err", err)
			}
			continue
		}
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !isGrokUpdates(path) {
				return nil
			}
			s.mu.Lock()
			offset := s.offsets[path]
			s.mu.Unlock()
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

	ingestStart := time.Now()
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
				parse := parseFile
				switch {
				case isCodexRollout(fe.path):
					parse = parseCodexFile
				case isGrokUpdates(fe.path):
					parse = parseGrokFile
				}
				if pf, err := parse(fe.path, fe.offset); err == nil {
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
	if err := s.runWriter(parsedCh, len(files)); err != nil {
		return err
	}
	slog.Info("ingest duration", "files", len(files), "elapsed", time.Since(ingestStart).Round(time.Millisecond))

	// Detect decision pairs (proposal + confirmation) in sessions that
	// haven't been scanned yet (e.g. sessions ingested before decisions
	// table existed).
	backfillDecisions(s.writeDB)

	// Extract and store images from all ingested entries and messages.
	// Runs synchronously (fast — no API calls). Description generation
	// happens separately in the background worker started by StartImageDescriber.
	backfillImages(s)

	// Git commits and GitHub PRs/issues are no longer backfilled at
	// boot: the "commits" and "github" mirror streams are
	// divergence-driven (🎯T68.5). On a fresh start each repo's cursor
	// is missing → reconciled on the first mirror-reconcile tick,
	// equivalent to the old boot backfill but self-healing afterwards.

	// FTS5 optimize (segment merging) is skipped intentionally.
	// On a fresh 577k-message database it takes 10+ minutes of solid
	// CPU at 100%, blocking all reads. FTS5 works fine with multiple
	// segments — search performance is slightly suboptimal but queries
	// complete in milliseconds regardless. Segments merge naturally as
	// new data trickles in via the watcher.
	return nil
}

const (
	entryInsertSQL = `INSERT OR IGNORE INTO entries
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
// transactions with periodic commits.
func (s *Store) runWriter(parsedCh <-chan parsedFile, totalFiles int) error {
	const commitInterval = 200 * time.Millisecond

	ws, err := newWriterState(s.writeDB)
	if err != nil {
		return err
	}
	defer func() { ws.tx.Rollback() }()
	defer ws.Close()

	lastCommit := time.Now()
	writeStart := time.Now()
	var processed int
	const progressEvery = 100

	commitBatch := func() error {
		ws.Close()
		if err := ws.tx.Commit(); err != nil {
			return err
		}
		ws, err = newWriterState(s.writeDB)
		if err != nil {
			return err
		}
		lastCommit = time.Now()
		return nil
	}

	for pf := range parsedCh {
		processed++
		slog.Info("ingest file",
			"n", processed, "of", totalFiles,
			"session", pf.sessionID,
			"entries", len(pf.entries), "messages", len(pf.messages))
		if totalFiles > 0 && progressEvery > 0 && processed%progressEvery == 0 {
			elapsed := time.Since(writeStart)
			rate := float64(processed) / elapsed.Seconds()
			var eta time.Duration
			if rate > 0 {
				eta = time.Duration(float64(totalFiles-processed)/rate) * time.Second
			}
			slog.Info("ingest progress",
				"processed", processed, "of", totalFiles,
				"rate", fmt.Sprintf("%.1f files/s", rate),
				"eta", eta.Round(time.Second))
		}
		s.writeParsedFile(ws, pf)

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

// writeParsedFile inserts one parsed transcript file (entries, content
// messages, session metadata, and the ingest offset) into the open
// writer transaction. Shared by the parallel batch writer (runWriter)
// and the single-file Codex path (ingestCodexFile), so both sources go
// through identical insert logic. Best-effort: individual insert
// failures are logged, not propagated, matching the original loop.
func (s *Store) writeParsedFile(ws *writerState, pf parsedFile) {
	// Grok session-level usage rows use a stable uuid and must refresh when
	// signals.json grows; delete any prior copy so INSERT OR IGNORE can land.
	if pf.source == "grok" {
		usageUUID := fmt.Sprintf("grok-%s-signals-usage", pf.sessionID)
		_, _ = ws.tx.Exec(`
			DELETE FROM messages WHERE session_id = ? AND text LIKE '[grok signals]%'`,
			pf.sessionID)
		_, _ = ws.tx.Exec(`
			DELETE FROM entries WHERE session_id = ? AND raw->>'$.uuid' = ?`,
			pf.sessionID, usageUUID)
	}

	// Insert all raw entries and build entryIdx→entryID map.
	// INSERT OR IGNORE skips duplicate (session_id, uuid) pairs, so
	// only record the entry ID when a row was actually inserted.
	entryIDs := make(map[int]int64, len(pf.entries))
	for i, e := range pf.entries {
		result, err := ws.entryStmt.Exec(pf.sessionID, pf.project, e.entryType, e.timestamp, string(e.raw))
		if err != nil {
			slog.Warn("entry insert failed", "session", pf.sessionID, "err", err)
			continue
		}
		n, _ := result.RowsAffected()
		if n > 0 {
			if id, err := result.LastInsertId(); err == nil {
				entryIDs[i] = id
			}
		}
	}

	// Insert content block messages linked to their entries.
	// Skip messages whose entry was a duplicate (INSERT OR IGNORE, entryID == 0).
	compactorInternal := 0
	for _, m := range pf.messages {
		entryID := entryIDs[m.entryIdx]
		if entryID == 0 {
			continue // entry was duplicate — skip associated messages
		}
		var toolInput any
		if m.toolInput != nil {
			toolInput = string(m.toolInput)
		}
		ws.msgStmt.Exec(entryID, pf.sessionID, pf.project, m.role, m.text, m.timestamp, m.typ, m.isNoise,
			m.contentType, m.toolName, m.toolUseID, toolInput, m.isError)

		// Detect self-identification nonces.
		if m.contentType == "text" && strings.HasPrefix(m.text, NoncePrefix) {
			ws.tx.Exec("INSERT OR IGNORE INTO session_nonces (nonce, session_id) VALUES (?, ?)",
				strings.TrimSpace(m.text), pf.sessionID)
		}

		// 🎯T72 recursion guard: flag claudia-spawned compaction runs
		// by the marker on their opening prompt.
		if m.contentType == "text" && IsCompactorMarker(m.text) {
			compactorInternal = 1
		}
	}

	// Upsert session metadata.
	if pf.cwd != "" || pf.branch != "" || pf.topic != "" || compactorInternal == 1 || pf.source != "" {
		repo := extractRepo(pf.cwd)
		// Grok may set project to org/repo from git remotes when cwd is opaque.
		if repo == "" && pf.project != "" && pf.project != "subagents" &&
			pf.project != "grok" && strings.Contains(pf.project, "/") {
			repo = pf.project
		}
		workType := classifyWorkType(pf.branch)
		source := pf.source
		if source == "" {
			source = "claude"
		}
		ws.tx.Exec(`INSERT INTO session_meta (session_id, repo, cwd, git_branch, work_type, topic, compactor_internal, source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE session_meta.repo END,
				cwd = CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE session_meta.cwd END,
				git_branch = CASE WHEN excluded.git_branch != '' THEN excluded.git_branch ELSE session_meta.git_branch END,
				work_type = CASE WHEN excluded.work_type != '' THEN excluded.work_type ELSE session_meta.work_type END,
				topic = CASE WHEN excluded.topic != '' AND session_meta.topic = '' THEN excluded.topic ELSE session_meta.topic END,
				compactor_internal = MAX(session_meta.compactor_internal, excluded.compactor_internal),
				source = CASE WHEN excluded.source != '' THEN excluded.source ELSE session_meta.source END`,
			pf.sessionID, repo, pf.cwd, pf.branch, workType, pf.topic, compactorInternal, source)

		// Parent/fork and subagent edges (Grok 🎯T111, Codex 🎯T112).
		if pf.parentSessionID != "" && pf.sessionID != "" {
			mech := pf.chainMechanism
			if mech == "" {
				mech = "parent"
			}
			_, _ = ws.tx.Exec(`
			INSERT OR IGNORE INTO session_chains
				(successor_id, predecessor_id, boundary, gap_ms, confidence, mechanism)
			VALUES (?, ?, 'fork', 0, 'high', ?)`,
				pf.sessionID, pf.parentSessionID, mech)
		}
		for _, m := range pf.messages {
			if m.contentType != "text" || !strings.HasPrefix(m.text, "[grok subagent spawned]") {
				continue
			}
			child := fieldAfter(m.text, "child=")
			if child == "" || pf.sessionID == "" {
				continue
			}
			_, _ = ws.tx.Exec(`
			INSERT OR IGNORE INTO session_chains
				(successor_id, predecessor_id, boundary, gap_ms, confidence, mechanism)
			VALUES (?, ?, 'subagent', 0, 'high', 'grok_subagent')`,
				child, pf.sessionID)
		}
	}

	// Update ingest offset + fingerprint (🎯T68.6 same-size
	// rewrite detection). recordedSize/Mtime is nullable; an
	// os.Stat error just records NULL — detection falls back to
	// offset-vs-size on the next pass.
	recSize, recMtime := statFingerprint(pf.path)
	ws.tx.Exec(`INSERT INTO ingest_state (path, offset, recorded_size, recorded_mtime)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			offset = MAX(ingest_state.offset, excluded.offset),
			recorded_size = CASE
				WHEN excluded.offset >= ingest_state.offset THEN excluded.recorded_size
				ELSE ingest_state.recorded_size
			END,
			recorded_mtime = CASE
				WHEN excluded.offset >= ingest_state.offset THEN excluded.recorded_mtime
				ELSE ingest_state.recorded_mtime
			END`,
		pf.path, pf.newOffset, recSize, recMtime)
	s.mu.Lock()
	if pf.newOffset > s.offsets[pf.path] {
		s.offsets[pf.path] = pf.newOffset
	}
	s.mu.Unlock()
}

// trimLineEnding strips a single trailing "\n" (and a preceding "\r"),
// matching bufio.ScanLines semantics for a line read via
// bufio.Reader.ReadBytes('\n').
func trimLineEnding(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n = len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
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

	reader := bufio.NewReader(f)

	pf := parsedFile{
		path:      path,
		sessionID: sessionID,
		project:   project,
	}

	// handleLine extracts one JSONL line into pf. Its guard clauses skip a
	// line by returning, which — unlike a bare loop `continue` — cannot
	// bypass the read-error / EOF handling in the read loop below.
	handleLine := func(line []byte) {
		var entry jsonlEntry
		if json.Unmarshal(line, &entry) != nil {
			return
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
			return
		}

		blocks := extractBlocks(entry.Message)
		if len(blocks) == 0 {
			return
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

	// bufio.Reader.ReadBytes has no per-line size cap, so oversized lines
	// (e.g. inline base64 images) are ingested rather than silently
	// dropped as they were under bufio.Scanner's token limit (🎯T104). The
	// offset is the running count of bytes consumed — always a true line
	// boundary — so a resume never overshoots unread content; a non-EOF
	// read error aborts without advancing it.
	consumed := offset
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return parsedFile{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		consumed += int64(len(raw))
		if line := trimLineEnding(raw); len(line) > 0 {
			handleLine(line)
		}
		if readErr == io.EOF {
			break
		}
	}

	pf.newOffset = consumed
	return pf, nil
}

// Watch watches for new/modified JSONL files and ingests them in realtime.
// It runs until ctx is cancelled (graceful drain, 🎯T97.1) or the fsnotify
// backend closes its channels. Returning promptly on cancellation is what
// lets Registry.Close finish stopping workers and reach the WAL checkpoint;
// before ctx observance the kqueue/inotify read blocked shutdown until the
// drain deadline forced a hard exit.
func (s *Store) Watch(ctx context.Context) error {
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

	// Watch Codex rollout roots (🎯T99). Date-nested subdirs created
	// later are picked up by the fsnotify Create→watcher.Add path below.
	for _, dir := range s.codexDirs() {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				if wErr := watcher.Add(path); wErr != nil {
					slog.Warn("failed to watch codex directory", "path", path, "err", wErr)
				}
			}
			return nil
		})
	}

	// Watch Grok session roots (🎯T110). New session dirs are picked up
	// by the fsnotify Create→watcher.Add path below.
	for _, dir := range s.grokDirs() {
		filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				if wErr := watcher.Add(path); wErr != nil {
					slog.Warn("failed to watch grok directory", "path", path, "err", wErr)
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
	db := newDebouncerWithConcurrency(300*time.Millisecond, 4)
	// On drain, cancel pending debounced ingests so none races the store's
	// Close/checkpoint (🎯T97.1).
	defer db.stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			name := event.Name
			if strings.HasSuffix(name, ".jsonl") &&
				(event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) {
				db.enqueue(name, func() {
					if isCodexRollout(name) {
						if err := s.ingestCodexFile(name); err != nil {
							slog.Error("ingest codex failed", "file", name, "err", err)
						}
						return
					}
					if isGrokUpdates(name) {
						if err := s.ingestGrokFile(name); err != nil {
							slog.Error("ingest grok failed", "file", name, "err", err)
						}
						return
					}
					// Ignore other Grok sidecars that share the tree
					// (events/chat_history/…) if a parent dir is watched.
					if filepath.Base(name) == "chat_history.jsonl" ||
						filepath.Base(name) == "events.jsonl" ||
						filepath.Base(name) == "rewind_points.jsonl" ||
						filepath.Base(name) == "prompt_history.jsonl" ||
						filepath.Base(name) == "hunk_records.jsonl" ||
						filepath.Base(name) == "feedback.jsonl" {
						return
					}
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

				// TODO.md / todos.md at the repo root or under docs/.
				// Deeper or glob-matched TODO files refresh on the next
				// startup backfill; the common locations are live-watched.
				if isTodoFileName(base) {
					repoRoot := dir
					if filepath.Base(dir) == "docs" {
						repoRoot = filepath.Dir(dir)
					}
					if repo, ok := repoForRoot[repoRoot]; ok {
						db.enqueue(name, func() {
							s.ingestTodoFile(name, repo)
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
	ftsRows, err := s.readDB.Query(`
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

	// Phase 2: enrich hits with message data and apply filters.
	var results []SearchResult
	for _, h := range hits {
		if len(results) >= limit {
			break
		}

		row := s.readDB.QueryRow(`
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
			err := s.readDB.QueryRow("SELECT session_type FROM session_summary WHERE session_id = ?", r.SessionID).Scan(&st)
			if err != nil || st != sessionType {
				continue
			}
		}

		// Apply repo filter.
		if needRepoFilter {
			var count int
			pattern := "%" + repoFilter + "%"
			err := s.readDB.QueryRow("SELECT COUNT(*) FROM session_meta WHERE session_id = ? AND (cwd LIKE ? OR repo LIKE ?)", r.SessionID, pattern, pattern).Scan(&count)
			if err != nil || count == 0 {
				continue
			}
		}

		if len(r.Text) > 500 {
			r.Text = r.Text[:497] + "..."
		}
		results = append(results, r)
	}

	// Phase 2.5 (🎯T72): compaction summaries — the dense, durable layer.
	// They are weighted above raw message hits, and any raw hit a matched
	// compaction covers is suppressed so the summary stands in for it
	// (raw addenda hits, past every cursor, are never covered and flow
	// through unchanged). entry_id_from/entry_id_to bound the covered
	// span in messages.id space.
	type matchedSpan struct{ from, to int }
	coveredBy := map[string][]matchedSpan{}
	var compactionResults []SearchResult
	compRows, compErr := s.readDB.Query(`
		SELECT c.id, c.session_id, c.summary, c.entry_id_from, c.entry_id_to, f.rank
		FROM compactions c
		JOIN compactions_fts f ON f.rowid = c.id
		WHERE compactions_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, ftsQuery, fetchLimit)
	if compErr != nil {
		slog.Warn("compaction FTS query failed", "err", compErr)
	} else {
		for compRows.Next() {
			var id int64
			var sid, summary string
			var from, to int
			var rank float64
			if err := compRows.Scan(&id, &sid, &summary, &from, &to, &rank); err != nil {
				continue
			}
			if needSessionFilter {
				var st string
				if err := s.readDB.QueryRow("SELECT session_type FROM session_summary WHERE session_id = ?", sid).Scan(&st); err != nil || st != sessionType {
					continue
				}
			}
			if needRepoFilter {
				var cnt int
				pattern := "%" + repoFilter + "%"
				if err := s.readDB.QueryRow("SELECT COUNT(*) FROM session_meta WHERE session_id = ? AND (cwd LIKE ? OR repo LIKE ?)", sid, pattern, pattern).Scan(&cnt); err != nil || cnt == 0 {
					continue
				}
			}
			coveredBy[sid] = append(coveredBy[sid], matchedSpan{from: from, to: to})
			if len(summary) > 500 {
				summary = summary[:497] + "..."
			}
			compactionResults = append(compactionResults, SearchResult{
				MessageID: int(id), // compaction id; Role distinguishes it
				SessionID: sid,
				Role:      "compaction",
				Text:      summary,
				Rank:      rank,
			})
			if len(compactionResults) >= limit {
				break
			}
		}
		compRows.Close()
	}
	// Suppress transcript hits covered by a matched compaction span.
	if len(coveredBy) > 0 {
		kept := results[:0]
		for _, r := range results {
			covered := false
			for _, sp := range coveredBy[r.SessionID] {
				if r.MessageID > sp.from && r.MessageID <= sp.to {
					covered = true
					break
				}
			}
			if !covered {
				kept = append(kept, r)
			}
		}
		results = kept
	}

	// Phase 3: vault annotation hits (human content below the generated fence).
	// Vault annotations live in the docs table (kind='vault') and are indexed
	// alongside transcript messages so search surfaces both at once.
	// repoFilter and sessionType are not applied — annotations are cross-repo.
	vaultRows, vaultErr := s.readDB.Query(`
		SELECT d.id, d.file_path, d.content, f.rank
		FROM docs d
		JOIN docs_fts f ON f.rowid = d.id
		WHERE docs_fts MATCH ? AND d.kind = 'vault'
		ORDER BY rank
		LIMIT ?
	`, ftsQuery, limit)
	if vaultErr != nil {
		slog.Warn("vault FTS query failed", "err", vaultErr)
	} else {
		for vaultRows.Next() {
			var docID int64
			var filePath, content string
			var rank float64
			if err := vaultRows.Scan(&docID, &filePath, &content, &rank); err != nil {
				continue
			}
			if len(content) > 500 {
				content = content[:497] + "..."
			}
			results = append(results, SearchResult{
				MessageID: int(-docID), // negative distinguishes from message row IDs
				SessionID: filePath,
				Role:      "vault",
				Text:      content,
				Rank:      rank,
			})
		}
		vaultRows.Close()
	}

	results = mergeBySourcePercentile(results)
	// Compaction summaries rank above transcript/vault hits (🎯T72):
	// prepend them best-rank-first so the dense layer wins, then let the
	// shared limit truncate the long tail.
	if len(compactionResults) > 0 {
		sort.SliceStable(compactionResults, func(i, j int) bool {
			return compactionResults[i].Rank < compactionResults[j].Rank
		})
		results = append(compactionResults, results...)
	}
	if len(results) > limit {
		results = results[:limit]
	}
	if len(results) == 0 {
		return nil, nil
	}

	// Fetch context messages for each transcript hit (skip vault and
	// compaction entries — neither has surrounding message context).
	if contextBefore > 0 || contextAfter > 0 {
		for i := range results {
			r := &results[i]
			if r.Role == "vault" || r.Role == "compaction" {
				continue
			}
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

// mergeBySourcePercentile re-orders a mixed slice of transcript and vault
// search hits so that BM25 ranks are compared on a source-relative scale
// rather than as raw scores.
//
// SQLite FTS5 BM25 scores from messages_fts and docs_fts are NOT directly
// comparable: BM25 calibrates against the corpus (doc-length distribution
// and IDF), so a rank of -3.5 from one corpus and -3.5 from the other were
// computed against different denominators. Sorting by raw rank tends to
// clump one source above the other regardless of true relevance.
//
// Fix: rank each hit by its position within its own source (best=0.0,
// worst=1.0) and sort the merged slice by that percentile. Raw rank is
// the tiebreaker so within-source order is preserved. The "vault" role
// distinguishes vault hits; everything else is treated as a transcript
// hit.
func mergeBySourcePercentile(results []SearchResult) []SearchResult {
	if len(results) <= 1 {
		return results
	}
	percentile := make([]float64, len(results))
	assignPercentiles := func(matches func(SearchResult) bool) {
		idxs := make([]int, 0, len(results))
		for i, r := range results {
			if matches(r) {
				idxs = append(idxs, i)
			}
		}
		sort.SliceStable(idxs, func(a, b int) bool {
			return results[idxs[a]].Rank < results[idxs[b]].Rank
		})
		n := len(idxs)
		for pos, i := range idxs {
			if n <= 1 {
				percentile[i] = 0
				continue
			}
			percentile[i] = float64(pos) / float64(n-1)
		}
	}
	assignPercentiles(func(r SearchResult) bool { return r.Role == "vault" })
	assignPercentiles(func(r SearchResult) bool { return r.Role != "vault" })

	order := make([]int, len(results))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if percentile[order[a]] != percentile[order[b]] {
			return percentile[order[a]] < percentile[order[b]]
		}
		return results[order[a]].Rank < results[order[b]].Rank
	})
	sorted := make([]SearchResult, len(results))
	for i, idx := range order {
		sorted[i] = results[idx]
	}
	return sorted
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

	rows, err := s.readDB.Query(q, sessionID, messageID, count)
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

	rows, err := s.readDB.Query(q, args...)
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

	rows, err := s.readDB.Query(`
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

	// Per-stream backfill status.
	if strRows, strErr := s.readDB.Query(`
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

	// CTE so the subselects below can reference display_repo.
	// SQLite does not propagate outer-SELECT aliases into inline
	// subqueries (unlike PostgreSQL), so a flat SELECT with inline
	// sub-selects against the alias fails with "no such column".
	//
	// Inner sub-selects pick:
	//   - the shortest CLAUDE.md path per repo as the canonical root
	//     (subdirectory CLAUDE.md files are usually scoped notes,
	//     not the project summary). LENGTH() ordering is cheap and
	//     deterministic.
	//   - MAX(commit_date) per repo for the last-commit signal.
	q := `
		WITH base AS (
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
		)
		SELECT
			base.display_repo,
			base.path,
			base.sessions,
			base.last_activity,
			(
				SELECT cc.content
				FROM claude_configs cc
				WHERE cc.repo = base.display_repo
				ORDER BY LENGTH(cc.file_path) ASC
				LIMIT 1
			) AS claude_md_content,
			(
				SELECT MAX(gc.commit_date)
				FROM git_commits gc
				WHERE gc.repo = base.display_repo
			) AS last_commit
		FROM base
		ORDER BY base.last_activity DESC
	`

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []RepoInfo
	for rows.Next() {
		var r RepoInfo
		var claudeMD, lastCommit sql.NullString
		if err := rows.Scan(&r.Repo, &r.Path, &r.Sessions, &r.LastActivity,
			&claudeMD, &lastCommit); err != nil {
			continue
		}
		if claudeMD.Valid {
			r.Summary = extractClaudeMDSummary(claudeMD.String)
		}
		if lastCommit.Valid {
			r.LastCommit = lastCommit.String
			if len(r.LastCommit) > 19 {
				r.LastCommit = r.LastCommit[:19]
			}
		}
		results = append(results, r)
	}

	// Decorate with the latest CLAUDE.md review verdict (🎯T41).
	// Done in a second pass so the SQL above stays simple — the
	// number of repos is small (tens, not millions) and each
	// LatestReview is a single indexed lookup. Failures here are
	// non-fatal: a repo without a review just gets empty fields.
	for i := range results {
		rev, err := s.LatestReview(results[i].Repo)
		if err != nil || rev == nil {
			continue
		}
		results[i].SummaryVerdict = rev.Verdict
		results[i].SummaryReviewedAt = rev.ReviewedAt
	}
	return results, nil
}

// extractClaudeMDSummary pulls a one-line summary out of CLAUDE.md
// content: skip leading blank lines and top-level headings, take the
// first non-blank line of body prose, then the first sentence of that
// line. Capped at 120 chars so the at-a-glance view stays scannable.
//
// This is best-effort prose extraction, not Markdown parsing — bullet
// lists and code fences are rare as the FIRST body content of a
// CLAUDE.md, and falling through to "use the first non-empty line"
// gives a useful answer even on edge cases.
func extractClaudeMDSummary(content string) string {
	const maxLen = 120
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// First sentence: split on ". " (period + space) which
		// catches normal English; fall through to the whole line if
		// no sentence boundary is found.
		if idx := strings.Index(line, ". "); idx > 0 && idx < maxLen {
			line = line[:idx+1]
		}
		if len(line) > maxLen {
			line = line[:maxLen-1] + "…"
		}
		return line
	}
	return ""
}

// RecentActivity returns per-repo summaries of session activity within the
// given recency window. Only interactive sessions are included.
func (s *Store) RecentActivity(days int, repoFilter string) ([]RecentActivityInfo, error) {

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

	rows, err := s.readDB.Query(q, args...)
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
// Use UsageParams to specify the window, filters, and grouping.
//
// groupBy supports: "day" (default), "model", "repo", "session", "block".
//
// The 5-hour billing block algorithm (group_by="block") mirrors ccusage:
//   - Sort assistant messages by timestamp.
//   - Floor the first message's timestamp to the UTC hour → block start.
//   - Messages within 5 hours of the block start fall in the same block.
//   - When the gap from the previous message exceeds 5 hours, or the total
//     span from the block start exceeds 5 hours, close the current block and
//     open a new one (floored to the UTC hour of the new message).
//
// This produces billing-aligned blocks matching what /cost and ccusage report.
func (s *Store) Usage(p UsageParams) (*UsageResult, error) {

	groupBy := p.GroupBy
	if groupBy == "" {
		groupBy = "day"
	}

	// Resolve time window.
	var sinceExpr, untilExpr string
	result := &UsageResult{}
	if p.Since != "" || p.Until != "" {
		// Explicit since/until window overrides days.
		if p.Since != "" {
			sinceExpr = p.Since
			result.Since = p.Since
		} else {
			sinceExpr = "1970-01-01T00:00:00Z"
		}
		if p.Until != "" {
			untilExpr = p.Until
			result.Until = p.Until
		} else {
			untilExpr = time.Now().UTC().Format(time.RFC3339)
		}
	} else {
		days := p.Days
		if days <= 0 {
			days = 30
		}
		result.Days = days
		sinceExpr = time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
		untilExpr = time.Now().UTC().Format(time.RFC3339)
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
	case "session":
		groupExpr = "e.session_id"
		periodExpr = "e.session_id"
	case "block":
		// Block grouping is done in Go after fetching per-message rows.
		groupExpr = "" // handled below
		periodExpr = ""
	default: // "day"
		groupExpr = "date(e.timestamp)"
		periodExpr = "date(e.timestamp)"
	}

	where := []string{
		"e.type = 'assistant'",
		"e.timestamp >= ?",
		"e.timestamp <= ?",
	}
	args := []any{sinceExpr, untilExpr}

	if p.RepoFilter != "" {
		where = append(where, "(sm.repo LIKE ? OR sm.cwd LIKE ?)")
		pattern := "%" + p.RepoFilter + "%"
		args = append(args, pattern, pattern)
	}
	if p.Model != "" {
		where = append(where, "e.model LIKE ?")
		args = append(args, p.Model+"%")
	}

	needJoin := p.RepoFilter != "" || groupBy == "repo"
	joinClause := ""
	if needJoin {
		joinClause = "LEFT JOIN session_meta sm ON sm.session_id = e.session_id"
	}

	// For block grouping, fetch per-message rows and group in Go.
	if groupBy == "block" {
		return s.usageByBlock(result, where, args, joinClause, p.RepoFilter, p.Model)
	}

	// Always group by model too, so cost estimation is accurate.
	// Re-aggregate in Go when the requested groupBy isn't "model".
	//
	// For "day" grouping, LEFT JOIN reconciled_costs so we can surface
	// authoritative Anthropic Admin API costs alongside estimated costs
	// in a single round-trip (avoids a second concurrent DB connection
	// under the single-connection pool constraint).
	//
	// MAX(e.timestamp) is included to derive the freshness field without
	// a second query.
	reconcJoin := ""
	reconcCostCol := "NULL"
	if groupBy == "day" {
		reconcJoin = "LEFT JOIN reconciled_costs rc ON rc.date = date(e.timestamp)"
		reconcCostCol = "MIN(rc.cost_usd)" // MIN collapses the per-model GROUP
	}

	q := fmt.Sprintf(`
		SELECT
			%s AS period,
			COALESCE(e.model, '') AS model,
			COALESCE(SUM(e.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(e.output_tokens), 0) AS output_tokens,
			COALESCE(SUM(e.cache_read_tokens), 0) AS cache_read_tokens,
			COALESCE(SUM(e.cache_creation_tokens), 0) AS cache_creation_tokens,
			COUNT(*) AS messages,
			%s AS reconciled_cost_usd,
			MAX(e.timestamp) AS max_ts
		FROM entries e
		%s
		%s
		WHERE %s
		GROUP BY %s, e.model
		ORDER BY period DESC
	`, periodExpr, reconcCostCol, joinClause, reconcJoin, strings.Join(where, " AND "), groupExpr)

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}

	// Accumulate per-(period, model) rows, computing accurate costs.
	// NOTE: rows.Close() is called explicitly after the loop so the DB
	// connection is released before the activeHours follow-on query.
	// With db.SetMaxOpenConns(1), holding rows open while issuing a
	// second Query() blocks indefinitely.
	merged := map[string]*UsageRow{} // period → aggregated row
	var order []string

	// Track per-period estimated/reconciled cost presence for "mixed" detection.
	// mergedEstimated[period] = true if any row in that period used estimated cost.
	// mergedReconciled[period] = true if any row used reconciled cost.
	mergedEstimated := map[string]bool{}
	mergedReconciled := map[string]bool{}

	totalEstimated := false
	totalReconciled := false

	var maxTS string // track overall freshness
	for rows.Next() {
		var period, rowModel string
		var input, output, cacheRead, cacheCreate int64
		var msgs int
		var reconciledCost sql.NullFloat64
		var rowMaxTS string
		if err := rows.Scan(&period, &rowModel, &input, &output,
			&cacheRead, &cacheCreate, &msgs, &reconciledCost, &rowMaxTS); err != nil {
			continue
		}
		if rowMaxTS > maxTS {
			maxTS = rowMaxTS
		}
		estimatedCost := estimateCost(rowModel, input, output, cacheRead, cacheCreate)

		// Use the reconciled cost (from Anthropic Admin API) when available;
		// fall back to the locally-computed estimate otherwise.
		var rowCost float64
		rowReconciled := false
		if reconciledCost.Valid {
			rowCost = reconciledCost.Float64
			rowReconciled = true
		} else {
			rowCost = estimatedCost
		}

		if groupBy == "model" {
			// Each model is its own row — no merging needed.
			src := "estimated"
			if rowReconciled {
				src = "reconciled"
			}
			result.Rows = append(result.Rows, UsageRow{
				Period: period, Model: period,
				InputTokens: input, OutputTokens: output,
				CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreate,
				Messages: msgs, CostUSD: rowCost, Source: src,
			})
		} else {
			r, ok := merged[period]
			if !ok {
				r = &UsageRow{Period: period}
				if groupBy == "repo" {
					r.Repo = period
				}
				if groupBy == "session" {
					r.SessionID = period
				}
				merged[period] = r
				order = append(order, period)
			}
			r.InputTokens += input
			r.OutputTokens += output
			r.CacheReadTokens += cacheRead
			r.CacheCreationTokens += cacheCreate
			r.Messages += msgs
			if rowReconciled {
				// For day grouping, the reconciled cost covers all models in
				// that day; only add it once (first model hit sets the cost,
				// subsequent models in same day add 0 to avoid double-counting).
				if !mergedReconciled[period] {
					r.CostUSD += rowCost
					mergedReconciled[period] = true
				}
			} else {
				r.CostUSD += rowCost
				mergedEstimated[period] = true
			}
		}

		if rowReconciled {
			totalReconciled = true
		} else {
			totalEstimated = true
		}

		result.Total.InputTokens += input
		result.Total.OutputTokens += output
		result.Total.CacheReadTokens += cacheRead
		result.Total.CacheCreationTokens += cacheCreate
		result.Total.Messages += msgs
		result.Total.CostUSD += rowCost
	}

	// Release the DB connection before issuing any follow-on queries.
	rows.Close()

	if groupBy != "model" {
		for _, k := range order {
			r := merged[k]
			// Set source per-row.
			if mergedReconciled[k] && mergedEstimated[k] {
				r.Source = "mixed"
			} else if mergedReconciled[k] {
				r.Source = "reconciled"
			} else {
				r.Source = "estimated"
			}
			result.Rows = append(result.Rows, *r)
		}
	}
	result.Total.Period = "total"
	if totalReconciled && totalEstimated {
		result.Total.Source = "mixed"
	} else if totalReconciled {
		result.Total.Source = "reconciled"
	} else {
		result.Total.Source = "estimated"
	}

	// Freshness: timestamp of most-recently ingested assistant message,
	// collected from MAX(e.timestamp) during the main iteration.
	if maxTS != "" {
		result.Freshness = maxTS
	}

	// Compute hourly rate from the actual time span of assistant messages.
	var activeHoursErr error
	var activeHoursVal float64
	if result.Days > 0 {
		activeHoursVal, activeHoursErr = s.activeHours(result.Days, p.RepoFilter, p.Model)
	} else {
		activeHoursVal, activeHoursErr = s.activeHoursRange(sinceExpr, untilExpr, p.RepoFilter, p.Model)
	}
	if activeHoursErr == nil && activeHoursVal > 0 {
		result.HourlyRate = &HourlyRate{
			ActiveHours:     activeHoursVal,
			InputPerHour:    float64(result.Total.InputTokens) / activeHoursVal,
			OutputPerHour:   float64(result.Total.OutputTokens) / activeHoursVal,
			CostPerHour:     result.Total.CostUSD / activeHoursVal,
			MessagesPerHour: float64(result.Total.Messages) / activeHoursVal,
		}
	}

	return result, nil
}

// usageByBlock groups assistant messages into 5-hour billing blocks.
// The block boundary algorithm mirrors ccusage/_session-blocks.ts:
//   - Floor the first message timestamp to the UTC hour → blockStart.
//   - Messages whose timestamp is within 5 hours of blockStart, AND whose
//     gap from the previous message is ≤ 5 hours, belong to the same block.
//   - Otherwise, close the block and open a new one.
func (s *Store) usageByBlock(
	result *UsageResult,
	where []string, args []any,
	joinClause, repoFilter, model string,
) (*UsageResult, error) {
	// Fetch per-message rows ordered by timestamp.
	q := fmt.Sprintf(`
		SELECT
			e.timestamp,
			COALESCE(e.model, '') AS model,
			COALESCE(e.input_tokens, 0) AS input_tokens,
			COALESCE(e.output_tokens, 0) AS output_tokens,
			COALESCE(e.cache_read_tokens, 0) AS cache_read_tokens,
			COALESCE(e.cache_creation_tokens, 0) AS cache_creation_tokens
		FROM entries e
		%s
		WHERE %s
		ORDER BY e.timestamp ASC
	`, joinClause, strings.Join(where, " AND "))

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type msgRow struct {
		ts          time.Time
		model       string
		input       int64
		output      int64
		cacheRead   int64
		cacheCreate int64
	}

	var msgs []msgRow
	var maxTS time.Time
	for rows.Next() {
		var tsStr, mdl string
		var inp, out, cr, cc int64
		if err := rows.Scan(&tsStr, &mdl, &inp, &out, &cr, &cc); err != nil {
			continue
		}
		ts, err := parseTimestamp(tsStr)
		if err != nil {
			continue
		}
		if ts.After(maxTS) {
			maxTS = ts
		}
		msgs = append(msgs, msgRow{ts, mdl, inp, out, cr, cc})
	}

	const blockDur = 5 * time.Hour

	type blockState struct {
		start       time.Time
		lastMsg     time.Time
		cost        float64
		input       int64
		output      int64
		cacheRead   int64
		cacheCreate int64
		messages    int
	}

	var blocks []blockState
	var cur *blockState

	for _, m := range msgs {
		cost := estimateCost(m.model, m.input, m.output, m.cacheRead, m.cacheCreate)

		if cur == nil {
			// Floor to UTC hour.
			start := m.ts.UTC().Truncate(time.Hour)
			cur = &blockState{start: start, lastMsg: m.ts}
		} else {
			sinceStart := m.ts.Sub(cur.start)
			sinceLastMsg := m.ts.Sub(cur.lastMsg)
			if sinceStart > blockDur || sinceLastMsg > blockDur {
				// Close current block, start new one.
				blocks = append(blocks, *cur)
				start := m.ts.UTC().Truncate(time.Hour)
				cur = &blockState{start: start, lastMsg: m.ts}
			} else {
				cur.lastMsg = m.ts
			}
		}
		cur.cost += cost
		cur.input += m.input
		cur.output += m.output
		cur.cacheRead += m.cacheRead
		cur.cacheCreate += m.cacheCreate
		cur.messages++
		result.Total.InputTokens += m.input
		result.Total.OutputTokens += m.output
		result.Total.CacheReadTokens += m.cacheRead
		result.Total.CacheCreationTokens += m.cacheCreate
		result.Total.Messages++
		result.Total.CostUSD += cost
	}
	if cur != nil {
		blocks = append(blocks, *cur)
	}

	// Emit rows newest-first.
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		endTime := b.start.Add(blockDur)
		period := fmt.Sprintf("%s/%s", b.start.UTC().Format(time.RFC3339), endTime.UTC().Format(time.RFC3339))
		result.Rows = append(result.Rows, UsageRow{
			Period:              period,
			InputTokens:         b.input,
			OutputTokens:        b.output,
			CacheReadTokens:     b.cacheRead,
			CacheCreationTokens: b.cacheCreate,
			Messages:            b.messages,
			CostUSD:             b.cost,
			Source:              "estimated",
		})
	}
	result.Total.Period = "total"
	result.Total.Source = "estimated"
	if !maxTS.IsZero() {
		result.Freshness = maxTS.UTC().Format(time.RFC3339Nano)
	}

	return result, nil
}

// parseTimestamp parses a SQLite timestamp string in RFC3339 or datetime format.
func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp: %s", s)
}

// UpsertReconciledCost stores or updates an authoritative cost figure from the
// Anthropic Admin API for a given UTC calendar date.
func (s *Store) UpsertReconciledCost(date string, costUSD float64) error {
	_, err := s.writeDB.Exec(`
		INSERT INTO reconciled_costs(date, cost_usd, fetched_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(date) DO UPDATE SET cost_usd=excluded.cost_usd, fetched_at=excluded.fetched_at
	`, date, costUSD)
	return err
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

	rows, err := s.readDB.Query(q, args...)
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

// activeHoursRange is like activeHours but uses explicit since/until bounds.
func (s *Store) activeHoursRange(since, until, repoFilter, model string) (float64, error) {
	where := []string{
		"e.type = 'assistant'",
		"e.timestamp >= ?",
		"e.timestamp <= ?",
	}
	args := []any{since, until}

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

	rows, err := s.readDB.Query(q, args...)
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
		t, err := parseTimestamp(ts)
		if err != nil {
			continue
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

	rows, err := s.readDB.Query(topQuery, topArgs...)
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

	bashRows, err := s.readDB.Query(bashQuery, bashArgs...)
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
		rows, err := s.readDB.Query(q, args...)
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
		rows, err := s.readDB.Query(q, args...)
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
		rows, err := s.readDB.Query(q, args...)
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
		rows, err := s.readDB.Query(q, args...)
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

	// Fetch the repo → sessions → excerpts hierarchy in a single sqldeep
	// nested-projection query (🎯T94). The previous version ran 1 + R + R*S
	// queries (repos, then sessions per repo, then excerpts per session)
	// and stitched the tree in Go; this collapses them into one round-trip
	// whose result is the tree as JSON, one repo object per row.
	//
	// Mechanics worth knowing before editing this query:
	//   - Named params (:window etc.) are reused across nesting levels and
	//     are immune to sqldeep's clause reordering; positional `?` is not
	//     (sqldeep v0.23.0). repoFilter is optional via the :repoPat=''
	//     sentinel, so one query serves filtered and unfiltered calls.
	//   - A nested SELECT with a *direct* LIMIT transpiles to a singular
	//     object, not an array. So each level's LIMIT (maxSessions, and
	//     newest-maxExcerpts) is pushed into a wrapped subquery; the outer
	//     nested SELECT has no LIMIT and pluralises via json_group_array.
	//     The excerpts subquery takes id DESC LIMIT N to pick the newest N.
	//   - Array element order is NOT reliably controllable from SQL here:
	//     json_group_array follows the inner subquery's order, and an outer
	//     ORDER BY on the aggregate query is ignored. So the final ordering
	//     (sessions by recency desc, excerpts by id asc) and the byte-based
	//     truncation are done in the Go pass below. Excerpt truncation is
	//     byte-based + sets Truncated; SQLite substr is char-based and would
	//     diverge on multibyte text.
	const statusQuery = `
FROM (
  SELECT
    CASE WHEN sm.repo != '' THEN sm.repo ELSE sm.cwd END AS display_repo,
    MAX(sm.cwd) AS path,
    MAX(ss.last_msg) AS last_activity
  FROM session_summary ss
  JOIN session_meta sm ON sm.session_id = ss.session_id
  WHERE ss.session_type = 'interactive'
    AND ss.last_msg >= datetime('now', :window)
    AND (:repoPat = '' OR sm.repo LIKE :repoPat OR sm.cwd LIKE :repoPat)
  GROUP BY display_repo
  HAVING display_repo != ''
  ORDER BY last_activity DESC
) r
SELECT {
  repo: r.display_repo,
  path: r.path,
  last_activity: r.last_activity,
  sessions: SELECT {
    session_id: sess.session_id,
    last_msg: sess.last_msg,
    messages: sess.substantive_msgs,
    work_type: sess.work_type,
    topic: sess.topic,
    excerpts: SELECT { id: ex.id, role: ex.role, text: ex.text, timestamp: ex.timestamp }
      FROM (
        SELECT id, role, text, timestamp FROM messages
        WHERE session_id = sess.session_id AND is_noise = 0 AND role IN ('user', 'assistant')
        ORDER BY id DESC LIMIT :maxExcerpts
      ) ex
  } FROM (
    SELECT ss.session_id AS session_id, ss.last_msg AS last_msg,
           ss.substantive_msgs AS substantive_msgs,
           COALESCE(sm.work_type, '') AS work_type, COALESCE(sm.topic, '') AS topic
    FROM session_summary ss
    JOIN session_meta sm ON sm.session_id = ss.session_id
    WHERE ss.session_type = 'interactive'
      AND ss.last_msg >= datetime('now', :window)
      AND (sm.repo = r.display_repo OR sm.cwd = r.path)
    ORDER BY ss.last_msg DESC LIMIT :maxSessions
  ) sess
}
`
	transpiled, err := sqldeep.Transpile(statusQuery)
	if err != nil {
		return nil, fmt.Errorf("status: transpile: %w", err)
	}

	repoPat := ""
	if repoFilter != "" {
		repoPat = "%" + repoFilter + "%"
	}

	repoRows, err := s.readDB.Query(transpiled,
		sql.Named("window", fmt.Sprintf("-%d days", days)),
		sql.Named("repoPat", repoPat),
		sql.Named("maxSessions", maxSessions),
		sql.Named("maxExcerpts", maxExcerpts),
	)
	if err != nil {
		return nil, fmt.Errorf("status: query: %w", err)
	}
	defer repoRows.Close()

	var repos []RepoStatus
	for repoRows.Next() {
		var blob []byte
		if err := repoRows.Scan(&blob); err != nil {
			continue
		}
		var r RepoStatus
		if err := json.Unmarshal(blob, &r); err != nil {
			continue
		}
		// Order the JSON arrays (json_group_array order is not reliable) and
		// truncate excerpts — all deterministic, on at most maxSessions ×
		// maxExcerpts rows. Sessions: most-recent first. Excerpts: oldest
		// first among the newest-N. Truncation is byte-based + sets the flag,
		// matching the pre-T94 behaviour (SQLite substr is char-based).
		sort.SliceStable(r.Sessions, func(a, b int) bool {
			return r.Sessions[a].LastMsg > r.Sessions[b].LastMsg
		})
		for si := range r.Sessions {
			exs := r.Sessions[si].Excerpts
			sort.SliceStable(exs, func(a, b int) bool { return exs[a].ID < exs[b].ID })
			for ei := range exs {
				if len(exs[ei].Text) > truncateLen {
					exs[ei].Text = exs[ei].Text[:truncateLen] + "..."
					exs[ei].Truncated = true
				}
			}
		}
		repos = append(repos, r)
	}
	if err := repoRows.Err(); err != nil {
		return nil, fmt.Errorf("status: scan: %w", err)
	}

	// Per-stream backfill status.
	var streams []BackfillStatus
	if strRows, strErr := s.readDB.Query(`
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

	// 🎯T75: attach the transcript-ingest freshness/lag diagnostics so a
	// stale or behind index is visible from mnemo_status itself.
	diag := s.IngestDiagnostics(repoFilter)

	return &StatusResult{Days: days, Diagnostics: diag, Repos: repos, Streams: streams}, nil
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

	rows, err := s.readDB.Query(q, args...)
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

// ReadSessionAfter returns substantive (non-noise) messages for a
// session whose messages.id is strictly greater than afterID, ordered
// ascending and capped at limit (default 500). It is the cursor-based
// read the compactor uses to advance through a session window by
// window (🎯T68.2): unlike ReadSession's positional offset, passing
// the previous compaction's to-cursor as afterID yields the *next*
// span, so a session longer than one window fully converges across
// successive ticks instead of stalling on its first 500 messages.
//
// afterID is a messages.id (the value stored in compactions.entry_id_to
// — see the Compaction type for the key-space note). is_noise rows are
// excluded in SQL so the result matches the owed-predicate in
// SelectCompactionCandidates exactly: owed ⟺ this returns ≥1 row.
func (s *Store) ReadSessionAfter(sessionID string, afterID int64, limit int) ([]SessionMessage, error) {

	if limit <= 0 {
		limit = 500
	}

	resolvedID, err := s.resolveSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := s.readDB.Query(`
		SELECT id, role, text, timestamp, is_noise FROM messages
		WHERE session_id = ? AND id > ? AND is_noise = 0
		ORDER BY id ASC
		LIMIT ?`, resolvedID, afterID, limit)
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
	return results, rows.Err()
}

// resolveSessionID resolves a full or prefix session ID to an exact session ID.
func (s *Store) resolveSessionID(id string) (string, error) {
	// Try exact match first (session_summary has one row per session).
	var exists int
	err := s.readDB.QueryRow("SELECT 1 FROM session_summary WHERE session_id = ?", id).Scan(&exists)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	// Try prefix match.
	rows, err := s.readDB.Query("SELECT session_id FROM session_summary WHERE session_id LIKE ? LIMIT 2", id+"%")
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

	var sessionID string
	err := s.readDB.QueryRow(
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
	home, _ := EffectiveHome()
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
	var cwd string
	s.readDB.QueryRow("SELECT cwd FROM session_meta WHERE session_id = ? LIMIT 1", sessionID).Scan(&cwd)
	return cwd
}

// runLsof is platform-specific: see store_unix.go / store_windows.go. It runs
// whatever OS-native command discovers the Claude process's open JSONL files
// and returns raw bytes for parseLsofOutput to consume.

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

// Query runs a read-only SQL query and returns rows as maps. Input is
// unconditionally passed through sqldeep.Transpile — sqldeep is a strict
// superset of SQL, so plain SELECT/WITH round-trips unchanged. Read-only
// enforcement is delegated to SQLite via PRAGMA query_only on the
// dedicated connection used for the query; write statements are rejected
// by SQLite itself, not by a string-prefix sniff in Go.
func (s *Store) Query(query string, args ...any) ([]map[string]any, error) {
	execSQL, err := sqldeep.Transpile(strings.TrimSpace(query))
	if err != nil {
		return nil, fmt.Errorf("sqldeep transpile: %w", err)
	}

	ctx := context.Background()
	conn, err := s.readDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// query_only=1 is the write boundary; the read pool's authorizer
	// (rodriver.go) both prevents this being turned back off and denies
	// ATTACH, so there is deliberately no reset to query_only=0 on
	// release — every read-pool connection stays read-only for its life.
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		return nil, fmt.Errorf("set query_only: %w", err)
	}

	rows, err := conn.QueryContext(ctx, execSQL, args...)
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
	// SQLITE_READONLY from PRAGMA query_only fires at sqlite3_step time,
	// which surfaces as rows.Err() rather than the QueryContext result —
	// so write statements (DELETE/DROP/INSERT/UPDATE) reach this check
	// without an earlier prepare-time error.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// healCompactionsFTS rebuilds the compactions_fts external-content
// index when it has drifted from the compactions table — the one-time
// case being existing compaction rows that predate the FTS table
// (🎯T72). The compactions_ai trigger keeps the two in lockstep for
// every subsequent insert, so the row counts match in steady state and
// this returns after two cheap COUNT(*) reads without touching the
// index. If applySchema was rejected (older binary vs newer DB) the
// FTS table is absent and the count query errors — we return silently.
func healCompactionsFTS(db *sql.DB) {
	var nComp, nFTS int
	if err := db.QueryRow("SELECT COUNT(*) FROM compactions").Scan(&nComp); err != nil {
		return
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM compactions_fts").Scan(&nFTS); err != nil {
		return
	}
	if nComp == nFTS {
		return
	}
	if _, err := db.Exec("INSERT INTO compactions_fts(compactions_fts) VALUES('rebuild')"); err != nil {
		slog.Warn("compactions_fts rebuild failed", "err", err)
		return
	}
	slog.Info("compactions_fts backfilled", "compactions", nComp)
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
// Windows paths (e.g. C:\Users\...\work\github.com\org\repo) are normalised
// to forward slashes before matching so the same regex works on every
// platform.
func extractRepo(cwd string) string {
	m := repoPattern.FindStringSubmatch(filepath.ToSlash(cwd))
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

// Ingest commit cadence for the realtime watcher path (ingestFile).
// Vars, not consts, so a test can force a mid-stream commit.
var (
	ingestCommitInterval    = 200 * time.Millisecond
	ingestLineCheckInterval = 50
)

// testMidStreamCommitOffset, when non-nil, is invoked with the offset
// persisted at each mid-stream commit — a test seam for asserting that
// offset lands on a true line boundary (🎯T107).
var testMidStreamCommitOffset func(int64)

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

	reader := bufio.NewReader(f)

	count := 0
	var metaCwd, metaBranch, metaTopic string
	metaCompactorInternal := 0 // 🎯T72: set when a compactor-run marker is seen

	ws, err := newWriterState(s.writeDB)
	if err != nil {
		return err
	}
	defer func() { ws.tx.Rollback() }()
	defer ws.Close()

	// handleLine writes one JSONL line into the current transaction. Its
	// guard clauses skip a line by returning rather than a loop `continue`,
	// so the periodic-commit and EOF handling in the read loop always run.
	// It closes over ws, which is reassigned after each mid-stream commit.
	handleLine := func(line []byte) {
		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return
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
		// INSERT OR IGNORE skips duplicate (session_id, uuid) pairs, so
		// only record the entry ID when a row was actually inserted.
		var entryID int64
		result, entryErr := ws.entryStmt.Exec(sessionID, project, entry.Type, ts, string(line))
		if entryErr == nil {
			if n, _ := result.RowsAffected(); n > 0 {
				entryID, _ = result.LastInsertId()
			}
		}

		// Only extract content blocks for user/assistant messages.
		// Skip if the entry was a duplicate (INSERT OR IGNORE, entryID == 0).
		if entry.Type != "user" && entry.Type != "assistant" || entryID == 0 {
			return
		}

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

			// 🎯T72 recursion guard: flag claudia-spawned compaction
			// runs by the marker on their opening prompt.
			if b.ContentType == "text" && IsCompactorMarker(b.Text) {
				metaCompactorInternal = 1
			}
		}
	}

	lastCommit := time.Now()
	linesSinceLockCheck := 0

	// bufio.Reader.ReadBytes has no per-line size cap, so oversized lines
	// are ingested rather than silently dropped (🎯T104). The committed
	// offset is the running count of bytes consumed — a true line boundary
	// — not f.Seek(0,1), which reads ahead of the last processed line and
	// at a mid-stream commit would persist an offset past not-yet-written
	// content, so a crash would skip it (🎯T107). A non-EOF read error
	// aborts without advancing the offset.
	consumed := offset
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		consumed += int64(len(raw))
		if line := trimLineEnding(raw); len(line) > 0 {
			handleLine(line)
		}

		// Periodically commit to avoid long-running transactions.
		linesSinceLockCheck++
		if linesSinceLockCheck >= ingestLineCheckInterval {
			linesSinceLockCheck = 0
			if time.Since(lastCommit) >= ingestCommitInterval {
				// Commit current transaction with offset update.
				recSize, recMtime := statFingerprint(path)
				ws.tx.Exec(`INSERT OR REPLACE INTO ingest_state (path, offset, recorded_size, recorded_mtime)
					VALUES (?, ?, ?, ?)`, path, consumed, recSize, recMtime)

				ws.Close()
				if err := ws.tx.Commit(); err != nil {
					return err
				}

				s.mu.Lock()
				s.offsets[path] = consumed
				s.mu.Unlock()

				if testMidStreamCommitOffset != nil {
					testMidStreamCommitOffset(consumed)
				}

				// Start a new transaction.
				ws, err = newWriterState(s.writeDB)
				if err != nil {
					return err
				}
				lastCommit = time.Now()
			}
		}

		if readErr == io.EOF {
			break
		}
	}

	// Upsert session metadata.
	if metaCwd != "" || metaBranch != "" || metaTopic != "" || metaCompactorInternal == 1 {
		repo := extractRepo(metaCwd)
		workType := classifyWorkType(metaBranch)
		ws.tx.Exec(`INSERT INTO session_meta (session_id, repo, cwd, git_branch, work_type, topic, compactor_internal)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				repo = CASE WHEN excluded.repo != '' THEN excluded.repo ELSE session_meta.repo END,
				cwd = CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE session_meta.cwd END,
				git_branch = CASE WHEN excluded.git_branch != '' THEN excluded.git_branch ELSE session_meta.git_branch END,
				work_type = CASE WHEN excluded.work_type != '' THEN excluded.work_type ELSE session_meta.work_type END,
				topic = CASE WHEN excluded.topic != '' AND session_meta.topic = '' THEN excluded.topic ELSE session_meta.topic END,
				compactor_internal = MAX(session_meta.compactor_internal, excluded.compactor_internal)`,
			sessionID, repo, metaCwd, metaBranch, workType, metaTopic, metaCompactorInternal)
	}

	recSize, recMtime := statFingerprint(path)
	ws.tx.Exec(`INSERT OR REPLACE INTO ingest_state (path, offset, recorded_size, recorded_mtime)
		VALUES (?, ?, ?, ?)`, path, consumed, recSize, recMtime)

	ws.Close()
	if err := ws.tx.Commit(); err != nil {
		return err
	}

	s.mu.Lock()
	s.offsets[path] = consumed
	s.mu.Unlock()

	if count > 0 {
		slog.Debug("ingested", "file", filepath.Base(path), "messages", count)
		s.NoteActivity()
	}

	// Detect decision pairs (proposal + confirmation) in this session.
	repo := extractRepo(metaCwd)
	detectDecisions(s.writeDB, sessionID, repo)

	// Extract and store any images from newly ingested entries.
	// Uses a targeted query so only new entries need scanning.
	go func() {
		rows, err := s.readDB.Query(`
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

// Predecessor returns the predecessor session ID for the given session,
// or "" if none exists.
func (s *Store) Predecessor(sessionID string) (string, error) {
	var predID string
	err := s.readDB.QueryRow(
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
	var succID string
	err := s.readDB.QueryRow(
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
	return s.chain(sessionID)
}

func (s *Store) chain(sessionID string) ([]ChainLink, error) {
	// Walk backwards to find the head (oldest) of the chain.
	head := sessionID
	visited := map[string]bool{head: true}
	for {
		var pred string
		err := s.readDB.QueryRow(
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
		err = s.readDB.QueryRow(
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

// runPsEnv and runPsMetrics are platform-gated seams — see store_unix.go /
// store_windows.go. Tests replace them with functions that return synthetic
// output. On Windows they return empty by default (live-session discovery and
// per-PID metrics degrade gracefully without a lsof/ps equivalent wired in).

// psRow is the per-PID metrics captured from `ps -o pid=,rss=,%cpu=,time=`.
type psRow struct {
	rss     int64
	cpuPct  float64
	cpuTime string
}

// parsePsMetricsOutput parses `ps -o pid=,rss=,%cpu=,time= -p <pids>` output
// into a map of PID → psRow. Malformed or short lines are silently skipped so
// the caller can degrade gracefully when ps fails or returns nothing.
func parsePsMetricsOutput(data []byte) map[int]psRow {
	result := make(map[int]psRow)
	for _, line := range strings.Split(string(data), "\n") {
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
		result[pid] = psRow{
			rss:     rss * 1024, // ps reports RSS in KB
			cpuPct:  cpuPct,
			cpuTime: fields[3],
		}
	}
	return result
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
	home, err := EffectiveHome()
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

	pidMetrics := parsePsMetricsOutput(runPsMetrics(pidList))

	// Collect cwd for each PID via ps -E (graceful: missing PWD is skipped).
	pidCwd := parsePsEnvOutput(runPsEnv(pidList))

	// Query session_meta for each session to get repo/topic/work_type.
	type metaRow struct {
		repo     string
		topic    string
		workType string
	}
	sessionMeta := make(map[string]metaRow, len(sessions))
	rows, err := s.readDB.Query(`
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
	home, err := EffectiveHome()
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

// collectVMStat is platform-specific — see store_unix.go / store_windows.go.
// Currently only the darwin implementation produces real data; non-darwin
// platforms return a zero SystemMetrics. The caller in Whatsup gates the
// invocation on runtime.GOOS == "darwin".

// parseVMStatOutput parses vm_stat output to compute macOS memory pressure.
// Kept in the shared file so it is exercised by tests on all platforms even
// though only the darwin build path invokes vm_stat.
func parseVMStatOutput(data []byte) SystemMetrics {
	vals := make(map[string]int64)
	for _, line := range strings.Split(string(data), "\n") {
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
	s.readDB.QueryRow(`
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

	_, err = s.writeDB.Exec(`
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
	var paramNamesJSON, queryText string
	err := s.readDB.QueryRow(
		`SELECT query_text, param_names FROM query_templates WHERE name = ?`, name,
	).Scan(&queryText, &paramNamesJSON)
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

	rows, err := s.readDB.Query(`
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

		rows, err := s.readDB.Query(q, args...)
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

// pollGitHubForRepo fetches and upserts PRs and issues for a single repo.
func (s *Store) pollGitHubForRepo(ghPath, repo string) error {
	// Find the most recent updated_at for incremental fetches.
	var lastPR, lastIssue string
	s.readDB.QueryRow(`SELECT MAX(updated_at) FROM github_prs WHERE repo = ?`, repo).Scan(&lastPR)
	s.readDB.QueryRow(`SELECT MAX(updated_at) FROM github_issues WHERE repo = ?`, repo).Scan(&lastIssue)

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
		_, err := s.writeDB.Exec(`
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

		_, err := s.writeDB.Exec(`
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

// backfillDecisions catches up decision detection for sessions that have
// never been scanned (no decision_scan_state watermark). Safe to call on
// every startup; converges to a cheap no-op once every session is
// watermarked.
//
// 🎯T92: the old predicate ("no row in decisions") never converged — a
// session with zero decisions never produced a decisions row, so it was
// rescanned on every pass forever. Worse, on a large existing DB the first
// watermark-based pass would re-scan tens of thousands of sessions serially
// (each loading its full message history) — observed at ~200 sessions/min,
// i.e. hours of multi-core load for a one-time catch-up.
//
// The catch-up is split by whether decisions have ever been detected:
//
//   - decisions already exist → this DB has been backfilled before (the old
//     code ran detectDecisions over all existing content on every startup),
//     so every detectable pair in existing content is already stored.
//     Re-scanning would find nothing new. Instead, bulk-record each
//     unwatermarked session's watermark in ONE indexed aggregate query
//     (per-session MAX(message id)); the incremental forward path then takes
//     over for future appends. This turns the hours-long migration into a
//     single query.
//   - no decisions yet → a fresh import. Scan each candidate once to find
//     its pairs (the original behaviour), recording watermarks as it goes.
func backfillDecisions(db *sql.DB) {
	rows, err := db.Query(`
		SELECT DISTINCT sm.session_id, COALESCE(sm.repo, '')
		FROM session_meta sm
		WHERE NOT EXISTS (SELECT 1 FROM decision_scan_state st WHERE st.session_id = sm.session_id)`)
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
	if len(sessions) == 0 {
		return // fully converged — cheap no-op
	}

	var decisionsExist bool
	_ = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM decisions)`).Scan(&decisionsExist)

	if decisionsExist {
		// Cold migration on an already-backfilled DB: bulk-seed watermarks
		// for the unwatermarked candidates in one aggregate query rather
		// than re-scanning each session's history. The watermark is the
		// session's current MAX text-message id, so the forward path resumes
		// inclusively from it (boundary-safe) on the next append.
		res, err := db.Exec(`
			INSERT OR IGNORE INTO decision_scan_state (session_id, scanned_through_id, scanned_at)
			SELECT m.session_id, MAX(m.id), ?
			FROM messages m
			WHERE m.is_noise = 0 AND m.content_type = 'text'
			  AND m.session_id IN (
			    SELECT sm.session_id FROM session_meta sm
			    WHERE NOT EXISTS (SELECT 1 FROM decision_scan_state st WHERE st.session_id = sm.session_id)
			  )
			GROUP BY m.session_id`, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			slog.Warn("backfill decisions seed failed", "err", err)
			return
		}
		n, _ := res.RowsAffected()
		slog.Info("seeded decision watermarks (migration)", "sessions_seeded", n, "candidates", len(sessions))
		return
	}

	// Fresh import: scan each candidate once to detect its pairs.
	found := 0
	for _, sr := range sessions {
		detectDecisions(db, sr.id, sr.repo)
	}
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

	rows, err := s.readDB.Query(q, args...)
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
		rows, err := s.readDB.Query(q, args...)
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
		rows, err := s.readDB.Query(q, args...)
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
		if err := s.readDB.QueryRow(`
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
		occRows, err := s.readDB.Query(`
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

	var blob []byte
	err := s.readDB.QueryRow(`SELECT vector FROM image_embeddings WHERE image_id = ? AND error IS NULL`, similarTo).Scan(&blob)
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

	rows, err := s.readDB.Query(filterQ, filterArgs...)
	if err != nil {
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
	s.readDB.QueryRow(`SELECT model FROM image_embeddings WHERE error IS NULL GROUP BY model ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&model) //nolint:errcheck

	candidates, err := loadCandidateEmbeddings(s.readDB, model, candidateIDs)
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

	var results []ImageSearchResult
	for _, id := range topIDs {
		var r ImageSearchResult
		var origPath sql.NullString
		if err := s.readDB.QueryRow(`
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
		occRows, err := s.readDB.Query(`
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
