// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// fakeBackend is a minimal stub that satisfies store.Backend for handler tests.
// Only the methods called by the api handlers are implemented; the rest panic
// so any unexpected call is immediately visible.
type fakeBackend struct {
	statsResult   *store.StatsResult
	usageResult   *store.UsageResult
	sessions      []store.SessionInfo
	activity      []store.RecentActivityInfo
	whatsupResult *store.WhatsupResult
	queryResult   []map[string]any
	messages      []store.SessionMessage
}

func (f *fakeBackend) Stats() (*store.StatsResult, error) { return f.statsResult, nil }
func (f *fakeBackend) Usage(p store.UsageParams) (*store.UsageResult, error) {
	return f.usageResult, nil
}
func (f *fakeBackend) ListSessions(sessionType string, minMessages int, limit int, projectFilter, repoFilter, workTypeFilter string) ([]store.SessionInfo, error) {
	return f.sessions, nil
}
func (f *fakeBackend) RecentActivity(days int, repoFilter string) ([]store.RecentActivityInfo, error) {
	return f.activity, nil
}
func (f *fakeBackend) Whatsup(postmortem bool) (*store.WhatsupResult, error) {
	return f.whatsupResult, nil
}
func (f *fakeBackend) Query(query string, args ...any) ([]map[string]any, error) {
	return f.queryResult, nil
}
func (f *fakeBackend) ReadSession(sessionID string, role string, offset int, limit int) ([]store.SessionMessage, error) {
	return f.messages, nil
}

