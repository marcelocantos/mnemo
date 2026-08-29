//go:build audit

// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"database/sql"
	"fmt"
	"github.com/marcelocantos/mnemo/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Tool-surface audit (🎯T143.6).
//
// The repeatable form of the 2026-08-07 audit that produced 🎯T143.
// Build-tagged because it reads the live index and the user's skills
// directory, neither of which belongs in the normal suite:
//
//	go test -tags "sqlite_fts5 audit" -run TestToolSurfaceAudit -v ./internal/tools/
//
// It counts the three consumer kinds SEPARATELY, which is the whole
// point. The audit's central finding was that "never called by an
// agent" and "dead" are different claims — the thread tools have an
// app, the vault tools have user workflows. Collapsing the three into
// one "used?" boolean is what makes a tool with an HTTP twin look dead,
// and would have led to deleting a working feature.
//
// This reports; it does not fail. A cold tool is a question for a human
// ("does anyone want this?"), not a build error. The ratchet in
// surface_test.go is the enforcing half.
func TestToolSurfaceAudit(t *testing.T) {
	registered := make([]string, 0, len(toolConsumers))
	for _, tool := range Definitions() {
		registered = append(registered, tool.Name)
	}
	sort.Strings(registered)

	agent := agentCallCounts(t)
	skills := skillReferences(t, registered)
	http := httpTwins(t)

	var cold []string
	t.Logf("%-26s %8s %8s %7s %6s", "TOOL", "CALLS", "SESSIONS", "SKILLS", "HTTP")
	for _, name := range registered {
		c := agent[name]
		nSkill := len(skills[name])
		hasHTTP := http[strings.TrimPrefix(name, "mnemo_")]
		t.Logf("%-26s %8d %8d %7d %6v", name, c.calls, c.sessions, nSkill, hasHTTP)
		if c.calls == 0 && nSkill == 0 && !hasHTTP {
			cold = append(cold, name)
		}
	}

	if len(cold) == 0 {
		t.Log("\nNo tool is cold on all three measures.")
		return
	}
	t.Logf("\n%d tool(s) with no agent call, no skill reference and no HTTP twin:\n  %s\n"+
		"That is a question, not a verdict — check for a documented user "+
		"workflow before concluding anything. Consumer kinds are declared in "+
		"surface.go; a tool marked consumerUser is expected to show cold here.",
		len(cold), strings.Join(cold, "\n  "))
}

type callCount struct{ calls, sessions int }

// agentCallCounts reads the live index for mcp__mnemo__* invocations.
func agentCallCounts(t *testing.T) map[string]callCount {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve home: %v", err)
	}
	path := filepath.Join(home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no live index at %s", path)
	}
	db, err := sql.Open(store.SQLiteDriverName, "file:"+path+"?mode=ro")
	if err != nil {
		t.Skipf("open index: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT tool_name, COUNT(*), COUNT(DISTINCT session_id)
		FROM messages
		WHERE tool_name LIKE 'mcp__mnemo__%'
		GROUP BY tool_name`)
	if err != nil {
		t.Skipf("query index: %v", err)
	}
	defer rows.Close()

	out := map[string]callCount{}
	for rows.Next() {
		var name string
		var c callCount
		if err := rows.Scan(&name, &c.calls, &c.sessions); err != nil {
			continue
		}
		out[strings.TrimPrefix(name, "mcp__mnemo__")] = c
	}
	return out
}

// skillReferences greps ~/.claude/skills for each tool name. A skill
// that invokes a tool is a consumer even with zero agent calls on
// record — it just means the skill has not run lately.
func skillReferences(t *testing.T, names []string) map[string][]string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".claude", "skills")
	if _, err := os.Stat(dir); err != nil {
		t.Logf("no skills directory at %s — skill column will read zero", dir)
		return nil
	}
	out := map[string][]string{}
	for _, n := range names {
		cmd := exec.Command("grep", "-rl", "--", n, dir)
		b, err := cmd.Output()
		if err != nil {
			continue // grep exits 1 when nothing matches
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				out[n] = append(out[n], filepath.Base(filepath.Dir(line)))
			}
		}
	}
	return out
}

// httpTwins reports which subsystems have a non-MCP caller, keyed by
// the tool-name suffix. Sourced from the route table rather than
// hand-maintained, so a new endpoint counts automatically.
func httpTwins(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "api", "api.go"))
	if err != nil {
		t.Logf("cannot read api.go: %v — HTTP column will read false", err)
		return nil
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.Index(line, `"/api/`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`"/api/`):]
		j := strings.IndexAny(rest, `/"`)
		if j <= 0 {
			continue
		}
		out[rest[:j]] = true
	}
	// The thread routes live outside api.go; count them explicitly so
	// the one group whose entire consumer is an app is not misreported.
	if _, err := os.Stat(filepath.Join("..", "threads")); err == nil {
		out["thread"] = true
	}
	fmt.Fprintln(os.Stderr) // keep -v output readable
	return out
}
