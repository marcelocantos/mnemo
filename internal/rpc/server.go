// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"encoding/json"
	"fmt"

	"github.com/marcelocantos/mnemo/internal/bridge"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// Server wraps mcpbridge.Server with mnemo-specific RPC methods.
type Server struct {
	bridge *mcpbridge.Server
}

// ServerOption configures a Server.
type ServerOption func(*serverConfig)

type serverConfig struct {
	toolDefs []mcp.Tool
}

// WithToolDefs overrides the default tool definitions.
func WithToolDefs(defs []mcp.Tool) ServerOption {
	return func(c *serverConfig) { c.toolDefs = defs }
}

// NewServer creates an RPC server bound to the default socket path.
func NewServer(s *store.Store, opts ...ServerOption) (*Server, error) {
	return NewServerAt(s, SocketPath(), opts...)
}

// NewServerAt creates an RPC server bound to the given socket path.
func NewServerAt(s *store.Store, sockPath string, opts ...ServerOption) (*Server, error) {
	cfg := &serverConfig{
		toolDefs: tools.Definitions(),
	}
	for _, opt := range opts {
		opt(cfg)
	}

	bridge, err := mcpbridge.NewServer(mcpbridge.DaemonConfig{
		SocketPath:   sockPath,
		Tools:        cfg.toolDefs,
		Handler:      tools.NewHandler(s),
		ExtraMethods: extraMethods(s),
	})
	if err != nil {
		return nil, err
	}
	return &Server{bridge: bridge}, nil
}

// Serve accepts connections. Blocks until the listener is closed.
func (s *Server) Serve() error { return s.bridge.Serve() }

// Close shuts down the listener and all active connections.
func (s *Server) Close() error { return s.bridge.Close() }

