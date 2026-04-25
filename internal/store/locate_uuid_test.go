// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

// TestLocateUUID verifies that LocateUUID finds entries by each of the four
// UUID sources: entry_uuid, parent_uuid, tool_use_id (content block), and
// tool_result_id (content block). It also checks top_tool_use_id and
// parent_tool_use_id (entry-level tool use fields used by progress entries).
func TestLocateUUID(t *testing.T) {
	projectDir := t.TempDir()
	sessionID := "locate-uuid-sess"
	project := "locatetest"

	// Entry 1: a plain user entry — locate by entry_uuid and parent_uuid of entry 2.
	entryUUID := "aaaa1111-0000-0000-0000-000000000001"

	// Entry 2: an assistant entry with a tool_use content block.
	assistantUUID := "bbbb2222-0000-0000-0000-000000000002"
	toolUseContentID := "toolu_01AAABBBCCCDDDEEE" // content block id

	// Entry 3: a user entry containing a tool_result that references entry 2's tool.
	toolResultEntryUUID := "cccc3333-0000-0000-0000-000000000003"

	// Entry 4: a progress/system entry with entry-level toolUseID.
	progressUUID := "dddd4444-0000-0000-0000-000000000004"
	entryLevelToolUseID := "toolu_01PROGRESSTOOLUSE"

	// Entry 5: an entry with parentToolUseID.
	resultEntryUUID := "eeee5555-0000-0000-0000-000000000005"
	parentToolUseIDValue := "toolu_01PARENTTOOLUSE"

	entries := []map[string]any{
		// Entry 1: plain user message.
		{
			"type":       "user",
			"uuid":       entryUUID,
			"parentUuid": nil,
			"timestamp":  "2026-04-01T10:00:00Z",
			"sessionId":  sessionID,
			"message": map[string]any{
				"role":    "user",
				"content": "Hello, please run a tool.",
			},
		},
		// Entry 2: assistant with tool_use content block.
		{
			"type":       "assistant",
			"uuid":       assistantUUID,
			"parentUuid": entryUUID,
			"timestamp":  "2026-04-01T10:00:05Z",
			"sessionId":  sessionID,
			"message": map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{
						"type": "text",
						"text": "I will run the tool now.",
					},
					{
						"type":  "tool_use",
						"id":    toolUseContentID,
						"name":  "Bash",
						"input": map[string]any{"command": "echo hello"},
					},
				},
			},
		},
		// Entry 3: user with tool_result referencing entry 2's tool.
		{
			"type":       "user",
			"uuid":       toolResultEntryUUID,
			"parentUuid": assistantUUID,
			"timestamp":  "2026-04-01T10:00:10Z",
			"sessionId":  sessionID,
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": toolUseContentID,
						"content":     "hello",
					},
				},
			},
		},
		// Entry 4: progress entry with entry-level toolUseID.
		{
			"type":      "system",
			"uuid":      progressUUID,
			"toolUseID": entryLevelToolUseID,
			"timestamp": "2026-04-01T10:00:15Z",
			"sessionId": sessionID,
			"message": map[string]any{
				"role":    "user",
				"content": "Tool is running...",
			},
		},
		// Entry 5: result entry with parentToolUseID.
		{
			"type":            "user",
			"uuid":            resultEntryUUID,
			"parentToolUseID": parentToolUseIDValue,
			"timestamp":       "2026-04-01T10:00:20Z",
			"sessionId":       sessionID,
			"message": map[string]any{
				"role":    "user",
				"content": "Tool result with parentToolUseID.",
			},
		},
	}

	writeJSONL(t, projectDir, project, sessionID, entries)

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		prefix        string
		wantMatchKind string
		wantMatchedID string
	}{
		{
			name:          "entry_uuid full",
			prefix:        entryUUID,
			wantMatchKind: "entry_uuid",
			wantMatchedID: entryUUID,
		},
		{
			name:          "entry_uuid prefix",
			prefix:        "aaaa1111",
			wantMatchKind: "entry_uuid",
			wantMatchedID: entryUUID,
		},
		{
			name:          "parent_uuid",
			prefix:        entryUUID, // entry 2 has parentUuid = entryUUID
			wantMatchKind: "parent_uuid",
			wantMatchedID: entryUUID,
		},
		{
			name:          "tool_use_id content block",
			prefix:        toolUseContentID,
			wantMatchKind: "tool_use_id",
			wantMatchedID: toolUseContentID,
		},
		{
			name:          "tool_result_id content block",
			prefix:        toolUseContentID, // tool_result back-references same id
			wantMatchKind: "tool_result_id",
			wantMatchedID: toolUseContentID,
		},
		{
			name:          "top_tool_use_id",
			prefix:        entryLevelToolUseID,
			wantMatchKind: "top_tool_use_id",
			wantMatchedID: entryLevelToolUseID,
		},
		{
			name:          "parent_tool_use_id",
			prefix:        parentToolUseIDValue,
			wantMatchKind: "parent_tool_use_id",
			wantMatchedID: parentToolUseIDValue,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			matches, err := s.LocateUUID(tc.prefix, 0, 0)
			if err != nil {
				t.Fatalf("LocateUUID(%q): %v", tc.prefix, err)
			}

			// Find a match with the expected match_kind.
			var found *UUIDMatch
			for i := range matches {
				if matches[i].MatchKind == tc.wantMatchKind {
					found = &matches[i]
					break
				}
			}

			if found == nil {
				kinds := make([]string, len(matches))
				for i, m := range matches {
					kinds[i] = m.MatchKind
				}
				t.Fatalf("LocateUUID(%q): no match with kind %q; got kinds %v", tc.prefix, tc.wantMatchKind, kinds)
			}

			if found.MatchedID != tc.wantMatchedID {
				t.Errorf("MatchedID = %q, want %q", found.MatchedID, tc.wantMatchedID)
			}
			if found.SessionID != sessionID {
				t.Errorf("SessionID = %q, want %q", found.SessionID, sessionID)
			}
		})
	}
}

