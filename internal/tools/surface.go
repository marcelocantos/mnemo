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
	"mnemo_search":          consumerAgent, // 1136 / 228
	"mnemo_query":           consumerAgent, // 209 / 26
	"mnemo_recent_activity": consumerAgent, // 154 / 136, skill-driven
	"mnemo_read_session":    consumerAgent, // 131 / 58
	"mnemo_sessions":        consumerAgent, // 99 / 63
	// Below ten agent calls each, so each carries its reason (🎯T156).
	// 7 / 5. The token-volume retrieval form (🎯T72): a converged session
	// is mostly summary plus a bounded tail, which is what makes reading
	// a long session affordable at all.
	"mnemo_compacted_session": consumerAgent,
	// 6 / 5. Answers "what shape is this session" without reading it —
	// the cheap probe before an expensive read.
	"mnemo_session_structure": consumerAgent,
	// 1 / 1, and 1506 bytes per call — the worst ratio on the surface.
	// Retained because it is the only way to resolve a bare UUID from a
	// stack trace or a log line back to its session, which is a recovery
	// path, not a browsing one. Reconsider if it stays unused.
	"mnemo_locate_uuid": consumerAgent,

	// Cross-repo indexes.
	"mnemo_repos": consumerAgent, // 14 / 12

	// Status and diagnostics kept OUT of mnemo_ops on purpose: these
	// carry traffic, and an op is a worse name than a name.
	"mnemo_status": consumerAgent, // 17 / 14
	"mnemo_stats":  consumerAgent, // 16 / 14

	"mnemo_usage": consumerAgent, // 13 / 5

	// Consolidated entry points (🎯T143.3/.4/.5). All three read cold on
	// agent calls, and all three have a consumer that is not an agent —
	// which is exactly why the audit counts the kinds separately.
	"mnemo_vault":  consumerUser,  // maintenance workflow; 10 tools folded
	"mnemo_thread": consumerApp,   // menubar app via /api/thread/*
	"mnemo_ops":    consumerAgent, // 0 direct calls but skill-driven; 37 across the six folded tools

	// mnemo_note was removed on 2026-08-30 (🎯T156). This ledger had it
	// "on notice": its 63 calls came from two sessions running `/loop
	// /inbox`, and the skills that drove them were deleted 2026-08-07,
	// so nothing had called it since 2026-07-19. It cost 522 tokens of
	// every session's context. The owner made the product call the
	// ledger said was needed; the notes table and store API remain.

	// 1 / 1, at 1218 bytes per call. Not agent-driven: bullseye_rework
	// consumes its output as the mnemo_history parameter, so the calls
	// are as rare as reworks are and the consumer is a skill.
	"mnemo_rework_history": consumerSkill,
}
