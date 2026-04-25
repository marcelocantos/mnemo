// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "time"

// Backend is the interface for querying the transcript store.
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
	SearchAuditLogs(query string, repo string, skill string, limit int) ([]AuditEntryInfo, error)
	SearchTargets(query string, repo string, status string, limit int) ([]TargetInfo, error)
	SearchPlans(query string, repo string, limit int) ([]PlanInfo, error)
	SearchDocs(query string, repo string, kind string, limit int) ([]DocInfo, error)
	SearchSynthesis(query string, taxonomy string, repo string, limit int) ([]DocInfo, error)
	WhoRan(pattern string, days int, repoFilter string, limit int) ([]WhoRanResult, error)
	Permissions(days int, repoFilter string, limit int) (*PermissionsResult, error)
	SearchCI(query string, repo string, conclusion string, days int, limit int) ([]CIRun, error)
	DefineTemplate(name, description, queryText string, paramNames []string) error
	EvaluateTemplate(name string, params map[string]string) ([]map[string]any, error)
	ListTemplates() ([]QueryTemplate, error)
	LiveSessions() map[string]int
	Predecessor(sessionID string) (string, error)
	Successor(sessionID string) (string, error)
	Chain(sessionID string) ([]ChainLink, error)
	SearchDecisions(query string, repo string, days int, limit int) ([]DecisionInfo, error)
	Whatsup(postmortem bool) (*WhatsupResult, error)
	SearchGitHubActivity(query string, repo string, state string, author string, activityType string, days int, limit int) ([]GitHubActivityResult, error)
	SearchCommits(query string, repo string, author string, days int, limit int) ([]GitCommit, error)
	DiscoverPatterns(days int, repoFilter string, minOccurrences int) ([]PatternCandidate, error)
	SearchImages(query string, repo string, session string, days int, limit int) ([]ImageSearchResult, error)
	SearchImagesFiltered(query string, repo string, session string, days int, limit int, searchFields string) ([]ImageSearchResult, error)
	SearchImagesSemantic(query string, repo string, session string, days int, limit int) ([]ImageSearchResult, error)
	SearchImagesSimilar(similarTo int, repo string, session string, days int, limit int) ([]ImageSearchResult, error)
	ChainCompactions(sessionID string) ([]Compaction, error)
	CompactionsForConnection(connectionID string) ([]Compaction, error)
	SessionTokens(sessionID string) (int64, int64, error)
	CompactionTokens(sessionID string) (int64, int64, error)
	RecordConnectionOpen(connectionID string, pid int, acceptedAt time.Time)
	RecordConnectionSession(connectionID, sessionID string)
	ConnectionsForSession(sessionID string) ([]ConnectionSession, error)
	InferChainHeuristic(sessionID string, limit int) ([]ChainCandidate, error)
}
