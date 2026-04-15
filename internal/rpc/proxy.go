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

func (p *Proxy) SearchMemories(query string, memType string, project string, limit int) ([]store.MemoryInfo, error) {
	raw, err := p.client.Call("SearchMemories", SearchMemoriesParams{
		Query: query, MemoryType: memType, Project: project, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.MemoryInfo
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

func (p *Proxy) SearchSkills(query string, limit int) ([]store.SkillInfo, error) {
	raw, err := p.client.Call("SearchSkills", SearchSkillsParams{Query: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	var results []store.SkillInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchClaudeConfigs(query string, repo string, limit int) ([]store.ClaudeConfigInfo, error) {
	raw, err := p.client.Call("SearchClaudeConfigs", SearchClaudeConfigsParams{
		Query: query, Repo: repo, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.ClaudeConfigInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchAuditLogs(query string, repo string, skill string, limit int) ([]store.AuditEntryInfo, error) {
	raw, err := p.client.Call("SearchAuditLogs", SearchAuditLogsParams{
		Query: query, Repo: repo, Skill: skill, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.AuditEntryInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchTargets(query string, repo string, status string, limit int) ([]store.TargetInfo, error) {
	raw, err := p.client.Call("SearchTargets", SearchTargetsParams{
		Query: query, Repo: repo, Status: status, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.TargetInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchPlans(query string, repo string, limit int) ([]store.PlanInfo, error) {
	raw, err := p.client.Call("SearchPlans", SearchPlansParams{
		Query: query, Repo: repo, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.PlanInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) WhoRan(pattern string, days int, repoFilter string, limit int) ([]store.WhoRanResult, error) {
	raw, err := p.client.Call("WhoRan", WhoRanParams{Pattern: pattern, Days: days, RepoFilter: repoFilter, Limit: limit})
	if err != nil {
		return nil, err
	}
	var results []store.WhoRanResult
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Permissions(days int, repoFilter string, limit int) (*store.PermissionsResult, error) {
	raw, err := p.client.Call("Permissions", PermissionsParams{Days: days, RepoFilter: repoFilter, Limit: limit})
	if err != nil {
		return nil, err
	}
	var result store.PermissionsResult
	return &result, json.Unmarshal(raw, &result)
}

func (p *Proxy) SearchCI(query string, repo string, conclusion string, days int, limit int) ([]store.CIRun, error) {
	raw, err := p.client.Call("SearchCI", SearchCIParams{Query: query, Repo: repo, Conclusion: conclusion, Days: days, Limit: limit})
	if err != nil {
		return nil, err
	}
	var results []store.CIRun
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) LiveSessions() map[string]int {
	raw, err := p.client.Call("LiveSessions", nil)
	if err != nil {
		return nil
	}
	var result map[string]int
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return result
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

func (p *Proxy) Chain(sessionID string) ([]store.ChainLink, error) {
	raw, err := p.client.Call("Chain", ChainParams{SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	var results []store.ChainLink
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchDecisions(query string, repo string, days int, limit int) ([]store.DecisionInfo, error) {
	raw, err := p.client.Call("SearchDecisions", SearchDecisionsParams{
		Query: query, Repo: repo, Days: days, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.DecisionInfo
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Whatsup(postmortem bool) (*store.WhatsupResult, error) {
	raw, err := p.client.Call("Whatsup", struct{ Postmortem bool }{postmortem})
	if err != nil {
		return nil, err
	}
	var result store.WhatsupResult
	return &result, json.Unmarshal(raw, &result)
}

func (p *Proxy) DefineTemplate(name, description, queryText string, paramNames []string) error {
	_, err := p.client.Call("DefineTemplate", DefineTemplateParams{
		Name:        name,
		Description: description,
		QueryText:   queryText,
		ParamNames:  paramNames,
	})
	return err
}

func (p *Proxy) EvaluateTemplate(name string, params map[string]string) ([]map[string]any, error) {
	raw, err := p.client.Call("EvaluateTemplate", EvaluateTemplateParams{
		Name:   name,
		Params: params,
	})
	if err != nil {
		return nil, err
	}
	var results []map[string]any
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) ListTemplates() ([]store.QueryTemplate, error) {
	raw, err := p.client.Call("ListTemplates", nil)
	if err != nil {
		return nil, err
	}
	var results []store.QueryTemplate
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchGitHubActivity(query string, repo string, state string, author string, activityType string, days int, limit int) ([]store.GitHubActivityResult, error) {
	raw, err := p.client.Call("SearchGitHubActivity", SearchGitHubActivityParams{
		Query:        query,
		Repo:         repo,
		State:        state,
		Author:       author,
		ActivityType: activityType,
		Days:         days,
		Limit:        limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.GitHubActivityResult
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchCommits(query string, repo string, author string, days int, limit int) ([]store.GitCommit, error) {
	raw, err := p.client.Call("SearchCommits", SearchCommitsParams{
		Query: query, Repo: repo, Author: author, Days: days, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.GitCommit
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) Predecessor(sessionID string) (string, error) {
	chain, err := p.Chain(sessionID)
	if err != nil {
		return "", err
	}
	for i, link := range chain {
		if link.SessionID == sessionID && i > 0 {
			return chain[i-1].SessionID, nil
		}
	}
	return "", nil
}

func (p *Proxy) Successor(sessionID string) (string, error) {
	chain, err := p.Chain(sessionID)
	if err != nil {
		return "", err
	}
	for i, link := range chain {
		if link.SessionID == sessionID && i < len(chain)-1 {
			return chain[i+1].SessionID, nil
		}
	}
	return "", nil
}

func (p *Proxy) DiscoverPatterns(days int, repoFilter string, minOccurrences int) ([]store.PatternCandidate, error) {
	raw, err := p.client.Call("DiscoverPatterns", DiscoverPatternsParams{
		Days:           days,
		RepoFilter:     repoFilter,
		MinOccurrences: minOccurrences,
	})
	if err != nil {
		return nil, err
	}
	var results []store.PatternCandidate
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchImages(query string, repo string, session string, days int, limit int) ([]store.ImageSearchResult, error) {
	return p.SearchImagesFiltered(query, repo, session, days, limit, "both")
}

func (p *Proxy) SearchImagesFiltered(query string, repo string, session string, days int, limit int, searchFields string) ([]store.ImageSearchResult, error) {
	raw, err := p.client.Call("SearchImages", SearchImagesParams{
		Query:        query,
		Repo:         repo,
		Session:      session,
		Days:         days,
		Limit:        limit,
		SearchFields: searchFields,
	})
	if err != nil {
		return nil, err
	}
	var results []store.ImageSearchResult
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchImagesSemantic(query string, repo string, session string, days int, limit int) ([]store.ImageSearchResult, error) {
	raw, err := p.client.Call("SearchImagesSemantic", SearchImagesSemanticParams{
		Query:   query,
		Repo:    repo,
		Session: session,
		Days:    days,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.ImageSearchResult
	return results, json.Unmarshal(raw, &results)
}

func (p *Proxy) SearchImagesSimilar(similarTo int, repo string, session string, days int, limit int) ([]store.ImageSearchResult, error) {
	raw, err := p.client.Call("SearchImagesSimilar", SearchImagesSimilarParams{
		SimilarTo: similarTo,
		Repo:      repo,
		Session:   session,
		Days:      days,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	var results []store.ImageSearchResult
	return results, json.Unmarshal(raw, &results)
}
