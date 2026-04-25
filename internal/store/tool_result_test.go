// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"strings"
	"testing"
)

// toolUseMsg builds a JSONL assistant message containing a tool_use block.
func toolUseMsg(ts, toolName, toolUseID string, input map[string]any) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"id":    toolUseID,
					"name":  toolName,
					"input": input,
				},
			},
		},
	}
}

// toolResultMsg builds a JSONL user message containing a tool_result block.
func toolResultMsg(ts, toolUseID, content string) map[string]any {
	return map[string]any{
		"type":      "user",
		"timestamp": ts,
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
				},
			},
		},
	}
}

// toolResultErrorMsg builds a JSONL user message with an is_error tool_result.
func toolResultErrorMsg(ts, toolUseID, content string) map[string]any {
	return map[string]any{
		"type":      "user",
		"timestamp": ts,
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolUseID,
					"content":     content,
					"is_error":    true,
				},
			},
		},
	}
}

func TestToolResult(t *testing.T) {
	projectDir := t.TempDir()

	const sessionID = "sess-tr-001"
	const toolUseID = "toolu_abc123"
	const resultBody = "file contents here\nline two\nline three"

	writeJSONL(t, projectDir, "myproject", sessionID, []map[string]any{
		metaMsg("user", "Read the file for me", "2026-04-10T10:00:00Z",
			"/Users/dev/work/github.com/acme/webapp", "main"),
		toolUseMsg("2026-04-10T10:00:01Z", "Read", toolUseID,
			map[string]any{"file_path": "/some/file.txt"}),
		toolResultMsg("2026-04-10T10:00:02Z", toolUseID, resultBody),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	t.Run("basic lookup", func(t *testing.T) {
		payload, err := s.ToolResult(sessionID, toolUseID, 0, 0)
		if err != nil {
			t.Fatalf("ToolResult: %v", err)
		}
		if payload.Text != resultBody {
			t.Errorf("got text %q, want %q", payload.Text, resultBody)
		}
		if payload.IsError {
			t.Error("expected IsError=false")
		}
		if payload.TotalLen != len(resultBody) {
			t.Errorf("TotalLen=%d, want %d", payload.TotalLen, len(resultBody))
		}
		if payload.Truncated {
			t.Error("expected Truncated=false")
		}
	})

	t.Run("prefix session_id", func(t *testing.T) {
		payload, err := s.ToolResult("sess-tr", toolUseID, 0, 0)
		if err != nil {
			t.Fatalf("ToolResult with prefix: %v", err)
		}
		if payload.Text != resultBody {
			t.Errorf("prefix lookup got %q, want %q", payload.Text, resultBody)
		}
	})

	t.Run("truncate_len", func(t *testing.T) {
		payload, err := s.ToolResult(sessionID, toolUseID, 0, 10)
		if err != nil {
			t.Fatalf("ToolResult with truncate: %v", err)
		}
		if payload.Text != resultBody[:10] {
			t.Errorf("truncated text=%q, want %q", payload.Text, resultBody[:10])
		}
		if !payload.Truncated {
			t.Error("expected Truncated=true")
		}
		if payload.TotalLen != len(resultBody) {
			t.Errorf("TotalLen=%d, want %d", payload.TotalLen, len(resultBody))
		}
	})

	t.Run("offset", func(t *testing.T) {
		payload, err := s.ToolResult(sessionID, toolUseID, 5, 0)
		if err != nil {
			t.Fatalf("ToolResult with offset: %v", err)
		}
		want := resultBody[5:]
		if payload.Text != want {
			t.Errorf("offset text=%q, want %q", payload.Text, want)
		}
		if payload.TotalLen != len(resultBody) {
			t.Errorf("TotalLen=%d, want %d", payload.TotalLen, len(resultBody))
		}
	})

	t.Run("offset+truncate", func(t *testing.T) {
		payload, err := s.ToolResult(sessionID, toolUseID, 5, 6)
		if err != nil {
			t.Fatalf("ToolResult with offset+truncate: %v", err)
		}
		want := resultBody[5 : 5+6]
		if payload.Text != want {
			t.Errorf("offset+truncate text=%q, want %q", payload.Text, want)
		}
		if !payload.Truncated {
			t.Error("expected Truncated=true")
		}
	})

	t.Run("missing tool_use_id", func(t *testing.T) {
		_, err := s.ToolResult(sessionID, "toolu_nonexistent", 0, 0)
		if err == nil {
			t.Fatal("expected error for missing tool_use_id, got nil")
		}
		var notFound *ToolResultNotFoundError
		if !errors.As(err, &notFound) {
			t.Errorf("expected ToolResultNotFoundError, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "toolu_nonexistent") {
			t.Errorf("error message should mention tool_use_id, got: %v", err)
		}
	})

	t.Run("missing session_id", func(t *testing.T) {
		_, err := s.ToolResult("sess-does-not-exist", toolUseID, 0, 0)
		if err == nil {
			t.Fatal("expected error for missing session, got nil")
		}
	})
}

func TestToolResultIsError(t *testing.T) {
	projectDir := t.TempDir()

	const sessionID = "sess-tr-err"
	const toolUseID = "toolu_err456"
	const errBody = "file not found: /missing.txt"

	writeJSONL(t, projectDir, "myproject", sessionID, []map[string]any{
		metaMsg("user", "Read missing file", "2026-04-10T11:00:00Z",
			"/Users/dev/work/github.com/acme/webapp", "main"),
		toolUseMsg("2026-04-10T11:00:01Z", "Read", toolUseID,
			map[string]any{"file_path": "/missing.txt"}),
		toolResultErrorMsg("2026-04-10T11:00:02Z", toolUseID, errBody),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	payload, err := s.ToolResult(sessionID, toolUseID, 0, 0)
	if err != nil {
		t.Fatalf("ToolResult: %v", err)
	}
	if !payload.IsError {
		t.Error("expected IsError=true")
	}
	if payload.Text != errBody {
		t.Errorf("got text %q, want %q", payload.Text, errBody)
	}
}
