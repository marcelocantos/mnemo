// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"path/filepath"
	"strings"
)

// grokSidecarJSONL are Grok files under the session tree that mnemo must not
// ingest (and should not enqueue from watches).
var grokSidecarJSONL = map[string]bool{
	"chat_history.jsonl":    true,
	"events.jsonl":          true,
	"rewind_points.jsonl":   true,
	"prompt_history.jsonl":  true,
	"hunk_records.jsonl":    true,
	"feedback.jsonl":        true,
}

// Interest reports whether path should be delivered to realtime ingest for mode.
func Interest(path string, mode WatchMode) bool {
	if path == "" {
		return false
	}
	// Normalize separators for cross-platform tests.
	clean := filepath.Clean(path)
	base := filepath.Base(clean)

	if hasPathComponent(clean, "terminal") {
		return false
	}

	switch mode {
	case ModeVault:
		if base == "" || strings.HasPrefix(base, ".") {
			return false
		}
		if hasHiddenComponent(clean) {
			return false
		}
		return strings.HasSuffix(strings.ToLower(base), ".md")
	default: // ModeTranscript
		if grokSidecarJSONL[base] {
			return false
		}
		if strings.HasSuffix(base, ".jsonl") {
			// Claude session jsonl, Codex rollout-*.jsonl, Grok updates.jsonl, etc.
			return true
		}
		if !strings.HasSuffix(strings.ToLower(base), ".md") {
			return false
		}
		// Memory files live under .../memory/*.md
		if hasPathComponent(clean, "memory") {
			return true
		}
		// Skills: ~/.claude/skills/*.md
		if strings.Contains(clean, string(filepath.Separator)+".claude"+string(filepath.Separator)+"skills"+string(filepath.Separator)) ||
			strings.Contains(clean, "/.claude/skills/") ||
			strings.Contains(clean, `\.claude\skills\`) {
			return true
		}
		switch base {
		case "CLAUDE.md", "audit-log.md", "targets.md", "TODO.md", "todo.md", "todos.md", "Todos.md":
			return true
		}
		// .planning/**/*.md
		if strings.Contains(clean, string(filepath.Separator)+".planning"+string(filepath.Separator)) ||
			strings.Contains(clean, "/.planning/") {
			return true
		}
		return false
	}
}

func hasPathComponent(path, name string) bool {
	for _, p := range strings.Split(path, string(filepath.Separator)) {
		if p == name {
			return true
		}
	}
	// Also accept slash-normalized form from FSEvents.
	for _, p := range strings.Split(path, "/") {
		if p == name {
			return true
		}
	}
	return false
}

func hasHiddenComponent(path string) bool {
	for _, p := range strings.Split(path, string(filepath.Separator)) {
		if p == "" || p == "." || p == ".." {
			continue
		}
		if strings.HasPrefix(p, ".") {
			return true
		}
	}
	for _, p := range strings.Split(path, "/") {
		if p == "" || p == "." || p == ".." {
			continue
		}
		if strings.HasPrefix(p, ".") {
			return true
		}
	}
	return false
}
