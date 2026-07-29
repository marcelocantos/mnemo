// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Agent-tree reconstruction and cost rollup (🎯T137).
//
// The failure this exists for is a fan-out where every individual agent
// looks reasonable and forty of them together trip the wire. Ranking
// sessions by cost — which is all 🎯T135 does — shows forty modest entries
// and nothing obviously wrong. Only an aggregate at the root makes the
// shape visible.
//
// It matters more than it first appears because there is no control side.
// 🎯T136 throttles the agents mnemo invokes itself, and a Claude Code
// sub-agent storm passes through neither mnemo nor claudia. For trees,
// reporting IS the only available response, so it has to be good enough to
// act on by hand.
//
// SCOPE IS CLAUDE-ONLY, and stated rather than discovered. The parentage
// fields come from Claude Code's record shape. Codex usage blocks are bare
// {input_tokens, output_tokens} with no message id and no parent linkage,
// and 🎯T135 quarantines that source entirely; a tree built over it would
// be noise presented as structure.

// AgentNode is one sub-agent's rolled-up spend.
type AgentNode struct {
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type,omitempty"`
	// ParentAgentID is empty when the agent was spawned from the main
	// conversation rather than by another agent.
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	// Depth is 1 for an agent spawned from the main line. A tree three
	// deep is a different problem from a wide shallow one, so it is
	// reported rather than flattened away.
	Depth int `json:"depth"`

	Records         int     `json:"records"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	StartedAt       string  `json:"started_at,omitempty"`
	EndedAt         string  `json:"ended_at,omitempty"`
}

// AgentTree is one session's fan-out, costed as a whole.
type AgentTree struct {
	SessionID string `json:"session_id"`
	Repo      string `json:"repo,omitempty"`
	Cwd       string `json:"cwd,omitempty"`

	// Skill and AgentType name what started the tree where the transcript
	// records it. "You spent a lot" is not actionable; "the release skill
	// spawned 40 agents" is.
	Skill      string   `json:"skill,omitempty"`
	AgentTypes []string `json:"agent_types,omitempty"`

	// RootTurnUUID identifies the turn that spawned the fan-out, and
	// RootTurnAt when. Together with Skill this is the root cause.
	RootTurnUUID string `json:"root_turn_uuid,omitempty"`
	RootTurnAt   string `json:"root_turn_at,omitempty"`

	Agents   int `json:"agents"`
	MaxDepth int `json:"max_depth"`

	// TreeCostUSD is the aggregate over every agent in the tree — the
	// number this whole feature exists to compute. DirectCostUSD is the
	// session's own main-line spend, reported beside it so a session whose
	// cost is mostly its own is distinguishable from one whose cost is
	// mostly its children.
	TreeCostUSD   float64 `json:"tree_cost_usd"`
	DirectCostUSD float64 `json:"direct_cost_usd"`
	TotalCostUSD  float64 `json:"total_cost_usd"`

	Records         int   `json:"records"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`

	// Live and PID distinguish a storm still running from one already
	// over. An in-progress fan-out can be stopped; a finished one can only
	// be mourned, and the report should not imply otherwise.
	Live   bool   `json:"live"`
	PID    int    `json:"pid,omitempty"`
	Action string `json:"action,omitempty"`

	// Priced is false when some of this tree's spend could not be costed.
	// A fan-out ranked at zero because its model has no rate is worse than
	// one ranked as unknown, so the flag travels with the figure.
	Priced         bool     `json:"priced"`
	UnpricedModels []string `json:"unpriced_models,omitempty"`

	Nodes []AgentNode `json:"nodes,omitempty"`
}

// AgentTreeParams filters the tree report.
type AgentTreeParams struct {
	Days       int
	Since      string
	Until      string
	RepoFilter string
	Limit      int
}

// DefaultAgentTreeLimit bounds how many trees are returned.
const DefaultAgentTreeLimit = 20

// AgentTrees reconstructs sub-agent fan-outs and ranks them by aggregate
// tree cost (🎯T137).
//
// Ranked by the tree total rather than per agent, so an expensive fan-out
// surfaces above a single expensive agent — which is the entire point.
func (s *Store) AgentTrees(p AgentTreeParams) ([]AgentTree, error) {
	since, until := resolveWindow(p.Days, p.Since, p.Until)
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultAgentTreeLimit
	}

	where := []string{
		"e.type = 'assistant'",
		"e.timestamp >= ?",
		"e.timestamp <= ?",
		// Claude only. The parentage fields exist nowhere else, and
		// 🎯T135 quarantines the other sources for want of a dedup key.
		"COALESCE(sm.source, 'claude') = 'claude'",
	}
	args := []any{since, until}
	if p.RepoFilter != "" {
		where = append(where, "(sm.repo LIKE ? OR sm.cwd LIKE ?)")
		pattern := "%" + p.RepoFilter + "%"
		args = append(args, pattern, pattern)
	}

	// Deduplicated exactly as usage accounting is (🎯T135). Sub-agent
	// records duplicate per content block like every other assistant
	// record, so a tree summed naively over-counts by the same 2-3x — and
	// this report exists to rank trees against each other, which a
	// per-tree-varying inflation would scramble.
	q := fmt.Sprintf(`
		WITH billable AS (
			SELECT
				e.session_id AS session_id,
				e.timestamp AS timestamp,
				COALESCE(e.model, '') AS model,
				COALESCE(e.is_sidechain, 0) AS sidechain,
				COALESCE(e.agent_id, '') AS agent_id,
				COALESCE(e.raw->>'$.uuid', '') AS uuid,
				COALESCE(e.raw->>'$.sourceToolAssistantUUID', '') AS src_uuid,
				COALESCE(e.raw->>'$.attributionSkill', '') AS skill,
				COALESCE(e.raw->>'$.attributionAgent', '') AS agent_type,
				MAX(COALESCE(e.input_tokens, 0))          AS input_tokens,
				MAX(COALESCE(e.output_tokens, 0))         AS output_tokens,
				MAX(COALESCE(e.cache_read_tokens, 0))     AS cache_read_tokens,
				MAX(COALESCE(e.cache_creation_tokens, 0)) AS cache_creation_tokens,
				MAX(COALESCE(%s, 0)) AS cw5m,
				MAX(COALESCE(%s, 0)) AS cw1h
			FROM entries e
			LEFT JOIN session_meta sm ON sm.session_id = e.session_id
			WHERE %s
			GROUP BY %s
		)
		SELECT session_id, timestamp, model, sidechain, agent_id, uuid,
		       src_uuid, skill, agent_type,
		       input_tokens, output_tokens, cache_read_tokens,
		       cache_creation_tokens, cw5m, cw1h
		FROM billable
		ORDER BY session_id, timestamp
	`, sqlCacheWrite5m, sqlCacheWrite1h,
		joinAnd(where), dedupGroupSQL(effectiveDedupKey()))

	rows, err := s.readDB.Query(q, args...)
	if err != nil {
		return nil, err
	}

	type rec struct {
		session, ts, model, agentID, uuid, srcUUID, skill, agentType string
		sidechain                                                    int
		in, out, cr, cc, cw5m, cw1h                                  int64
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.session, &r.ts, &r.model, &r.sidechain, &r.agentID,
			&r.uuid, &r.srcUUID, &r.skill, &r.agentType,
			&r.in, &r.out, &r.cr, &r.cc, &r.cw5m, &r.cw1h); err != nil {
			continue
		}
		recs = append(recs, r)
	}
	rows.Close()

	// Spawn links: agent id -> the session and turn that started it.
	//
	// Sub-agents come in two shapes and both must roll up, or the report
	// answers the wrong question. Inline sub-agents write into the
	// PARENT's transcript with isSidechain set. Background and worktree
	// agents get their OWN session file (agent-<id>), and grouping by the
	// session a record lives in then yields one agent per "tree" — which
	// is not a fan-out rollup at all, just a per-agent list wearing the
	// wrong name. Observed directly: a csp fan-out appeared as several
	// separate single-agent trees of $72, $43, ... rather than one tree.
	//
	// The parent records the link as toolUseResult.agentId on the
	// tool_result turn, alongside the tool_use id that spawned it — which
	// is exactly the root cause this target needs.
	links, err := s.spawnLinks(since, until)
	if err != nil {
		return nil, err
	}

	// uuid -> agent, so a sub-agent spawned BY a sub-agent resolves to its
	// real parent rather than to the main line. This is what makes nested
	// fan-outs roll up through every level instead of appearing as a wide
	// shallow tree.
	uuidAgent := map[string]string{}
	for _, r := range recs {
		if r.sidechain == 1 && r.uuid != "" && r.agentID != "" {
			uuidAgent[r.uuid] = r.agentID
		}
	}

	type agentAcc struct {
		node   AgentNode
		parent string
		// hops is how many spawn links separate this agent from the root
		// session. Inline sidechain agents resolve depth through the uuid
		// map instead; a cross-session agent has no such link, so its
		// nesting is only visible in the chain it was resolved through.
		hops int
	}
	type treeAcc struct {
		tree    AgentTree
		agents  map[string]*agentAcc
		order   []string
		skills  map[string]bool
		types   map[string]bool
		unknown map[string]bool
	}
	trees := map[string]*treeAcc{}
	var order []string

	for _, r := range recs {
		// Attribute to the session that STARTED the work, following the
		// chain so a nested fan-out rolls up through every level to the
		// human turn at its root.
		rootSession, spawn, hops := links.root(r.session, r.agentID)

		t, ok := trees[rootSession]
		if !ok {
			t = &treeAcc{
				tree:    AgentTree{SessionID: rootSession, Priced: true},
				agents:  map[string]*agentAcc{},
				skills:  map[string]bool{},
				types:   map[string]bool{},
				unknown: map[string]bool{},
			}
			trees[rootSession] = t
			order = append(order, rootSession)
		}
		// A record living in its own agent session is sub-agent spend even
		// though it carries no sidechain flag there — it IS the sub-agent.
		sidechain := r.sidechain == 1 || rootSession != r.session
		if spawn.ToolUseID != "" && t.tree.RootTurnUUID == "" {
			t.tree.RootTurnUUID = spawn.ToolUseID
			t.tree.RootTurnAt = spawn.At
		}

		cost, priced := priceBucket(
			RateCardAsOf(mustParse(r.ts)), r.model,
			r.in+r.cr+r.cc > LongContextThreshold,
			TokenCounts{
				Input: r.in, Output: r.out, CacheRead: r.cr,
				CacheWrite5m: r.cw5m, CacheWrite1h: r.cw1h,
				CacheWriteFlat: r.cc - r.cw5m - r.cw1h,
			})
		if !priced {
			t.tree.Priced = false
			t.unknown[r.model] = true
		}

		t.tree.Records++
		t.tree.OutputTokens += r.out
		t.tree.CacheReadTokens += r.cr
		t.tree.TotalCostUSD += cost

		if !sidechain {
			t.tree.DirectCostUSD += cost
			continue
		}

		t.tree.TreeCostUSD += cost
		if r.skill != "" {
			t.skills[r.skill] = true
		}
		if r.agentType != "" {
			t.types[r.agentType] = true
		}

		id := r.agentID
		if id == "" && rootSession != r.session {
			// An agent session whose records omit agentId is still one
			// agent; its session id names it.
			id = r.session
		}
		if id == "" {
			// An unattributed sidechain record still belongs to the tree's
			// cost; bucketing it under a sentinel keeps it visible rather
			// than dropping it into the main line, where it would look
			// like the user's own spend.
			id = "(unattributed)"
		}
		a, ok := t.agents[id]
		if !ok {
			a = &agentAcc{node: AgentNode{
				AgentID: id, AgentType: r.agentType, StartedAt: r.ts,
			}, hops: hops}
			t.agents[id] = a
			t.order = append(t.order, id)
		}
		if a.node.AgentType == "" {
			a.node.AgentType = r.agentType
		}
		if a.parent == "" && r.srcUUID != "" {
			a.parent = uuidAgent[r.srcUUID] // empty → spawned from the main line
			// The first spawn point seen is the root turn: the turn that
			// started the fan-out.
			if t.tree.RootTurnUUID == "" && a.parent == "" {
				t.tree.RootTurnUUID = r.srcUUID
				t.tree.RootTurnAt = r.ts
			}
		}
		a.node.Records++
		a.node.OutputTokens += r.out
		a.node.CacheReadTokens += r.cr
		a.node.CostUSD += cost
		a.node.EndedAt = r.ts
	}

	live := s.LiveSessions()
	out := make([]AgentTree, 0, len(order))
	for _, id := range order {
		t := trees[id]
		if len(t.agents) == 0 {
			continue // no fan-out; this report is about trees
		}
		// Depth, walking parent links with a cycle guard. Depth 1 means
		// spawned from the main conversation; deeper means an agent
		// spawned it, which is a materially different situation from a
		// wide shallow fan-out and so is reported rather than flattened.
		var depth func(id string, seen map[string]bool) int
		depth = func(id string, seen map[string]bool) int {
			a, ok := t.agents[id]
			if !ok || seen[id] {
				// A missing or cyclic parent stops the walk rather than
				// recursing forever. Transcripts are external input and a
				// cycle must not be able to hang a report.
				return 1
			}
			seen[id] = true
			if a.parent == "" {
				return 1
			}
			return 1 + depth(a.parent, seen)
		}
		for _, aid := range t.order {
			a := t.agents[aid]
			a.node.ParentAgentID = a.parent
			a.node.Depth = depth(aid, map[string]bool{})
			if a.hops > a.node.Depth {
				a.node.Depth = a.hops
			}
			if a.node.Depth > t.tree.MaxDepth {
				t.tree.MaxDepth = a.node.Depth
			}
			t.tree.Nodes = append(t.tree.Nodes, a.node)
		}
		sort.Slice(t.tree.Nodes, func(i, j int) bool {
			return t.tree.Nodes[i].CostUSD > t.tree.Nodes[j].CostUSD
		})
		t.tree.Agents = len(t.agents)
		for sk := range t.skills {
			if t.tree.Skill == "" {
				t.tree.Skill = sk
			}
		}
		for ty := range t.types {
			t.tree.AgentTypes = append(t.tree.AgentTypes, ty)
		}
		sort.Strings(t.tree.AgentTypes)
		for m := range t.unknown {
			t.tree.UnpricedModels = append(t.tree.UnpricedModels, m)
		}
		sort.Strings(t.tree.UnpricedModels)

		t.tree.Repo, t.tree.Cwd = s.sessionLocation(id)
		if pid, ok := live[id]; ok && pid > 0 {
			t.tree.Live, t.tree.PID = true, pid
			t.tree.Action = fmt.Sprintf(
				"STILL RUNNING (pid %d) — mnemo_session_go to attach, or kill %d to stop the fan-out",
				pid, pid)
		} else {
			t.tree.Action = "finished — spend already incurred, nothing to stop"
		}
		out = append(out, t.tree)
	}

	// Rank by aggregate tree cost. A fan-out of forty individually modest
	// agents outranks one expensive agent, which is the whole point.
	sort.Slice(out, func(i, j int) bool { return out[i].TreeCostUSD > out[j].TreeCostUSD })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// mustParse converts a stored timestamp, falling back to now so a single
// unparseable row cannot drop a tree.
func mustParse(ts string) time.Time {
	t, err := parseTimestamp(ts)
	if err != nil {
		return time.Now()
	}
	return t
}

// joinAnd renders a WHERE clause.
func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

// resolveWindow turns a days/since/until triple into RFC3339 bounds.
func resolveWindow(days int, since, until string) (string, string) {
	if since != "" {
		if until == "" {
			until = time.Now().UTC().Format(time.RFC3339)
		}
		return since, until
	}
	if days <= 0 {
		days = 7
	}
	now := time.Now().UTC()
	return now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339),
		now.Format(time.RFC3339)
}

// spawnLink records where a sub-agent came from.
type spawnLink struct {
	ParentSession string
	ToolUseID     string
	At            string
}

// spawnIndex resolves an agent to the session that started it.
type spawnIndex struct {
	byAgent map[string]spawnLink
}

// root follows the spawn chain to the session a fan-out started in.
//
// Transitive so a sub-agent that spawns sub-agents attributes to the
// human turn at the root rather than to its immediate parent — the whole
// point of rolling up. The visited set guards against a cycle, because
// transcripts are external input and a cycle must not hang a report.
func (ix spawnIndex) root(session, agentID string) (string, spawnLink, int) {
	var first spawnLink
	seen := map[string]bool{}
	cur := agentID
	if cur == "" {
		cur = strings.TrimPrefix(session, "agent-")
	}
	out := session
	hops := 0
	for i := 0; i < 32; i++ {
		l, ok := ix.byAgent[cur]
		if !ok || seen[cur] {
			break
		}
		seen[cur] = true
		hops++
		if first.ParentSession == "" {
			first = l
		}
		out = l.ParentSession
		cur = strings.TrimPrefix(l.ParentSession, "agent-")
	}
	return out, first, hops
}

// spawnLinks builds the agent -> parent index for a window.
//
// Read from the parent's tool_result turn, which carries
// toolUseResult.agentId next to the tool_use id that spawned it. The
// window is widened backwards because a fan-out started before the window
// can still be spending inside it, and a tree missing its root would be
// reported as an orphan.
func (s *Store) spawnLinks(since, until string) (spawnIndex, error) {
	ix := spawnIndex{byAgent: map[string]spawnLink{}}
	from := since
	if t, err := parseTimestamp(since); err == nil {
		from = t.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	rows, err := s.readDB.Query(`
		SELECT COALESCE(e.raw->>'$.toolUseResult.agentId', ''),
		       e.session_id,
		       COALESCE(e.raw->>'$.message.content[0].tool_use_id', ''),
		       e.timestamp
		FROM entries e
		WHERE e.timestamp >= ? AND e.timestamp <= ?
		  AND e.raw->>'$.toolUseResult.agentId' IS NOT NULL`, from, until)
	if err != nil {
		return ix, err
	}
	defer rows.Close()
	for rows.Next() {
		var agent, session, toolUse, ts string
		if rows.Scan(&agent, &session, &toolUse, &ts) != nil || agent == "" {
			continue
		}
		if _, exists := ix.byAgent[agent]; !exists {
			ix.byAgent[agent] = spawnLink{ParentSession: session, ToolUseID: toolUse, At: ts}
		}
	}
	return ix, nil
}
