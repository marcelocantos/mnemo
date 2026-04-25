// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

// assistantWithBlocks returns a raw assistant entry whose message has an
// array of content blocks, including tool_use calls and a stop_reason.
func assistantWithBlocks(ts, stopReason string, blocks []map[string]any) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"message": map[string]any{
			"role":        "assistant",
			"stop_reason": stopReason,
			"content":     blocks,
		},
	}
}

// userWithToolResult returns a raw user entry whose message contains a
// tool_result content block (the canonical shape Claude Code produces).
func userWithToolResult(ts, toolUseID, resultText string) map[string]any {
	return map[string]any{
		"type":      "user",
		"timestamp": ts,
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     resultText,
				},
			},
		},
	}
}

// systemEntry returns a raw system-type entry with the given subtype.
func systemEntry(ts, subtype string) map[string]any {
	e := map[string]any{
		"type":      "system",
		"timestamp": ts,
	}
	if subtype != "" {
		e["subtype"] = subtype
	}
	return e
}

func TestSessionStructure_Basic(t *testing.T) {
	projectDir := t.TempDir()

	// Session with a mix of entry types and content-block kinds.
	writeJSONL(t, projectDir, "myproject", "sess-struct1", []map[string]any{
		// System entry with subtype.
		systemEntry("2026-04-01T10:00:00Z", "prompt"),
		// User text message.
		msg("user", "Run the build for me.", "2026-04-01T10:00:01Z"),
		// Assistant with tool_use + stop_reason "tool_use".
		assistantWithBlocks("2026-04-01T10:00:02Z", "tool_use", []map[string]any{
			{"type": "text", "text": "I'll run the build."},
			{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "make"}},
		}),
		// User with tool_result.
		userWithToolResult("2026-04-01T10:00:03Z", "tu1", "Build succeeded."),
		// Assistant with end_turn stop reason.
		assistantWithBlocks("2026-04-01T10:00:04Z", "end_turn", []map[string]any{
			{"type": "text", "text": "Build completed successfully."},
		}),
		// Another system entry, no subtype.
		systemEntry("2026-04-01T10:00:05Z", ""),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	st, err := s.SessionStructure("sess-struct1")
	if err != nil {
		t.Fatalf("SessionStructure: %v", err)
	}

	// Session ID should be resolved.
	if st.SessionID != "sess-struct1" {
		t.Errorf("unexpected session_id: %s", st.SessionID)
	}

	// TotalEntries: 6 JSONL lines.
	if st.TotalEntries != 6 {
		t.Errorf("TotalEntries: want 6, got %d", st.TotalEntries)
	}

	// EntryTypes: 2 user, 2 assistant, 2 system.
	if st.EntryTypes["user"] != 2 {
		t.Errorf("EntryTypes[user]: want 2, got %d", st.EntryTypes["user"])
	}
	if st.EntryTypes["assistant"] != 2 {
		t.Errorf("EntryTypes[assistant]: want 2, got %d", st.EntryTypes["assistant"])
	}
	if st.EntryTypes["system"] != 2 {
		t.Errorf("EntryTypes[system]: want 2, got %d", st.EntryTypes["system"])
	}

	// AssistantStopReasons: 1 tool_use, 1 end_turn.
	if st.AssistantStopReasons["tool_use"] != 1 {
		t.Errorf("stop_reasons[tool_use]: want 1, got %d", st.AssistantStopReasons["tool_use"])
	}
	if st.AssistantStopReasons["end_turn"] != 1 {
		t.Errorf("stop_reasons[end_turn]: want 1, got %d", st.AssistantStopReasons["end_turn"])
	}

	// SystemSubtypes: 1 "prompt", 1 "(none)".
	if st.SystemSubtypes["prompt"] != 1 {
		t.Errorf("system_subtypes[prompt]: want 1, got %d", st.SystemSubtypes["prompt"])
	}
	if st.SystemSubtypes["(none)"] != 1 {
		t.Errorf("system_subtypes[(none)]: want 1, got %d", st.SystemSubtypes["(none)"])
	}

	// ContentBlockKinds: text (3 blocks), tool_use (1), tool_result (1).
	// Note: the second user message is stored as a plain text string ("Run the build for me.")
	// via the simple-string path in extractBlocks, giving content_type "text" for that message too.
	if st.ContentBlockKinds["text"] < 1 {
		t.Errorf("content_block_kinds[text]: want ≥1, got %d", st.ContentBlockKinds["text"])
	}
	if st.ContentBlockKinds["tool_use"] != 1 {
		t.Errorf("content_block_kinds[tool_use]: want 1, got %d", st.ContentBlockKinds["tool_use"])
	}
	if st.ContentBlockKinds["tool_result"] != 1 {
		t.Errorf("content_block_kinds[tool_result]: want 1, got %d", st.ContentBlockKinds["tool_result"])
	}

	// ToolNames: Bash called once.
	if st.ToolNames["Bash"] != 1 {
		t.Errorf("tool_names[Bash]: want 1, got %d", st.ToolNames["Bash"])
	}
}

func TestSessionStructure_PrefixResolution(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "proj", "sess-prefix-abc", []map[string]any{
		msg("user", "hello", "2026-04-01T10:00:00Z"),
		msg("assistant", "hi", "2026-04-01T10:00:01Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	// Resolve by prefix.
	st, err := s.SessionStructure("sess-prefix")
	if err != nil {
		t.Fatalf("SessionStructure prefix: %v", err)
	}
	if st.SessionID != "sess-prefix-abc" {
		t.Errorf("resolved session_id: want sess-prefix-abc, got %s", st.SessionID)
	}
	if st.TotalEntries != 2 {
		t.Errorf("TotalEntries: want 2, got %d", st.TotalEntries)
	}
}

func TestSessionStructure_NotFound(t *testing.T) {
	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)
	// No ingest — the session doesn't exist.
	_, err := s.SessionStructure("nonexistent-session")
	if err == nil {
		t.Fatal("expected error for nonexistent session, got nil")
	}
}

func TestSessionStructure_AmbiguousPrefix(t *testing.T) {
	projectDir := t.TempDir()

	writeJSONL(t, projectDir, "proj", "sess-dup-aaa", []map[string]any{
		msg("user", "hello", "2026-04-01T10:00:00Z"),
	})
	writeJSONL(t, projectDir, "proj", "sess-dup-bbb", []map[string]any{
		msg("user", "world", "2026-04-01T10:00:00Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	_, err := s.SessionStructure("sess-dup")
	if err == nil {
		t.Fatal("expected error for ambiguous prefix, got nil")
	}
}