// extraMethods registers mnemo-specific RPC methods (Search, ListSessions, etc.)
// that the proxy uses for typed access beyond the generic CallTool path.
func extraMethods(s *store.Store) map[string]mcpbridge.MethodFunc {
	return map[string]mcpbridge.MethodFunc{
		"Search": makeMethod(func(p SearchParams) (any, error) {
			return s.Search(p.Query, p.Limit, p.SessionType, p.RepoFilter, p.ContextBefore, p.ContextAfter, p.SubstantiveOnly)
		}),
		"ListSessions": makeMethod(func(p ListSessionsParams) (any, error) {
			return s.ListSessions(p.SessionType, p.MinMessages, p.Limit, p.ProjectFilter, p.RepoFilter, p.WorkTypeFilter)
		}),
		"ReadSession": makeMethod(func(p ReadSessionParams) (any, error) {
			return s.ReadSession(p.SessionID, p.Role, p.Offset, p.Limit)
		}),
		"Query": makeMethod(func(p QueryParams) (any, error) {
			return s.Query(p.Query)
		}),
		"Stats": func(_ mcpbridge.ConnContext, _ json.RawMessage) (any, error) {
			return s.Stats()
		},
		"ListRepos": makeMethod(func(p ListReposParams) (any, error) {
			return s.ListRepos(p.Filter)
		}),
		"RecentActivity": makeMethod(func(p RecentActivityParams) (any, error) {
			return s.RecentActivity(p.Days, p.RepoFilter)
		}),
		"Status": makeMethod(func(p StatusParams) (any, error) {
			return s.Status(p.Days, p.RepoFilter, p.MaxSessions, p.MaxExcerpts, p.TruncateLen)
		}),
		"Usage": makeMethod(func(p UsageParams) (any, error) {
			return s.Usage(p.Days, p.RepoFilter, p.Model, p.GroupBy)
		}),
		"SearchMemories": makeMethod(func(p SearchMemoriesParams) (any, error) {
			return s.SearchMemories(p.Query, p.MemoryType, p.Project, p.Limit)
		}),
		"SearchSkills": makeMethod(func(p SearchSkillsParams) (any, error) {
			return s.SearchSkills(p.Query, p.Limit)
		}),
		"SearchClaudeConfigs": makeMethod(func(p SearchClaudeConfigsParams) (any, error) {
			return s.SearchClaudeConfigs(p.Query, p.Repo, p.Limit)
		}),
		"SearchAuditLogs": makeMethod(func(p SearchAuditLogsParams) (any, error) {
			return s.SearchAuditLogs(p.Query, p.Repo, p.Skill, p.Limit)
		}),
		"SearchTargets": makeMethod(func(p SearchTargetsParams) (any, error) {
			return s.SearchTargets(p.Query, p.Repo, p.Status, p.Limit)
		}),
		"SearchPlans": makeMethod(func(p SearchPlansParams) (any, error) {
			return s.SearchPlans(p.Query, p.Repo, p.Limit)
		}),
		"SearchDocs": makeMethod(func(p SearchDocsParams) (any, error) {
			return s.SearchDocs(p.Query, p.Repo, p.Kind, p.Limit)
		}),
		"WhoRan": makeMethod(func(p WhoRanParams) (any, error) {
			return s.WhoRan(p.Pattern, p.Days, p.RepoFilter, p.Limit)
		}),
		"SearchCI": makeMethod(func(p SearchCIParams) (any, error) {
			return s.SearchCI(p.Query, p.Repo, p.Conclusion, p.Days, p.Limit)
		}),
		"ResolveNonce": makeMethod(func(p ResolveNonceParams) (any, error) {
			sid, err := s.ResolveNonce(p.Nonce)
			if err != nil {
				return nil, err
			}
			return map[string]string{"session_id": sid}, nil
		}),
		"Permissions": makeMethod(func(p PermissionsParams) (any, error) {
			return s.Permissions(p.Days, p.RepoFilter, p.Limit)
		}),
		"LiveSessions": func(_ mcpbridge.ConnContext, _ json.RawMessage) (any, error) {
			return s.LiveSessions(), nil
		},
		"Chain": makeMethod(func(p ChainParams) (any, error) {
			return s.Chain(p.SessionID)
		}),
		"SearchDecisions": makeMethod(func(p SearchDecisionsParams) (any, error) {
			return s.SearchDecisions(p.Query, p.Repo, p.Days, p.Limit)
		}),
		"Whatsup": makeMethod(func(p struct{ Postmortem bool }) (any, error) {
			return s.Whatsup(p.Postmortem)
		}),
		"SearchGitHubActivity": makeMethod(func(p SearchGitHubActivityParams) (any, error) {
			return s.SearchGitHubActivity(p.Query, p.Repo, p.State, p.Author, p.ActivityType, p.Days, p.Limit)
		}),
		"SearchCommits": makeMethod(func(p SearchCommitsParams) (any, error) {
			return s.SearchCommits(p.Query, p.Repo, p.Author, p.Days, p.Limit)
		}),
		"DefineTemplate": makeMethod(func(p DefineTemplateParams) (any, error) {
			return nil, s.DefineTemplate(p.Name, p.Description, p.QueryText, p.ParamNames)
		}),
		"EvaluateTemplate": makeMethod(func(p EvaluateTemplateParams) (any, error) {
			return s.EvaluateTemplate(p.Name, p.Params)
		}),
		"ListTemplates": func(_ mcpbridge.ConnContext, _ json.RawMessage) (any, error) {
			return s.ListTemplates()
		},
		"DiscoverPatterns": makeMethod(func(p DiscoverPatternsParams) (any, error) {
			return s.DiscoverPatterns(p.Days, p.RepoFilter, p.MinOccurrences)
		}),
		"SearchImages": makeMethod(func(p SearchImagesParams) (any, error) {
			return s.SearchImagesFiltered(p.Query, p.Repo, p.Session, p.Days, p.Limit, p.SearchFields)
		}),
		"SearchImagesSemantic": makeMethod(func(p SearchImagesSemanticParams) (any, error) {
			return s.SearchImagesSemantic(p.Query, p.Repo, p.Session, p.Days, p.Limit)
		}),
		"SearchImagesSimilar": makeMethod(func(p SearchImagesSimilarParams) (any, error) {
			return s.SearchImagesSimilar(p.SimilarTo, p.Repo, p.Session, p.Days, p.Limit)
		}),
		"ChainCompactions": makeMethod(func(p ChainCompactionsParams) (any, error) {
			return s.ChainCompactions(p.SessionID)
		}),
		"SessionTokens": makeMethod(func(p ChainCompactionsParams) (any, error) {
			in, out, err := s.SessionTokens(p.SessionID)
			return map[string]int64{"input": in, "output": out}, err
		}),
		"CompactionTokens": makeMethod(func(p ChainCompactionsParams) (any, error) {
			in, out, err := s.CompactionTokens(p.SessionID)
			return map[string]int64{"input": in, "output": out}, err
		}),
	}
}