// Unimplemented methods — panic on unexpected call.
func (f *fakeBackend) Search(query string, limit int, sessionType, repoFilter string, contextBefore, contextAfter int, substantiveOnly bool) ([]store.SearchResult, error) {
	panic("unexpected Search call")
}
func (f *fakeBackend) ListRepos(filter string) ([]store.RepoInfo, error) {
	panic("unexpected ListRepos call")
}
func (f *fakeBackend) ResolveNonce(nonce string) (string, error) {
	panic("unexpected ResolveNonce call")
}
func (f *fakeBackend) Status(days int, repoFilter string, maxSessions int, maxExcerpts int, truncateLen int) (*store.StatusResult, error) {
	panic("unexpected Status call")
}
func (f *fakeBackend) UpsertReconciledCost(date string, costUSD float64) error {
	panic("unexpected UpsertReconciledCost call")
}
func (f *fakeBackend) SearchMemories(query string, memType string, project string, limit int) ([]store.MemoryInfo, error) {
	panic("unexpected SearchMemories call")
}
func (f *fakeBackend) GetMemory(project, name string) (*store.MemoryInfo, error) {
	panic("unexpected GetMemory call")
}
func (f *fakeBackend) SearchSkills(query string, limit int) ([]store.SkillInfo, error) {
	panic("unexpected SearchSkills call")
}
func (f *fakeBackend) SearchClaudeConfigs(query string, repo string, limit int) ([]store.ClaudeConfigInfo, error) {
	panic("unexpected SearchClaudeConfigs call")
}
func (f *fakeBackend) SearchAuditLogs(query string, repo string, skill string, limit int) ([]store.AuditEntryInfo, error) {
	panic("unexpected SearchAuditLogs call")
}
func (f *fakeBackend) SearchTargets(query string, repo string, status string, limit int) ([]store.TargetInfo, error) {
	panic("unexpected SearchTargets call")
}
func (f *fakeBackend) SearchPlans(query string, repo string, limit int) ([]store.PlanInfo, error) {
	panic("unexpected SearchPlans call")
}
func (f *fakeBackend) SearchDocs(query string, repo string, kind string, limit int) ([]store.DocInfo, error) {
	panic("unexpected SearchDocs call")
}
func (f *fakeBackend) SearchSynthesis(query string, taxonomy string, repo string, limit int) ([]store.DocInfo, error) {
	panic("unexpected SearchSynthesis call")
}
func (f *fakeBackend) WhoRan(pattern string, days int, repoFilter string, limit int) ([]store.WhoRanResult, error) {
	panic("unexpected WhoRan call")
}
func (f *fakeBackend) Permissions(days int, repoFilter string, limit int) (*store.PermissionsResult, error) {
	panic("unexpected Permissions call")
}
func (f *fakeBackend) SearchCI(query string, repo string, conclusion string, days int, limit int) ([]store.CIRun, error) {
	panic("unexpected SearchCI call")
}
func (f *fakeBackend) DefineTemplate(name, description, queryText string, paramNames []string) error {
	panic("unexpected DefineTemplate call")
}
func (f *fakeBackend) EvaluateTemplate(name string, params map[string]string) ([]map[string]any, error) {
	panic("unexpected EvaluateTemplate call")
}
func (f *fakeBackend) ListTemplates() ([]store.QueryTemplate, error) {
	panic("unexpected ListTemplates call")
}
func (f *fakeBackend) LiveSessions() map[string]int { panic("unexpected LiveSessions call") }
func (f *fakeBackend) Predecessor(sessionID string) (string, error) {
	panic("unexpected Predecessor call")
}
func (f *fakeBackend) Successor(sessionID string) (string, error) {
	panic("unexpected Successor call")
}
func (f *fakeBackend) Chain(sessionID string) ([]store.ChainLink, error) {
	panic("unexpected Chain call")
}
func (f *fakeBackend) SearchDecisions(query string, repo string, days int, limit int) ([]store.DecisionInfo, error) {
	panic("unexpected SearchDecisions call")
}
func (f *fakeBackend) SearchGitHubActivity(query string, repo string, state string, author string, activityType string, days int, limit int) ([]store.GitHubActivityResult, error) {
	panic("unexpected SearchGitHubActivity call")
}
func (f *fakeBackend) SearchCommits(query string, repo string, author string, days int, limit int) ([]store.GitCommit, error) {
	panic("unexpected SearchCommits call")
}
func (f *fakeBackend) DiscoverPatterns(days int, repoFilter string, minOccurrences int) ([]store.PatternCandidate, error) {
	panic("unexpected DiscoverPatterns call")
}
func (f *fakeBackend) SearchImages(query string, repo string, session string, days int, limit int) ([]store.ImageSearchResult, error) {
	panic("unexpected SearchImages call")
}
func (f *fakeBackend) SearchImagesFiltered(query string, repo string, session string, days int, limit int, searchFields string) ([]store.ImageSearchResult, error) {
	panic("unexpected SearchImagesFiltered call")
}
func (f *fakeBackend) SearchImagesSemantic(query string, repo string, session string, days int, limit int) ([]store.ImageSearchResult, error) {
	panic("unexpected SearchImagesSemantic call")
}
func (f *fakeBackend) SearchImagesSimilar(similarTo int, repo string, session string, days int, limit int) ([]store.ImageSearchResult, error) {
	panic("unexpected SearchImagesSimilar call")
}
func (f *fakeBackend) ToolResult(sessionID, toolUseID string, offset, truncateLen int) (*store.ToolResultPayload, error) {
	panic("unexpected ToolResult call")
}
func (f *fakeBackend) ChainCompactions(sessionID string) ([]store.Compaction, error) {
	panic("unexpected ChainCompactions call")
}
func (f *fakeBackend) CompactionsForConnection(connectionID string) ([]store.Compaction, error) {
	panic("unexpected CompactionsForConnection call")
}
func (f *fakeBackend) SessionTokens(sessionID string) (int64, int64, error) {
	panic("unexpected SessionTokens call")
}
func (f *fakeBackend) CompactionTokens(sessionID string) (int64, int64, error) {
	panic("unexpected CompactionTokens call")
}
func (f *fakeBackend) RecordConnectionOpen(connectionID string, pid int, acceptedAt time.Time) {
	panic("unexpected RecordConnectionOpen call")
}
func (f *fakeBackend) RecordConnectionSession(connectionID, sessionID string) {
	panic("unexpected RecordConnectionSession call")
}
func (f *fakeBackend) ConnectionsForSession(sessionID string) ([]store.ConnectionSession, error) {
	panic("unexpected ConnectionsForSession call")
}
func (f *fakeBackend) InferChainHeuristic(sessionID string, limit int) ([]store.ChainCandidate, error) {
	panic("unexpected InferChainHeuristic call")
}
func (f *fakeBackend) SessionStructure(sessionID string) (*store.SessionStructure, error) {
	panic("unexpected SessionStructure call")
}
func (f *fakeBackend) LocateUUID(prefix string, contextBefore, contextAfter int) ([]store.UUIDMatch, error) {
	panic("unexpected LocateUUID call")
}
func (f *fakeBackend) ReworkHistory(targetID string, repo string, limit int) ([]store.ReworkAttempt, error) {
	panic("unexpected ReworkHistory call")
}

