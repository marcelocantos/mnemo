// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"github.com/mark3labs/mcp-go/mcp"
)

// --- Threads (🎯T143.4) ---

// threadOps consolidates the five mnemo_thread_* tools.
//
// ZERO was a live option here and was rejected on evidence. All five
// have complete HTTP twins (/api/thread/list, /show, /new, /archive,
// /go) serving the macOS menubar app, and no agent has ever called one
// — which argues for dropping them from MCP entirely. Against that:
// `go` and `new` are agent-shaped actions in exactly the way
// mnemo_session_go is, and that tool does get used; and 🎯T86 is an
// open target about surfacing more thread signal, which could produce
// an agent workflow. Removing the capability to save one slot would
// have to be undone the moment it did. One entry point keeps the
// affordance at a cost of one slot instead of five.
var threadOps = opTable{
	tool: "mnemo_thread",
	ops: []opSpec{
		{name: "list", desc: "List threads with state, activity and markers"},
		{name: "show", desc: "Show one thread's detail and preview", params: []string{"name"}},
		{name: "new", desc: "Create a thread", params: []string{"name"}},
		{name: "archive", desc: "Archive a thread", params: []string{"name"}},
		{
			name:   "go",
			desc:   "Open the thread in a terminal tab in its directory, reusing the tab already tagged for it",
			params: []string{"name", "no_resume"},
		},
	},
}

func threadTool() mcp.Tool {
	return mcp.NewTool("mnemo_thread",
		mcp.WithDescription(`Thread navigation — the working contexts the menubar popup shows. Same data as the daemon's /api/thread/* endpoints.

Ops:`+threadOps.describe()),
		mcp.WithString("op", mcp.Required(), mcp.Description("Operation to perform — see the list above")),
		mcp.WithString("name", mcp.Description("Thread name (ops show, new, archive, go)")),
		mcp.WithBoolean("no_resume", mcp.Description("op=go: open the directory without resuming the conversation")),
	)
}

func (h *callHandler) threadDispatch(args map[string]any) (string, bool, error) {
	op, err := threadOps.resolve(args)
	if err != nil {
		return err.Error(), true, nil
	}
	switch op {
	case "list":
		return h.threadList(args)
	case "show":
		return h.threadShow(args)
	case "new":
		return h.threadNew(args)
	case "archive":
		return h.threadArchive(args)
	case "go":
		return h.threadGo(args)
	}
	return "mnemo_thread: op " + op + " has no handler", true, nil
}

// --- Notes (🎯T143.4) ---

// noteOps consolidates the three mnemo_note_* tools.
//
// Unlike threads these are USED — 63 calls — so capability
// preservation here is load-bearing rather than theoretical. Read the
// usage correctly though: 55 of those calls come from two sessions,
// which is /loop polling an inbox, not broad adoption. Three tools for
// one primitive is still the wrong shape.
//
// The delivery semantics that must survive intact are recv's: the
// unread_only and mark_read defaults, and the idempotency that stops
// concurrent receivers double-delivering (🎯T65).
var noteOps = opTable{
	tool: "mnemo_note",
	ops: []opSpec{
		{
			name:   "post",
			desc:   "Post a note to a directory-addressed inbox for another session to pick up",
			params: []string{"inbox", "body", "from_session", "from_repo"},
		},
		{
			name:   "recv",
			desc:   "Receive notes addressed to a directory. Defaults unread_only=true, mark_read=true; idempotent, so concurrent receivers never double-deliver",
			params: []string{"inbox", "unread_only", "mark_read", "limit"},
		},
		{
			name:   "list",
			desc:   "Browse notes without consuming them. Omit inbox to list every inbox touched in the window. Never marks notes read",
			params: []string{"inbox", "days"},
		},
	},
}

