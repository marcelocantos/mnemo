// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"encoding/json"
	"fmt"

	"github.com/marcelocantos/mcpbridge"

	"github.com/marcelocantos/mnemo/internal/store"
)

// Proxy wraps mcpbridge.ToolProxy with mnemo-specific typed RPC methods.
type Proxy struct {
	*mcpbridge.ToolProxy
	client *mcpbridge.Client
}

// NewProxy creates a proxy for mnemo-specific and generic tool operations.
func NewProxy(c *mcpbridge.Client) *Proxy {
	return &Proxy{
		ToolProxy: mcpbridge.NewToolProxy(c),
		client:    c,
	}
}

func (p *Proxy) Search(query string, limit int, sessionType, repoFilter string, contextBefore, contextAfter int, substantiveOnly bool) ([]store.SearchResult, error) {
	raw, err := p.client.Call("Search", SearchParams{
		Query:           query,
		Limit:           limit,
		SessionType:     sessionType,
		RepoFilter:      repoFilter,
		ContextBefore:   contextBefore,
		ContextAfter:    contextAfter,
		SubstantiveOnly: substantiveOnly,
	})
	if err != nil {
		return nil, err
	}
	var results []store.SearchResult
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) ListSessions(sessionType string, minMessages int, limit int, projectFilter, repoFilter, workTypeFilter string) ([]store.SessionInfo, error) {
	raw, err := p.client.Call("ListSessions", ListSessionsParams{
		SessionType:    sessionType,
		MinMessages:    minMessages,
		Limit:          limit,
		ProjectFilter:  projectFilter,
		RepoFilter:     repoFilter,
		WorkTypeFilter: workTypeFilter,
	})
	if err != nil {
		return nil, err
	}
	var results []store.SessionInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) ReadSession(sessionID string, role string, offset int, limit int) ([]store.SessionMessage, error) {
	raw, err := p.client.Call("ReadSession", ReadSessionParams{
		SessionID: sessionID,
		Role:      role,
		Offset:    offset,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.SessionMessage
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Query(query string) ([]map[string]any, error) {
	raw, err := p.client.Call("Query", QueryParams{Query: query})
	if err != nil {
		return nil, err
	}
	var results []map[string]any
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Stats() (*store.StatsResult, error) {
	raw, err := p.client.Call("Stats", nil)
	if err != nil {
		return nil, err
	}
	var result store.StatsResult
	return &result, json.Unmarshal(raw, &result)
}

func (p *Proxy) ListRepos(filter string) ([]store.RepoInfo, error) {
	raw, err := p.client.Call("ListRepos", ListReposParams{Filter: filter})
	if err != nil {
		return nil, err
	}
	var results []store.RepoInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Status(days int, repoFilter string, maxSessions int, maxExcerpts int, truncateLen int) (*store.StatusResult, error) {
	raw, err := p.client.Call("Status", StatusParams{
		Days: days, RepoFilter: repoFilter,
		MaxSessions: maxSessions, MaxExcerpts: maxExcerpts, TruncateLen: truncateLen,
	})
	if err != nil {
		return nil, err
	}
	var result store.StatusResult
	return &result, json.Unmarshal(raw, &result)
}

func (p *Proxy) RecentActivity(days int, repoFilter string) ([]store.RecentActivityInfo, error) {
	raw, err := p.client.Call("RecentActivity", RecentActivityParams{Days: days, RepoFilter: repoFilter})
	if err != nil {
		return nil, err
	}
	var results []store.RecentActivityInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Usage(days int, repoFilter, model, groupBy string) (*store.UsageResult, error) {
	raw, err := p.client.Call("Usage", UsageParams{
		Days: days, RepoFilter: repoFilter, Model: model, GroupBy: groupBy,
	})
	if err != nil {
		return nil, err
	}
	var result store.UsageResult
	return &result, json.Unmarshal(raw, &result)
}

func (p *Proxy) ResolveNonce(nonce string) (string, error) {
	raw, err := p.client.Call("ResolveNonce", ResolveNonceParams{Nonce: nonce})
	if err != nil {
		return "", err
	}
	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	sid, ok := result["session_id"]
	if !ok {
		return "", fmt.Errorf("no session_id in response")
	}
	return sid, nil
}