// newTestHandler returns a Handler and ServeMux wired up with a fakeBackend.
func newTestHandler(fb *fakeBackend) (*Handler, *http.ServeMux) {
	h := New(func(string) (store.Backend, error) { return fb, nil })
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

func TestStatsHandler(t *testing.T) {
	fb := &fakeBackend{
		statsResult: &store.StatsResult{TotalSessions: 42, TotalMessages: 1000},
	}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var result store.StatsResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.TotalSessions != 42 {
		t.Errorf("want TotalSessions=42, got %d", result.TotalSessions)
	}
}

func TestStatsNoCORSHeader(t *testing.T) {
	fb := &fakeBackend{statsResult: &store.StatsResult{}}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats", nil))

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("CORS header should not be set, got %q", got)
	}
}

func TestUsageHandler(t *testing.T) {
	fb := &fakeBackend{
		usageResult: &store.UsageResult{},
	}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/usage?days=7&group_by=model", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("want application/json, got %q", ct)
	}
}

func TestSessionsHandler(t *testing.T) {
	fb := &fakeBackend{
		sessions: []store.SessionInfo{
			{SessionID: "abc123", SessionType: "interactive", TotalMsgs: 10},
		},
	}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/sessions?type=interactive&limit=5", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var result []store.SessionInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result) != 1 || result[0].SessionID != "abc123" {
		t.Errorf("unexpected sessions: %+v", result)
	}
}

func TestMessagesHandlerMissingID(t *testing.T) {
	fb := &fakeBackend{}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/messages", nil))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestMessagesHandler(t *testing.T) {
	fb := &fakeBackend{
		messages: []store.SessionMessage{
			{Role: "user", Text: "hello"},
		},
	}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/messages?id=abc123&limit=10", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var result []store.SessionMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result) != 1 || result[0].Role != "user" {
		t.Errorf("unexpected messages: %+v", result)
	}
}

func TestContextHandler(t *testing.T) {
	fb := &fakeBackend{
		queryResult: []map[string]any{
			{
				"session_id":        "sess-1",
				"session_type":      "interactive",
				"repo":              "myrepo",
				"work_type":         "feature",
				"topic":             "test topic",
				"model":             "claude-sonnet-4-5",
				"peak_input_tokens": float64(50_000),
				"last_msg":          "2026-05-01T12:00:00Z",
			},
		},
	}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/context?days=1&limit=10", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var result []ContextRow
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("want 1 row, got %d", len(result))
	}
	row := result[0]
	if row.ContextWindowSize != 200_000 {
		t.Errorf("want 200K window for sonnet, got %d", row.ContextWindowSize)
	}
	wantPct := float64(50_000) / float64(200_000) * 100
	if row.PressurePct != wantPct {
		t.Errorf("want pressure %.2f%%, got %.2f%%", wantPct, row.PressurePct)
	}
}