func noteTool() mcp.Tool {
	return mcp.NewTool("mnemo_note",
		mcp.WithDescription(`Cross-session inbox notes (🎯T65) — the directory-addressed primitive for handing work between sessions.

An inbox is a directory path: absolute, or relative to the calling session's initial cwd. A leading ~ is rejected, ./.. are collapsed, symlinks resolved, and the directory must exist — so every spelling of one directory addresses one inbox.

Ops:`+noteOps.describe()),
		mcp.WithString("op", mcp.Required(), mcp.Description("Operation to perform — see the list above")),
		mcp.WithString("inbox", mcp.Description("Inbox directory. Required for post and recv; optional for list")),
		mcp.WithString("body", mcp.Description("op=post: the note body (required)")),
		mcp.WithString("from_session", mcp.Description("op=post: overrides the sender session from connection identity")),
		mcp.WithString("from_repo", mcp.Description("op=post: overrides the sender repo from connection identity")),
		mcp.WithBoolean("unread_only", mcp.Description("op=recv: only undelivered notes (default true)")),
		mcp.WithBoolean("mark_read", mcp.Description("op=recv: mark delivered (default true)")),
		mcp.WithNumber("limit", mcp.Description("op=recv: max notes to return")),
		mcp.WithNumber("days", mcp.Description("op=list: window in days (default 30)")),
	)
}

func (h *callHandler) noteDispatch(args map[string]any) (string, bool, error) {
	op, err := noteOps.resolve(args)
	if err != nil {
		return err.Error(), true, nil
	}
	switch op {
	case "post":
		return h.notePost(args)
	case "recv":
		return h.noteRecv(args)
	case "list":
		return h.noteList(args)
	}
	return "mnemo_note: op " + op + " has no handler", true, nil
}

// --- Operations / diagnostics (🎯T143.5) ---

// opsOps consolidates the six operational tools that answer "is mnemo
// healthy and what is it doing".
//
// NOT included, deliberately: mnemo_status and mnemo_stats. They carry
// the traffic (17 and 16 calls across 14 sessions each) and
// mnemo_status is the documented first-line "is the index stale for
// this repo" check. Folding them would take the one diagnostic agents
// actually reach for and hide it behind an op — precisely what the
// convention in opdispatch.go says not to do.
//
// restore is the one destructive op in the group. Consolidation
// generally shortens the path to an operation, which for a destructive
// one is a downside, so it keeps its required session_id and its
// existing guard rails rather than becoming easier to invoke.
var opsOps = opTable{
	tool: "mnemo_ops",
	ops: []opSpec{
		{name: "doctor", desc: "Run self-diagnostics: per-check name, severity, tier, detail and remediation. Same report as GET /health"},
		{name: "compactor", desc: "Compactor status: queue, circuit-breaker state, recent runs"},
		{name: "divergence", desc: "Convergence data-plane divergence: per-stream gap between desired and actual"},
		{name: "backup_status", desc: "Report the latest backup snapshot and its age"},
		{name: "backup_now", desc: "Take a backup snapshot now", params: []string{"force"}},
		{
			name:   "restore",
			desc:   "DESTRUCTIVE — restore a session's context. Requires session_id",
			params: []string{"session_id"},
		},
	},
}

func opsTool() mcp.Tool {
	return mcp.NewTool("mnemo_ops",
		mcp.WithDescription(`Operational surface — health, compaction, divergence and backup. For index freshness and content questions use mnemo_status and mnemo_stats, which are deliberately NOT folded in here.

op=restore is destructive and requires an explicit session_id.

Ops:`+opsOps.describe()),
		mcp.WithString("op", mcp.Required(), mcp.Description("Operation to perform — see the list above")),
		mcp.WithBoolean("force", mcp.Description("op=backup_now: snapshot even if a recent backup exists")),
		mcp.WithString("session_id", mcp.Description("op=restore: the session to restore (required)")),
	)
}

func (h *callHandler) opsDispatch(args map[string]any, resolveCompactor func(string) CompactorHealthReporter, diag DiagRunner) (string, bool, error) {
	op, err := opsOps.resolve(args)
	if err != nil {
		return err.Error(), true, nil
	}
	switch op {
	case "doctor":
		return h.doctor(diag)
	case "compactor":
		return h.compactorStatus(resolveCompactor)
	case "divergence":
		return h.divergence()
	case "backup_status":
		return h.backupStatus()
	case "backup_now":
		return h.backupNow(args)
	case "restore":
		return h.restore(args)
	}
	return "mnemo_ops: op " + op + " has no handler", true, nil
}