// TestLocateUUIDNotFound verifies that LocateUUID returns nil (not an error)
// when no entry matches the supplied prefix.
func TestLocateUUIDNotFound(t *testing.T) {
	projectDir := t.TempDir()
	writeJSONL(t, projectDir, "empty", "empty-sess", []map[string]any{
		{
			"type":      "user",
			"uuid":      "ffff0000-0000-0000-0000-000000000001",
			"timestamp": "2026-04-01T10:00:00Z",
			"sessionId": "empty-sess",
			"message":   map[string]any{"role": "user", "content": "hi"},
		},
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	matches, err := s.LocateUUID("00000000-dead-beef-0000-000000000000", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no matches, got %d", len(matches))
	}
}

// TestLocateUUIDContext verifies that context messages are returned when
// context_before/context_after are non-zero.
func TestLocateUUIDContext(t *testing.T) {
	projectDir := t.TempDir()
	sessionID := "ctx-sess"

	targetUUID := "ctx00001-0000-0000-0000-000000000003"

	writeJSONL(t, projectDir, "ctxproj", sessionID, []map[string]any{
		{
			"type":      "user",
			"uuid":      "ctx00001-0000-0000-0000-000000000001",
			"timestamp": "2026-04-01T10:00:00Z",
			"sessionId": sessionID,
			"message":   map[string]any{"role": "user", "content": "message before target"},
		},
		{
			"type":      "assistant",
			"uuid":      "ctx00001-0000-0000-0000-000000000002",
			"timestamp": "2026-04-01T10:00:05Z",
			"sessionId": sessionID,
			"message":   map[string]any{"role": "assistant", "content": "assistant before target"},
		},
		{
			"type":      "user",
			"uuid":      targetUUID,
			"timestamp": "2026-04-01T10:00:10Z",
			"sessionId": sessionID,
			"message":   map[string]any{"role": "user", "content": "the target message"},
		},
		{
			"type":      "assistant",
			"uuid":      "ctx00001-0000-0000-0000-000000000004",
			"timestamp": "2026-04-01T10:00:15Z",
			"sessionId": sessionID,
			"message":   map[string]any{"role": "assistant", "content": "assistant after target"},
		},
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	matches, err := s.LocateUUID(targetUUID, 2, 2)
	if err != nil {
		t.Fatalf("LocateUUID: %v", err)
	}

	var found *UUIDMatch
	for i := range matches {
		if matches[i].MatchKind == "entry_uuid" {
			found = &matches[i]
			break
		}
	}
	if found == nil {
		t.Fatal("no entry_uuid match found")
	}

	if len(found.Before) == 0 {
		t.Error("expected Before context messages, got none")
	}
	if len(found.After) == 0 {
		t.Error("expected After context messages, got none")
	}
}