func TestModelContextWindow(t *testing.T) {
	cases := []struct {
		model string
		want  int64
	}{
		{"claude-opus-4-5[1m]", 1_000_000},
		{"claude-sonnet-4-6[1m]", 1_000_000},
		{"claude-opus-4-5", 200_000},
		{"claude-opus-4", 200_000},
		{"claude-sonnet-4-6", 200_000},
		{"claude-haiku-4-5", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"", 200_000},
		{"unknown-model", 200_000},
	}
	for _, c := range cases {
		if got := modelContextWindow(c.model); got != c.want {
			t.Errorf("modelContextWindow(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestGetOnlyRejectsPost(t *testing.T) {
	fb := &fakeBackend{statsResult: &store.StatsResult{}}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/stats", nil))

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET" {
		t.Errorf("want Allow: GET, got %q", allow)
	}
}

func TestResolveError(t *testing.T) {
	h := New(func(string) (store.Backend, error) {
		return nil, fmt.Errorf("no backend")
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/stats", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rr.Code)
	}
}

func TestEstimateCost(t *testing.T) {
	cases := []struct {
		name                                 string
		model                                string
		input, output, cacheRead, cacheWrite float64
		wantMin, wantMax                     float64
	}{
		{
			name:  "opus",
			model: "claude-opus-4-5",
			input: 1_000_000, output: 100_000, cacheRead: 500_000, cacheWrite: 50_000,
			wantMin: 20.0, wantMax: 30.0, // ~$15 input + $7.5 output + $0.75 cache_read + $0.9375 cache_write
		},
		{
			name:  "sonnet",
			model: "claude-sonnet-4-6",
			input: 1_000_000, output: 100_000, cacheRead: 500_000, cacheWrite: 50_000,
			wantMin: 4.0, wantMax: 5.0, // ~$3 input + $1.5 output + $0.15 cache_read + $0.1875 cache_write
		},
		{
			name:  "haiku",
			model: "claude-haiku-4-5",
			input: 1_000_000, output: 100_000, cacheRead: 500_000, cacheWrite: 50_000,
			wantMin: 1.0, wantMax: 2.0, // ~$0.80 input + $0.40 output + $0.04 cache_read + $0.05 cache_write
		},
		{
			name:  "unknown defaults to sonnet",
			model: "unknown-model",
			input: 1_000_000, output: 100_000, cacheRead: 500_000, cacheWrite: 50_000,
			wantMin: 4.0, wantMax: 5.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := estimateCost(c.model, c.input, c.output, c.cacheRead, c.cacheWrite)
			if got < c.wantMin || got > c.wantMax {
				t.Errorf("estimateCost(%q) = %.4f, want [%.1f, %.1f]", c.model, got, c.wantMin, c.wantMax)
			}
		})
	}
}

func TestDBStatsHandler(t *testing.T) {
	fb := &fakeBackend{
		queryResult: []map[string]any{
			{
				"images":      float64(10),
				"described":   float64(5),
				"decisions":   float64(20),
				"git_commits": float64(100),
				"compactions": float64(3),
			},
		},
		statsResult: &store.StatsResult{},
	}
	_, mux := newTestHandler(fb)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/api/dbstats", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var result DBStats
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Images != 10 {
		t.Errorf("want Images=10, got %d", result.Images)
	}
	if result.Decisions != 20 {
		t.Errorf("want Decisions=20, got %d", result.Decisions)
	}
}

func TestClampBounds(t *testing.T) {
	if got := clamp(0, 1, 365); got != 1 {
		t.Errorf("clamp(0,1,365) = %d, want 1", got)
	}
	if got := clamp(500, 1, 365); got != 365 {
		t.Errorf("clamp(500,1,365) = %d, want 365", got)
	}
	if got := clamp(30, 1, 365); got != 30 {
		t.Errorf("clamp(30,1,365) = %d, want 30", got)
	}
}

func TestListClaudeProcesses(t *testing.T) {
	// Parse a synthetic ps line similar to real output.
	// We can't call the real ps in a unit test, so we test the parsing helper
	// by constructing the raw bytes it would produce and verifying extraction.
	lines := []string{
		"  PID %CPU   RSS COMM  ARGS",
		"12345  2.3 65536 claude /usr/local/bin/claude --resume abc-def-123 --model sonnet",
		"99999  0.0 12288 iCloud iCloud Daemon",         // should be excluded
		"  777  0.1  8192 claudia /usr/bin/claudia",     // should be excluded
		"54321  1.0 32768 claude /usr/local/bin/claude", // fresh session, no --resume
	}
	input := strings.Join(lines, "\n") + "\n"

	// Directly exercise the parsing logic via listClaudeProcesses by mocking
	// the ps output. Since exec.Command is hard to mock without DI, we test
	// the parsing logic extracted from it instead.
	procs := parsePsOutput([]byte(input))

	if len(procs) != 2 {
		t.Fatalf("want 2 claude procs, got %d: %+v", len(procs), procs)
	}
	if procs[0].PID != 12345 {
		t.Errorf("want PID 12345, got %d", procs[0].PID)
	}
	if procs[0].SessionID != "abc-def-123" {
		t.Errorf("want sessionID abc-def-123, got %q", procs[0].SessionID)
	}
	if procs[0].RSSBytes != 65536*1024 {
		t.Errorf("want RSSBytes %d, got %d", 65536*1024, procs[0].RSSBytes)
	}
	if procs[1].PID != 54321 {
		t.Errorf("want PID 54321, got %d", procs[1].PID)
	}
	if procs[1].SessionID != "" {
		t.Errorf("want empty sessionID for fresh session, got %q", procs[1].SessionID)
	}
}
