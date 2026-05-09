// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package api exposes a thin JSON REST layer over the store.Backend so
// the web dashboard can pull data without going through MCP.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Handler wraps a store resolver and serves JSON REST endpoints.
// resolve("") returns the default user's backend, matching the behaviour
// of a local single-user deployment; multi-user deployments can pass a
// username explicitly if needed in future.
type Handler struct {
	resolve func(string) (store.Backend, error)
}

// New creates a Handler backed by resolve. Pass the same resolver used
// by the MCP layer so the dashboard always reflects the default user's
// store.
func New(resolve func(string) (store.Backend, error)) *Handler {
	return &Handler{resolve: resolve}
}

// backend returns the default user's store.Backend.
func (h *Handler) backend() (store.Backend, error) {
	return h.resolve("")
}

// RegisterRoutes attaches all /api/* routes to mux.
// No CORS headers are set: the dashboard is served same-origin from the
// same mux, so cross-origin access is not needed and would expose
// sensitive transcript data to any page the user visits.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/stats", getOnly(h.stats))
	mux.HandleFunc("/api/usage", getOnly(h.usage))
	mux.HandleFunc("/api/sessions", getOnly(h.sessions))
	mux.HandleFunc("/api/activity", getOnly(h.activity))
	mux.HandleFunc("/api/whatsup", getOnly(h.whatsup))
	mux.HandleFunc("/api/context", getOnly(h.context))
	mux.HandleFunc("/api/messages", getOnly(h.messages))
	mux.HandleFunc("/api/dbstats", getOnly(h.dbstats))
	mux.HandleFunc("/api/active", getOnly(h.active))
}