// makeMethod creates a MethodFunc that unmarshals params into type P.
// The connection context is accepted but ignored by the vast majority
// of typed methods; methods that need connection identity use
// makeMethodCC below.
func makeMethod[P any](fn func(P) (any, error)) mcpbridge.MethodFunc {
	return func(_ mcpbridge.ConnContext, raw json.RawMessage) (any, error) {
		var p P
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return fn(p)
	}
}

// --- Param types for mnemo-specific RPC methods ---

// SearchParams matches the Search method signature.
type SearchParams struct {
	Query           string `json:"query"`
	Limit           int    `json:"limit"`
	SessionType     string `json:"session_type"`
	RepoFilter      string `json:"repo_filter"`
	ContextBefore   int    `json:"context_before"`
	ContextAfter    int    `json:"context_after"`
	SubstantiveOnly bool   `json:"substantive_only"`
}

// ListSessionsParams matches the ListSessions method signature.
type ListSessionsParams struct {
	SessionType    string `json:"session_type"`
	MinMessages    int    `json:"min_messages"`
	Limit          int    `json:"limit"`
	ProjectFilter  string `json:"project_filter"`
	RepoFilter     string `json:"repo_filter"`
	WorkTypeFilter string `json:"work_type_filter"`
}

// ReadSessionParams matches the ReadSession method signature.
type ReadSessionParams struct {
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

// QueryParams matches the Query method signature.
type QueryParams struct {
	Query string `json:"query"`
}

// ListReposParams matches the ListRepos method signature.
type ListReposParams struct {
	Filter string `json:"filter"`
}

// RecentActivityParams matches the RecentActivity method signature.
type RecentActivityParams struct {
	Days       int    `json:"days"`
	RepoFilter string `json:"repo_filter"`
}

// StatusParams matches the Status method signature.
type StatusParams struct {
	Days        int    `json:"days"`
	RepoFilter  string `json:"repo_filter"`
	MaxSessions int    `json:"max_sessions"`
	MaxExcerpts int    `json:"max_excerpts"`
	TruncateLen int    `json:"truncate_len"`
}

// SearchMemoriesParams matches the SearchMemories method signature.
type SearchMemoriesParams struct {
	Query      string `json:"query"`
	MemoryType string `json:"memory_type"`
	Project    string `json:"project"`
	Limit      int    `json:"limit"`
}

// UsageParams matches the Usage method signature.
type UsageParams struct {
	Days       int    `json:"days"`
	RepoFilter string `json:"repo_filter"`
	Model      string `json:"model"`
	GroupBy    string `json:"group_by"`
}

// SearchSkillsParams matches the SearchSkills method signature.
type SearchSkillsParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// SearchClaudeConfigsParams matches the SearchClaudeConfigs method signature.
type SearchClaudeConfigsParams struct {
	Query string `json:"query"`
	Repo  string `json:"repo"`
	Limit int    `json:"limit"`
}

// SearchAuditLogsParams matches the SearchAuditLogs method signature.
type SearchAuditLogsParams struct {
	Query string `json:"query"`
	Repo  string `json:"repo"`
	Skill string `json:"skill"`
	Limit int    `json:"limit"`
}

// SearchTargetsParams matches the SearchTargets method signature.
type SearchTargetsParams struct {
	Query  string `json:"query"`
	Repo   string `json:"repo"`
	Status string `json:"status"`
	Limit  int    `json:"limit"`
}

// SearchPlansParams matches the SearchPlans method signature.
type SearchPlansParams struct {
	Query string `json:"query"`
	Repo  string `json:"repo"`
	Limit int    `json:"limit"`
}

// SearchDocsParams matches the SearchDocs method signature.
type SearchDocsParams struct {
	Query string `json:"query"`
	Repo  string `json:"repo"`
	Kind  string `json:"kind"`
	Limit int    `json:"limit"`
}

// WhoRanParams matches the WhoRan method signature.
type WhoRanParams struct {
	Pattern    string `json:"pattern"`
	Days       int    `json:"days"`
	RepoFilter string `json:"repo_filter"`
	Limit      int    `json:"limit"`
}

// SearchCIParams matches the SearchCI method signature.
type SearchCIParams struct {
	Query      string `json:"query"`
	Repo       string `json:"repo"`
	Conclusion string `json:"conclusion"`
	Days       int    `json:"days"`
	Limit      int    `json:"limit"`
}

// ResolveNonceParams matches the ResolveNonce method signature.
type ResolveNonceParams struct {
	Nonce string `json:"nonce"`
}

// PermissionsParams matches the Permissions method signature.
type PermissionsParams struct {
	Days       int    `json:"days"`
	RepoFilter string `json:"repo_filter"`
	Limit      int    `json:"limit"`
}

// ChainParams matches the Chain method signature.
type ChainParams struct {
	SessionID string `json:"session_id"`
}

// SearchDecisionsParams matches the SearchDecisions method signature.
type SearchDecisionsParams struct {
	Query string `json:"query"`
	Repo  string `json:"repo"`
	Days  int    `json:"days"`
	Limit int    `json:"limit"`
}

// DefineTemplateParams matches the DefineTemplate method signature.
type DefineTemplateParams struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	QueryText   string   `json:"query_text"`
	ParamNames  []string `json:"param_names"`
}

