// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/store"
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
		// 🎯T140: spend/throttle after mnemo_budget was removed. Primary
		// human surfaces remain dashboard + `mnemo budget` CLI; this is
		// the agent/MCP home.
		{name: "budget", desc: "Monthly budget, projection, throttle state, and top agent trees"},
		{name: "agent_trees", desc: "Costliest agent trees (T137), ranked by aggregate tree cost", params: []string{"days", "limit", "repo"}},
		// 🎯T151: per-row zstd compression of messages.text / docs.content.
		{name: "compress_status", desc: "Compression dictionaries and per-column compressed/plain accounting, plus backfill progress"},
		{name: "compress_train", desc: "Train a new zstd dictionary for a column family from a random row sample and make it active for new rows", params: []string{"family"}},
		{name: "compress_gc", desc: "Repack historical plain rows compressed, in resumable batches (background unless wait=true); VACUUM afterwards to reclaim space", params: []string{"family", "wait"}},
	},
}

func opsTool() mcp.Tool {
	return mcp.NewTool("mnemo_ops",
		mcp.WithDescription(`Operational surface — health, compaction, divergence, backup, and budget/throttle (🎯T140). For index freshness and content questions use mnemo_status and mnemo_stats, which are deliberately NOT folded in here.

op=restore is destructive and requires an explicit session_id.
op=budget / op=agent_trees replace the removed mnemo_budget and mnemo_agent_trees tools; humans may prefer GET /api/budget or CLI "mnemo budget".

Ops:`+opsOps.describe()),
		mcp.WithString("op", mcp.Required(), mcp.Description("Operation to perform — see the list above")),
		mcp.WithBoolean("force", mcp.Description("op=backup_now: snapshot even if a recent backup exists")),
		mcp.WithString("session_id", mcp.Description("op=restore: the session to restore (required)")),
		mcp.WithNumber("days", mcp.Description("op=agent_trees: lookback days (default 7)")),
		mcp.WithNumber("limit", mcp.Description("op=agent_trees: max trees (default 20)")),
		mcp.WithString("repo", mcp.Description("op=agent_trees: repo filter")),
		mcp.WithString("family", mcp.Description("op=compress_train / compress_gc: messages (default), docs, or entries")),
		mcp.WithBoolean("wait", mcp.Description("op=compress_gc: run to completion in this call instead of in the background")),
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
	case "budget":
		return h.opsBudget()
	case "agent_trees":
		return h.opsAgentTrees(args)
	case "compress_status":
		return h.compressStatus()
	case "compress_train":
		return h.compressTrain(args)
	case "compress_gc":
		return h.compressGC(args)
	}
	return "mnemo_ops: op " + op + " has no handler", true, nil
}

// opsBudget reports spend + throttle for agents (🎯T140 MCP home after
// mnemo_budget was removed). Humans should prefer `mnemo budget` or the
// dashboard /api/budget card.
func (h *callHandler) opsBudget() (string, bool, error) {
	// Parent Handler is not on callHandler — budget uses Backend + tooling
	// config injected via process-global is wrong. Use store methods only
	// and read throttle via a package-level optional set by tools.Handler.
	return formatOpsBudget(h.mem, opsBudgetCfg, opsThrottleReport)
}

func (h *callHandler) opsAgentTrees(args map[string]any) (string, bool, error) {
	days := 7
	if v, ok := args["days"].(float64); ok && v > 0 {
		days = int(v)
	}
	limit := store.DefaultAgentTreeLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	repo, _ := args["repo"].(string)
	trees, err := h.mem.AgentTrees(store.AgentTreeParams{Days: days, Limit: limit, RepoFilter: repo})
	if err != nil {
		return fmt.Sprintf("agent_trees failed: %v", err), true, nil
	}
	if len(trees) == 0 {
		return "No agent trees in window.", false, nil
	}
	var b strings.Builder
	for _, tr := range trees {
		fmt.Fprintf(&b, "$%.2f tree  agents=%d depth=%d  skill=%s  repo=%s  live=%v\n  %s\n",
			tr.TreeCostUSD, tr.Agents, tr.MaxDepth, tr.Skill, tr.Repo, tr.Live, tr.Action)
	}
	return b.String(), false, nil
}

