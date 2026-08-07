// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

// Tool-surface ledger (🎯T143.6).
//
// The surface reached 70 tools without anyone deciding it should.
// Every individual addition was defensible — a subsystem shipped, it
// got a tool — and the aggregate was never reviewed until the
// 2026-08-07 audit went looking and found 30 tools that no agent had
// ever called. The failure was not carelessness; it was that no point
// existed at which the total was visible.
//
// This file is that point. Adding a tool means adding a line here,
// which means the reviewer sees the surface grow. Removing one means
// deleting a line. The test in surface_test.go fails on any drift
// between this ledger and what Tools() actually registers, in BOTH
// directions — a silent removal is also worth seeing, since it means a
// capability left without anyone noting it.

// consumerKind records who actually calls a tool. The audit's central
// finding was that "never called by an agent" and "dead" are different
// claims: the thread tools have an app, the vault tools have user
// workflows. A check that cannot tell those apart would either flag
// healthy tools or miss dead ones.
type consumerKind string

const (
	// consumerAgent: called by agents through MCP, with usage on record.
	consumerAgent consumerKind = "agent"
	// consumerSkill: invoked by a skill in ~/.claude/skills.
	consumerSkill consumerKind = "skill"
	// consumerApp: has a non-MCP caller — HTTP endpoint, menubar app, CLI.
	consumerApp consumerKind = "app"
	// consumerUser: a documented human workflow (maintenance, recovery)
	// rather than anything automated.
	consumerUser consumerKind = "user"
)

// toolConsumers is the committed ledger: every registered tool and who
// consumes it. Counts in the comments are from the 2026-08-07 audit
// (agent calls / distinct sessions, whole index since 2026-04-06).
var toolConsumers = map[string]consumerKind{
	// Retrieval — the load-bearing surface. mnemo_search alone was 55%
	// of all agent calls; these are why the product exists.
	"mnemo_search":            consumerAgent, // 1136 / 228
	"mnemo_query":             consumerAgent, // 209 / 26
	"mnemo_recent_activity":   consumerAgent, // 154 / 136, skill-driven
	"mnemo_read_session":      consumerAgent, // 131 / 58
	"mnemo_sessions":          consumerAgent, // 99 / 63
	"mnemo_decisions":         consumerAgent, // 83 / 77 (one July fan-out)
	"mnemo_compacted_session": consumerAgent, // 6 / 4
	"mnemo_segments":          consumerAgent, // 8 / 2
	"mnemo_chain":             consumerAgent, // 3 / 3
	"mnemo_session_structure": consumerAgent, // 6 / 5
	"mnemo_locate_uuid":       consumerAgent, // 1 / 1

	// Cross-repo indexes.
	"mnemo_repos": consumerAgent, // 14 / 12

	// Status and diagnostics kept OUT of mnemo_ops on purpose: these
	// carry traffic, and an op is a worse name than a name.
	"mnemo_status": consumerAgent, // 17 / 14
	"mnemo_stats":  consumerAgent, // 16 / 14

	// Cost. Young — filed 2026-07-30, so absence of calls is absence of
	// opportunity, not absence of demand. 🎯T140 is building their
	// surfaces.
	"mnemo_usage":       consumerAgent, // 13 / 5
	"mnemo_budget":      consumerUser,  // new with 🎯T135
	"mnemo_agent_trees": consumerUser,  // new with 🎯T137

	// Consolidated entry points (🎯T143.3/.4/.5).
	"mnemo_vault":  consumerUser,  // maintenance; 10 tools folded
	"mnemo_thread": consumerApp,   // menubar app via /api/thread/*
	"mnemo_ops":    consumerAgent, // 37 across the six folded tools

	// mnemo_note is on notice, and the ledger should say so rather than
	// carry a stale justification.
	//
	// Its 63 calls looked like agent adoption. They were not: 55 came
	// from two sessions running `/loop /inbox`, and the /inbox and /post
	// skills that drove them were deleted 2026-08-07 as not having
	// proven useful. Nothing has called any note op since 2026-07-19.
	// So the consumer that justified consumerAgent no longer exists.
	//
	// Kept for now because 🎯T65 built it deliberately as a primitive
	// and removing it is a product call, not a cleanup. Marked
	// consumerUser so the audit reports it cold honestly instead of
	// citing usage that a deleted skill generated.
	"mnemo_note": consumerUser,

	// Session control and introspection.
	"mnemo_session_go":        consumerAgent, // new with 🎯T125
	"mnemo_config":            consumerAgent, // 12 / 4
	"mnemo_whatsup":           consumerAgent, // 2 / 2
	"mnemo_permissions":       consumerAgent, // 1 / 1
	"mnemo_discover_patterns": consumerAgent, // 1 / 1
	"mnemo_rework_history":    consumerSkill, // bullseye_rework feeds on it
}
