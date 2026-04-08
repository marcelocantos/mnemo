// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/marcelocantos/mcpbridge"

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
		"Stats": func(_ json.RawMessage) (any, error) {
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
		"ResolveNonce": makeMethod(func(p ResolveNonceParams) (any, error) {
			sid, err := s.ResolveNonce(p.Nonce)
			if err != nil {
				return nil, err
			}
			return map[string]string{"session_id": sid}, nil
		}),
	}
}

// makeMethod creates a MethodFunc that unmarshals params into type P.
func makeMethod[P any](fn func(P) (any, error)) mcpbridge.MethodFunc {
	return func(raw json.RawMessage) (any, error) {
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

// ResolveNonceParams matches the ResolveNonce method signature.
type ResolveNonceParams struct {
	Nonce string `json:"nonce"`
}