// Package-level wiring for ops budget (set from tools.Handler.SetBudgetWiring
// via setOpsBudgetWiring). Avoids expanding callHandler for every tool call.
var (
	opsBudgetCfg      store.BudgetConfig
	opsThrottleReport func() (level, detail, remediation string)
)

func setOpsBudgetWiring(cfg store.BudgetConfig, thr func() (string, string, string)) {
	opsBudgetCfg = cfg
	opsThrottleReport = thr
}

func formatOpsBudget(mem store.Backend, cfg store.BudgetConfig, thr func() (string, string, string)) (string, bool, error) {
	b, err := mem.BudgetStatusNow(cfg, time.Now())
	if err != nil {
		return fmt.Sprintf("budget failed: %v", err), true, nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n", b.Headline)
	fmt.Fprintf(&out, "cap=$%.2f spent=$%.2f (%.0f%%) projected=$%.2f (%.0f%%) elapsed=%.0f%% governed=$%.2f (%.0f%%) priced=%v\n",
		b.CapUSD, b.SpentUSD, b.SpentPct, b.ProjectedUSD, b.ProjectedPct, b.ElapsedPct, b.GovernedUSD, b.GovernedPct, b.Priced)
	if b.ExhaustionDate != "" {
		fmt.Fprintf(&out, "exhaustion_date=%s\n", b.ExhaustionDate)
	}
	if thr != nil {
		level, detail, rem := thr()
		fmt.Fprintf(&out, "throttle=%s\n  %s\n", level, detail)
		if rem != "" {
			fmt.Fprintf(&out, "  lifts: %s\n", rem)
		}
	} else {
		out.WriteString("throttle=unknown (not wired)\n")
	}
	trees, terr := mem.AgentTrees(store.AgentTreeParams{Days: 7, Limit: 5})
	if terr == nil && len(trees) > 0 {
		out.WriteString("top agent trees:\n")
		for _, tr := range trees {
			fmt.Fprintf(&out, "  $%.2f  n=%d  %s  %s\n", tr.TreeCostUSD, tr.Agents, tr.Skill, tr.Repo)
		}
	}
	return out.String(), false, nil
}

// --- Compression (🎯T151) ---

// textCompressor is the slice of the store the compression ops need.
// Optional: a Backend that is not the real store (federation, tests)
// simply reports the ops as unavailable.
type textCompressor interface {
	CompressionStatus() (store.CompressionStatus, error)
	EntriesMaterialised() (bool, error)
	TrainDictionary(ctx context.Context, family string) (uint32, error)
	CompressBackfill(ctx context.Context, family string) (store.BackfillResult, error)
}

func (h *callHandler) compressor() (textCompressor, bool) {
	c, ok := h.mem.(textCompressor)
	return c, ok
}

func (h *callHandler) compressStatus() (string, bool, error) {
	c, ok := h.compressor()
	if !ok {
		return "compression ops are not available on this backend", true, nil
	}
	st, err := c.CompressionStatus()
	if err != nil {
		return fmt.Sprintf("compress_status failed: %v", err), true, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Compression ready: %v\n\n", st.Ready)
	if len(st.Dicts) == 0 {
		b.WriteString("Dictionaries: none (rows are written dictionary-less; op=compress_train once a few thousand rows exist)\n")
	} else {
		b.WriteString("Dictionaries:\n")
		for _, d := range st.Dicts {
			active := ""
			if d.Active {
				active = "  (active)"
			}
			fmt.Fprintf(&b, "  %-16s id=%-10d %8s  trained %s on %d rows%s\n",
				d.Family, d.DictID, humanSize(int64(d.Bytes)), d.CreatedAt, d.SampleRows, active)
		}
	}
	b.WriteString("\nFamilies:\n")
	for _, f := range st.Families {
		fmt.Fprintf(&b, "  %-16s rows=%d compressed=%d plain=%s packed=%s",
			f.Family, f.Rows, f.Compressed, humanSize(f.PlainBytes), humanSize(f.PackedBytes))
		switch {
		case f.Running:
			fmt.Fprintf(&b, "  backfill: running (next id %d, saved %s)", f.BackfillNext, humanSize(f.BackfillSaved))
		case f.BackfillDone:
			fmt.Fprintf(&b, "  backfill: done at %s (saved %s)", f.BackfillAt, humanSize(f.BackfillSaved))
		case f.BackfillNext > 0:
			fmt.Fprintf(&b, "  backfill: paused at id %d (saved %s)", f.BackfillNext, humanSize(f.BackfillSaved))
		default:
			b.WriteString("  backfill: not started")
		}
		if f.LastError != "" {
			fmt.Fprintf(&b, "  last error: %s", f.LastError)
		}
		b.WriteString("\n")
	}
	if mat, err := h.materialisation(); err == nil && mat != "" {
		b.WriteString("\n" + mat + "\n")
	}
	b.WriteString("\nSpace is reclaimed only by VACUUM (needs free disk equal to the DB size); run it after the backfill reports done.\n")
	return b.String(), false, nil
}

func compressFamily(args map[string]any) (string, error) {
	family, _ := args["family"].(string)
	switch family {
	case "", "messages":
		return store.FamilyMessagesText, nil
	case "docs":
		return store.FamilyDocsContent, nil
	case "entries":
		return store.FamilyEntriesRaw, nil
	case store.FamilyMessagesText, store.FamilyDocsContent, store.FamilyEntriesRaw:
		return family, nil
	}
	return "", fmt.Errorf("unknown family %q (want messages, docs or entries)", family)
}

func (h *callHandler) compressTrain(args map[string]any) (string, bool, error) {
	c, ok := h.compressor()
	if !ok {
		return "compression ops are not available on this backend", true, nil
	}
	family, err := compressFamily(args)
	if err != nil {
		return err.Error(), true, nil
	}
	id, err := c.TrainDictionary(h.ctx, family)
	if err != nil {
		return fmt.Sprintf("compress_train failed: %v", err), true, nil
	}
	return fmt.Sprintf("Trained dictionary %d for %s; new rows use it from now. Existing rows keep their dictionary; run op=compress_gc to repack history.", id, family), false, nil
}

// compressGC starts the backfill in the background — over millions of
// rows it runs for minutes, well past any MCP call budget — and returns
// at once; op=compress_status reports progress and the resumable cursor.
func (h *callHandler) compressGC(args map[string]any) (string, bool, error) {
	c, ok := h.compressor()
	if !ok {
		return "compression ops are not available on this backend", true, nil
	}
	family, err := compressFamily(args)
	if err != nil {
		return err.Error(), true, nil
	}
	if wait, _ := args["wait"].(bool); wait {
		res, err := c.CompressBackfill(h.ctx, family)
		if err != nil {
			return fmt.Sprintf("compress_gc failed after %d rows: %v", res.Rows, err), true, nil
		}
		return fmt.Sprintf("Backfill %s: visited %d rows, compressed %d, saved %s, done=%v",
			family, res.Rows, res.Compressed, humanSize(res.Saved), res.Done), false, nil
	}
	go func() {
		res, err := c.CompressBackfill(context.Background(), family)
		if err != nil {
			slog.Warn("compress backfill stopped", "family", family, "rows", res.Rows, "err", err)
			return
		}
		slog.Info("compress backfill finished", "family", family,
			"rows", res.Rows, "compressed", res.Compressed, "saved", res.Saved, "done", res.Done)
	}()
	return fmt.Sprintf("Backfill of %s started in the background; poll op=compress_status. It is resumable: a restart continues from the last committed batch.", family), false, nil
}

// materialisation reports the 🎯T152 boot-time entries field pass, which
// gates entries.raw compression.
func (h *callHandler) materialisation() (string, error) {
	c, ok := h.compressor()
	if !ok {
		return "", nil
	}
	done, err := c.EntriesMaterialised()
	if err != nil {
		return "", err
	}
	if done {
		return "entries.fields: materialised (entries.raw may be compressed)", nil
	}
	return "entries.fields: boot-time materialisation still running; op=compress_gc family=entries is refused until it finishes", nil
}