// EvaluateTemplateParams matches the EvaluateTemplate method signature.
type EvaluateTemplateParams struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params"`
}

// SearchGitHubActivityParams matches the SearchGitHubActivity method signature.
type SearchGitHubActivityParams struct {
	Query        string `json:"query"`
	Repo         string `json:"repo"`
	State        string `json:"state"`
	Author       string `json:"author"`
	ActivityType string `json:"activity_type"`
	Days         int    `json:"days"`
	Limit        int    `json:"limit"`
}

// SearchCommitsParams matches the SearchCommits method signature.
type SearchCommitsParams struct {
	Query  string `json:"query"`
	Repo   string `json:"repo"`
	Author string `json:"author"`
	Days   int    `json:"days"`
	Limit  int    `json:"limit"`
}

// DiscoverPatternsParams matches the DiscoverPatterns method signature.
type DiscoverPatternsParams struct {
	Days           int    `json:"days"`
	RepoFilter     string `json:"repo_filter"`
	MinOccurrences int    `json:"min_occurrences"`
}

// SearchImagesParams matches the SearchImages method signature.
type SearchImagesParams struct {
	Query        string `json:"query"`
	Repo         string `json:"repo"`
	Session      string `json:"session"`
	Days         int    `json:"days"`
	Limit        int    `json:"limit"`
	SearchFields string `json:"search_fields"`
}

// SearchImagesSemanticParams matches the SearchImagesSemantic method signature.
type SearchImagesSemanticParams struct {
	Query   string `json:"query"`
	Repo    string `json:"repo"`
	Session string `json:"session"`
	Days    int    `json:"days"`
	Limit   int    `json:"limit"`
}

// SearchImagesSimilarParams matches the SearchImagesSimilar method signature.
type SearchImagesSimilarParams struct {
	SimilarTo int    `json:"similar_to"`
	Repo      string `json:"repo"`
	Session   string `json:"session"`
	Days      int    `json:"days"`
	Limit     int    `json:"limit"`
}

// ChainCompactionsParams matches the ChainCompactions method signature.
type ChainCompactionsParams struct {
	SessionID string `json:"session_id"`
}
