// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

const toolSampleTodo = `# Roadmap

- [ ] ship the parser 📅 2026-06-10 #core
- [x] design schema ✅ 2026-06-08 #core
- [ ] no due date task #ideas
`

// newTodoHandler builds a callHandler backed by a real store that has
// indexed one TODO file in a discoverable repo.
func newTodoHandler(t *testing.T) (*callHandler, *store.Store, string) {
	t.Helper()
	workRoot := t.TempDir()
	repoRoot := filepath.Join(workRoot, "myrepo")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	todoPath := filepath.Join(repoRoot, "TODO.md")
	if err := os.WriteFile(todoPath, []byte(toolSampleTodo), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(filepath.Join(t.TempDir(), "db.sqlite"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetWorkspaceRoots([]string{workRoot})
	if err := s.IngestTodos(); err != nil {
		t.Fatal(err)
	}
	return &callHandler{mem: s}, s, todoPath
}

func TestTodosToolQuery(t *testing.T) {
	ch, _, _ := newTodoHandler(t)

	out, isErr, err := ch.todos(map[string]any{"status": "open"})
	if err != nil || isErr {
		t.Fatalf("todos: isErr=%v err=%v out=%s", isErr, err, out)
	}
	var got []store.TodoInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d open todos, want 2", len(got))
	}

	// Invalid status is rejected.
	if out, isErr, _ := ch.todos(map[string]any{"status": "bogus"}); !isErr {
		t.Errorf("expected error for bogus status, got %s", out)
	}
}

func TestTodoSetTool(t *testing.T) {
	ch, s, path := newTodoHandler(t)
	tasks, _ := s.SearchTodos(store.TodoQuery{})
	var id int64
	for _, td := range tasks {
		if td.Text == "ship the parser" {
			id = td.ID
		}
	}
	if id == 0 {
		t.Fatal("task not found")
	}

	out, isErr, err := ch.todoSet(map[string]any{"id": float64(id), "status": "done"})
	if err != nil || isErr {
		t.Fatalf("todoSet: isErr=%v err=%v out=%s", isErr, err, out)
	}
	var updated store.TodoInfo
	if err := json.Unmarshal([]byte(out), &updated); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if updated.Status != "done" {
		t.Errorf("status=%s, want done", updated.Status)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "- [x] ship the parser") {
		t.Errorf("file not updated:\n%s", data)
	}

	// No-op call (no fields) is rejected.
	if _, isErr, _ := ch.todoSet(map[string]any{"id": float64(id)}); !isErr {
		t.Error("expected error for empty mutation")
	}
}

func TestTodoAddTool(t *testing.T) {
	ch, s, path := newTodoHandler(t)

	out, isErr, err := ch.todoAdd(map[string]any{
		"file": path,
		"text": "added via tool 📅 2026-09-01 #core",
	})
	if err != nil || isErr {
		t.Fatalf("todoAdd: isErr=%v err=%v out=%s", isErr, err, out)
	}
	tasks, _ := s.SearchTodos(store.TodoQuery{})
	found := false
	for _, td := range tasks {
		if td.Text == "added via tool" && td.Due == "2026-09-01" {
			found = true
		}
	}
	if !found {
		t.Errorf("added task not indexed: %+v", tasks)
	}

	// Missing required args are rejected.
	if _, isErr, _ := ch.todoAdd(map[string]any{"text": "x"}); !isErr {
		t.Error("expected error for missing file")
	}
}
