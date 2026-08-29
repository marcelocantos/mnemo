// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import "testing"

func TestInterestTranscriptAllowDeny(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/Users/x/.claude/projects/foo/abc-session.jsonl", true},
		{"/Users/x/.codex/sessions/2026/08/01/rollout-2026-08-01T12-00-00-uuid.jsonl", true},
		{"/Users/x/.grok/sessions/proj/sid/updates.jsonl", true},
		{"/Users/x/.grok/sessions/proj/sid/events.jsonl", false},
		{"/Users/x/.grok/sessions/proj/sid/chat_history.jsonl", false},
		{"/Users/x/.grok/sessions/proj/sid/terminal/out.jsonl", false},
		{"/Users/x/.cursor/chats/abc123/deadbeef-0000-0000-0000-000000000001/store.db", true},
		{"/Users/x/.cursor/acp-sessions/deadbeef-0000-0000-0000-000000000001/store.db", false},
		{"/Users/x/.cursor/chats/abc123/deadbeef-0000-0000-0000-000000000001/store.db-wal", false},
		{"/Users/x/.claude/projects/foo/memory/MEMORY.md", true},
		{"/Users/x/.claude/skills/commit/SKILL.md", true},
		{"/repo/CLAUDE.md", true},
		{"/repo/docs/audit-log.md", true},
		{"/repo/docs/targets.md", true},
		{"/repo/TODO.md", true},
		{"/repo/.planning/phase-1.md", true},
		{"/repo/README.md", false},
		{"/repo/main.go", false},
	}
	for _, tc := range cases {
		got := Interest(tc.path, ModeTranscript)
		if got != tc.want {
			t.Errorf("Interest(%q)=%v want %v", tc.path, got, tc.want)
		}
	}
}

func TestInterestVault(t *testing.T) {
	if !Interest("/vault/notes/foo.md", ModeVault) {
		t.Fatal("expected vault md allowed")
	}
	if Interest("/vault/.obsidian/app.json", ModeVault) {
		t.Fatal("hidden vault path denied")
	}
	if Interest("/vault/notes/foo.txt", ModeVault) {
		t.Fatal("non-md denied")
	}
}
