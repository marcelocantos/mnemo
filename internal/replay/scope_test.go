// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/replay"
	"github.com/marcelocantos/mnemo/internal/storetest"
)

func TestScopeSelectors(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "proj")
	_ = os.MkdirAll(filepath.Join(cwd, ".git"), 0o755)
	sid := "scope-sess-001"
	storetest.WriteJSONL(t, dir, "u--p", sid, []map[string]any{
		storetest.MetaMsg("user", "hi", "2026-08-26T10:00:00Z", cwd, "main"),
		writeTool("2026-08-26T10:00:01Z", "tu1", filepath.Join(cwd, "a.go"), "a"),
		toolResult("2026-08-26T10:00:02Z", "tu1"),
		writeTool("2026-08-26T10:01:00Z", "tu2", filepath.Join(cwd, "b.go"), "b"),
		toolResult("2026-08-26T10:01:01Z", "tu2"),
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
	db := replay.NewDB(sqlDB)

	// Single session
	ops, _, err := db.CollectOps(replay.Scope{SessionID: sid})
	if err != nil || len(ops) != 2 {
		t.Fatalf("session scope: ops=%d err=%v", len(ops), err)
	}

	// Intra-session window: only first write
	since := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 26, 10, 0, 30, 0, time.UTC)
	ops, _, err = db.CollectOps(replay.Scope{SessionID: sid, Since: &since, Until: &until})
	if err != nil || len(ops) != 1 {
		t.Fatalf("intra-session window: ops=%d err=%v", len(ops), err)
	}

	// Multi-session window (one session in range)
	ops, _, err = db.CollectOps(replay.Scope{Since: &since, Until: &until})
	if err != nil || len(ops) != 1 {
		t.Fatalf("multi-session window: ops=%d err=%v", len(ops), err)
	}
}

func writeTool(ts, id, path, content string) map[string]any {
	return map[string]any{
		"type": "assistant", "timestamp": ts,
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "tool_use", "id": id, "name": "Write",
					"input": map[string]any{"file_path": path, "content": content},
				},
			},
		},
	}
}

func toolResult(ts, id string) map[string]any {
	return map[string]any{
		"type": "user", "timestamp": ts,
		"message": map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"},
			},
		},
	}
}