// getOnly rejects non-GET requests with 405 Method Not Allowed.
func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// stats serves GET /api/stats → StatsResult
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := mem.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// usage serves GET /api/usage?days=1&group_by=day|model|repo&repo=&model=
func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	days := clamp(queryInt(q.Get("days"), 1), 1, 365)
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "day"
	}
	repoFilter := q.Get("repo")
	modelFilter := q.Get("model")

	result, err := mem.Usage(store.UsageParams{
		Days:       days,
		RepoFilter: repoFilter,
		Model:      modelFilter,
		GroupBy:    groupBy,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// sessions serves GET /api/sessions?type=subagent&limit=20&repo=
func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	sessionType := q.Get("type")
	limit := clamp(queryInt(q.Get("limit"), 20), 1, 100)
	repo := q.Get("repo")

	result, err := mem.ListSessions(sessionType, 0, limit, "", repo, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// activity serves GET /api/activity?days=1&repo=
func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	days := clamp(queryInt(q.Get("days"), 1), 1, 365)
	repo := q.Get("repo")

	result, err := mem.RecentActivity(days, repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// whatsup serves GET /api/whatsup
func (h *Handler) whatsup(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := mem.Whatsup(false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, result)
}

// modelContextWindow returns the context window token limit for a model slug.
// Prefixes are matched longest-first so more-specific entries win.
func modelContextWindow(model string) int64 {
	// Models with non-default (>200K) windows.
	for _, entry := range []struct {
		prefix string
		tokens int64
	}{
		{"claude-opus-4", 1_000_000},
	} {
		if strings.HasPrefix(model, entry.prefix) {
			return entry.tokens
		}
	}
	return 200_000
}

// modelCostRates maps model slug prefixes to per-token costs in USD,
// mirroring the rates in store.go's modelCosts table.
var modelCostRates = []struct {
	prefix                                   string
	input, output, cacheRead, cacheWrite float64
}{
	{"claude-opus-4", 15.0 / 1e6, 75.0 / 1e6, 1.5 / 1e6, 18.75 / 1e6},
	{"claude-sonnet-4", 3.0 / 1e6, 15.0 / 1e6, 0.3 / 1e6, 3.75 / 1e6},
	{"claude-haiku-4", 0.80 / 1e6, 4.0 / 1e6, 0.08 / 1e6, 1.0 / 1e6},
}

// estimateCost returns the estimated USD cost for a set of token counts
// at the given model's rates. Falls back to Sonnet pricing for unknown models.
func estimateCost(model string, input, output, cacheRead, cacheWrite float64) float64 {
	for _, m := range modelCostRates {
		if strings.HasPrefix(model, m.prefix) {
			return input*m.input + output*m.output + cacheRead*m.cacheRead + cacheWrite*m.cacheWrite
		}
	}
	// Default to Sonnet pricing.
	m := modelCostRates[1]
	return input*m.input + output*m.output + cacheRead*m.cacheRead + cacheWrite*m.cacheWrite
}

// ContextRow is one session's peak context usage.
type ContextRow struct {
	SessionID         string  `json:"session_id"`
	SessionType       string  `json:"session_type"`
	Repo              string  `json:"repo"`
	WorkType          string  `json:"work_type"`
	Topic             string  `json:"topic"`
	Model             string  `json:"model"`
	PeakInputTokens   int64   `json:"peak_input_tokens"`
	ContextWindowSize int64   `json:"context_window_size"`
	PressurePct       float64 `json:"pressure_pct"`
	LastMsg           string  `json:"last_msg"`
}

// context serves GET /api/context?days=1&limit=20
// Returns per-session peak context pressure (peak input_tokens / context window).
func (h *Handler) context(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	days := clamp(queryInt(q.Get("days"), 1), 1, 365)
	limit := clamp(queryInt(q.Get("limit"), 20), 1, 100)

	rows, err := mem.QueryArgs(`
		SELECT
			s.session_id,
			s.session_type,
			COALESCE(m.repo, '')      AS repo,
			COALESCE(m.work_type, '') AS work_type,
			COALESCE(m.topic, '')     AS topic,
			COALESCE(e.model, '')     AS model,
			COALESCE(e.peak_input, 0) AS peak_input_tokens,
			s.last_msg
		FROM session_summary s
		LEFT JOIN session_meta m ON m.session_id = s.session_id
		LEFT JOIN (
			SELECT
				session_id,
				model,
				MAX(COALESCE(input_tokens, 0) + COALESCE(cache_read_tokens, 0)) AS peak_input
			FROM entries
			WHERE input_tokens IS NOT NULL
			GROUP BY session_id
		) e ON e.session_id = s.session_id
		WHERE s.last_msg >= datetime('now', '-' || ? || ' days')
		ORDER BY peak_input_tokens DESC
		LIMIT ?
	`, days, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]ContextRow, 0, len(rows))
	for _, row := range rows {
		model := str(row["model"])
		peak := int64(num(row["peak_input_tokens"]))
		window := modelContextWindow(model)
		pct := 0.0
		if window > 0 {
			pct = float64(peak) / float64(window) * 100
		}
		out = append(out, ContextRow{
			SessionID:         str(row["session_id"]),
			SessionType:       str(row["session_type"]),
			Repo:              str(row["repo"]),
			WorkType:          str(row["work_type"]),
			Topic:             str(row["topic"]),
			Model:             model,
			PeakInputTokens:   peak,
			ContextWindowSize: window,
			PressurePct:       pct,
			LastMsg:           str(row["last_msg"]),
		})
	}

	writeJSON(w, out)
}

// ActiveSession is a running claude process correlated with session metadata.
type ActiveSession struct {
	PID       int     `json:"pid"`
	SessionID string  `json:"session_id"`
	Repo      string  `json:"repo"`
	Cwd       string  `json:"cwd"`
	WorkType  string  `json:"work_type"`
	Topic     string  `json:"topic"`
	TotalMsgs int     `json:"total_msgs"`
	LastMsg   string  `json:"last_msg"`
	CPUPct    float64 `json:"cpu_pct"`
	RSSBytes  int64   `json:"rss_bytes"`
}

// active serves GET /api/active — finds running claude processes via ps,
// extracts session IDs from command-line args, and joins with DB metadata.
func (h *Handler) active(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	procs, err := listClaudeProcesses()
	if err != nil {
		slog.Warn("api/active: ps failed", "err", err)
		http.Error(w, "ps failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Collect all session IDs for a single batched DB query.
	var sessionIDs []string
	for _, p := range procs {
		if p.SessionID != "" {
			sessionIDs = append(sessionIDs, p.SessionID)
		}
	}

	// Build a map of session metadata keyed by session ID.
	meta := map[string]map[string]any{}
	if len(sessionIDs) > 0 {
		placeholders := strings.Repeat("?,", len(sessionIDs))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(sessionIDs))
		for i, id := range sessionIDs {
			args[i] = id
		}
		rows, err := mem.QueryArgs(`
			SELECT
				s.session_id,
				COALESCE(m.repo, '')      AS repo,
				COALESCE(m.cwd, '')       AS cwd,
				COALESCE(m.work_type, '') AS work_type,
				COALESCE(m.topic, '')     AS topic,
				COALESCE(s.total_msgs, 0) AS total_msgs,
				COALESCE(s.last_msg, '')  AS last_msg
			FROM session_summary s
			LEFT JOIN session_meta m ON m.session_id = s.session_id
			WHERE s.session_id IN (`+placeholders+`)
		`, args...)
		if err != nil {
			slog.Warn("api/active: metadata query failed", "err", err)
		} else {
			for _, row := range rows {
				if id := str(row["session_id"]); id != "" {
					meta[id] = row
				}
			}
		}
	}

	out := make([]ActiveSession, 0, len(procs))
	for _, p := range procs {
		as := ActiveSession{
			PID:       p.PID,
			SessionID: p.SessionID,
			CPUPct:    p.CPUPct,
			RSSBytes:  p.RSSBytes,
		}
		if row, ok := meta[p.SessionID]; ok {
			as.Repo = str(row["repo"])
			as.Cwd = str(row["cwd"])
			as.WorkType = str(row["work_type"])
			as.Topic = str(row["topic"])
			as.TotalMsgs = int(num(row["total_msgs"]))
			as.LastMsg = str(row["last_msg"])
		}
		out = append(out, as)
	}

	writeJSON(w, out)
}

// procInfo holds raw data from ps for one process.
type procInfo struct {
	PID       int
	CPUPct    float64
	RSSBytes  int64
	SessionID string
}

// listClaudeProcesses runs ps and returns one entry per process whose
// binary basename is exactly "claude". Using comm (basename) avoids
// false positives from iCloud, claudia, browser tabs, etc.
func listClaudeProcesses() ([]procInfo, error) {
	// pid,%cpu,rss,comm,args — comm is the basename, args is the full command line.
	data, err := exec.Command("ps", "-axo", "pid,%cpu,rss,comm,args").Output()
	if err != nil {
		return nil, err
	}
	return parsePsOutput(data), nil
}

// parsePsOutput parses the output of `ps -axo pid,%cpu,rss,comm,args` and
// returns one procInfo per line whose comm field is exactly "claude".
// Extracted so the parsing logic can be unit-tested without running ps.
func parsePsOutput(data []byte) []procInfo {
	var procs []procInfo
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 {
			continue // skip header
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Fields: pid cpu rss comm args...
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// Match only processes whose basename is exactly "claude".
		if fields[3] != "claude" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cpu, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			slog.Warn("api/active: parse cpu", "val", fields[1], "err", err)
		}
		rss, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			slog.Warn("api/active: parse rss", "val", fields[2], "err", err)
		}

		// Extract session ID from --resume <id> or --resume=<id> in args.
		sessionID := ""
		args := strings.Join(fields[4:], " ")
		if idx := strings.Index(args, "--resume"); idx >= 0 {
			rest := strings.TrimSpace(args[idx+len("--resume"):])
			rest = strings.TrimPrefix(rest, "=")
			rest = strings.TrimSpace(rest)
			if parts := strings.Fields(rest); len(parts) > 0 {
				sessionID = parts[0]
			}
		}

		procs = append(procs, procInfo{
			PID:       pid,
			CPUPct:    cpu,
			RSSBytes:  rss * 1024, // ps reports RSS in KB
			SessionID: sessionID,
		})
	}
	return procs
}

// DBStats holds database health metrics.
type DBStats struct {
	FileSizeBytes   int64   `json:"file_size_bytes"`
	Images          int64   `json:"images"`
	ImagesDescribed int64   `json:"images_described"`
	Decisions       int64   `json:"decisions"`
	GitCommits      int64   `json:"git_commits"`
	Compactions     int64   `json:"compactions"`
	AllTimeCostUSD  float64 `json:"all_time_cost_usd"`
	IngestDrift     int     `json:"ingest_drift"` // total files_on_disk - files_indexed across all streams
}

// dbstats serves GET /api/dbstats
func (h *Handler) dbstats(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// File size
	homeDir, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("api/dbstats: cannot determine home directory", "err", err)
	}
	dbPath := filepath.Join(homeDir, ".mnemo", "mnemo.db")
	var fileSize int64
	if fi, err := os.Stat(dbPath); err == nil {
		fileSize = fi.Size()
	}

	// Table counts
	countRows, err := mem.Query(`
		SELECT
			(SELECT COUNT(*) FROM images)             AS images,
			(SELECT COUNT(*) FROM image_descriptions) AS described,
			(SELECT COUNT(*) FROM decisions)          AS decisions,
			(SELECT COUNT(*) FROM git_commits)        AS git_commits,
			(SELECT COUNT(*) FROM compactions)        AS compactions
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Per-model token totals for accurate cost estimation.
	costRows, err := mem.Query(`
		SELECT
			COALESCE(model, '') AS model,
			COALESCE(SUM(input_tokens), 0)          AS input_tokens,
			COALESCE(SUM(output_tokens), 0)         AS output_tokens,
			COALESCE(SUM(cache_read_tokens), 0)     AS cache_read_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens
		FROM entries
		WHERE input_tokens IS NOT NULL
		GROUP BY model
	`)
	if err != nil {
		slog.Warn("api/dbstats: cost query failed", "err", err)
	}

	var allTimeCost float64
	for _, row := range costRows {
		allTimeCost += estimateCost(
			str(row["model"]),
			num(row["input_tokens"]),
			num(row["output_tokens"]),
			num(row["cache_read_tokens"]),
			num(row["cache_creation_tokens"]),
		)
	}

	// Ingest drift from stats
	stats, err := mem.Stats()
	if err != nil {
		slog.Warn("api/dbstats: stats failed", "err", err)
	}
	drift := 0
	if stats != nil {
		for _, s := range stats.Streams {
			if s.FilesOnDisk > s.FilesIndexed {
				drift += s.FilesOnDisk - s.FilesIndexed
			}
		}
	}

	out := DBStats{FileSizeBytes: fileSize, IngestDrift: drift, AllTimeCostUSD: allTimeCost}
	if len(countRows) == 1 {
		out.Images = int64(num(countRows[0]["images"]))
		out.ImagesDescribed = int64(num(countRows[0]["described"]))
		out.Decisions = int64(num(countRows[0]["decisions"]))
		out.GitCommits = int64(num(countRows[0]["git_commits"]))
		out.Compactions = int64(num(countRows[0]["compactions"]))
	}

	writeJSON(w, out)
}

// messages serves GET /api/messages?id=<session_id>&limit=20&offset=0
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	mem, err := h.backend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	id := q.Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	limit := clamp(queryInt(q.Get("limit"), 20), 1, 100)
	offset := queryInt(q.Get("offset"), 0)

	msgs, err := mem.ReadSession(id, "", offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, msgs)
}

func str(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func queryInt(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
