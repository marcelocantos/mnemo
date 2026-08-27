// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/replay"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

func TestClaudeWriteEditEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "proj")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "replay-claude-001"
	storetest.WriteJSONL(t, dir, "user--proj", sid, []map[string]any{
		storetest.MetaMsg("user", "fix it", "2026-08-26T10:00:00Z", cwd, "main"),
		{
			"type": "assistant", "timestamp": "2026-08-26T10:00:01Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "tool_use", "id": "tu_w", "name": "Write",
						"input": map[string]any{
							"file_path": filepath.Join(cwd, "main.go"),
							"content":   "package main\n",
						},
					},
				},
			},
		},
		{
			"type": "user", "timestamp": "2026-08-26T10:00:02Z",
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_w", "content": "ok"},
				},
			},
		},
		{
			"type": "assistant", "timestamp": "2026-08-26T10:00:03Z",
			"message": map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "tool_use", "id": "tu_e", "name": "Edit",
						"input": map[string]any{
							"file_path":  filepath.Join(cwd, "main.go"),
							"old_string": "package main",
							"new_string": "package main // edited",
						},
					},
				},
			},
		},
		{
			"type": "user", "timestamp": "2026-08-26T10:00:04Z",
			"message": map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "tu_e", "content": "ok"},
				},
			},
		},
	})

	s := storetest.NewStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	sqlDB, err := replay.OpenReadOnly(s.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var nTool, nMeta int
	_ = sqlDB.QueryRow(`SELECT count(*) FROM messages WHERE session_id = ? AND content_type = 'tool_use'`, sid).Scan(&nTool)
	_ = sqlDB.QueryRow(`SELECT count(*) FROM session_meta WHERE session_id = ?`, sid).Scan(&nMeta)
	if nTool == 0 || nMeta == 0 {
		t.Fatalf("tool_use=%d session_meta=%d for %q", nTool, nMeta, sid)
	}
	db := replay.NewDB(sqlDB)
	ops, _, err := db.CollectOps(replay.Scope{SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatalf("ops=%d want 2", len(ops))
	}

	quar := t.TempDir()
	report, err := replay.NewEngine(replay.DefaultConfig()).Run(quar, ops)
	if err != nil {
		t.Fatal(err)
	}
	if report.OpsApplied != 2 {
		t.Fatalf("report=%+v", report)
	}
}
