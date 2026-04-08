// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Backend is the interface for querying the transcript store.
// Both the direct Store and the RPC Proxy implement this.
type Backend interface {
	Search(query string, limit int, sessionType, repoFilter string, contextBefore, contextAfter int, substantiveOnly bool) ([]SearchResult, error)
	ListSessions(sessionType string, minMessages int, limit int, projectFilter, repoFilter, workTypeFilter string) ([]SessionInfo, error)
	ReadSession(sessionID string, role string, offset int, limit int) ([]SessionMessage, error)
	Query(query string) ([]map[string]any, error)
	Stats() (*StatsResult, error)
	ListRepos(filter string) ([]RepoInfo, error)
	ResolveNonce(nonce string) (string, error)
	RecentActivity(days int, repoFilter string) ([]RecentActivityInfo, error)
	Status(days int, repoFilter string, maxSessions int, maxExcerpts int, truncateLen int) (*StatusResult, error)
	Usage(days int, repoFilter, model, groupBy string) (*UsageResult, error)
	SearchMemories(query string, memType string, project string, limit int) ([]MemoryInfo, error)
	SearchSkills(query string, limit int) ([]SkillInfo, error)
	SearchClaudeConfigs(query string, repo string, limit int) ([]ClaudeConfigInfo, error)
}
