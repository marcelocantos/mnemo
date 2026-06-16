// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"path/filepath"
	"testing"
	"time"
)

// fixedNow is a stable reference date for the date-predicate tests.
var fixedNow = time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

const sampleTodo = `# Roadmap

- [ ] ship the parser 📅 2026-06-10 #core
- [ ] write the tool 📅 2026-06-20 🔼 #core
- [x] design schema ✅ 2026-06-08 #core
- [-] abandoned idea ❌ 2026-06-09

## Someday

- [ ] no due date task #ideas
- [ ] far future 📅 2026-12-01 #ideas
`

func ingestSampleTodos(t *testing.T) (*Store, string) {
	t.Helper()
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "myorg", "myrepo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)
	path := filepath.Join(repoRoot, "TODO.md")
	writeDoc(t, path, sampleTodo)
	if err := s.IngestTodos(); err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestIngestTodosBasic(t *testing.T) {
	s, _ := ingestSampleTodos(t)
	todos, err := s.SearchTodos(TodoQuery{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(todos) != 6 {
		t.Fatalf("got %d todos, want 6", len(todos))
	}

	// File fingerprint row exists.
	rows, err := s.Query("SELECT todo_count, content_hash, size FROM todo_files")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d todo_files rows, want 1", len(rows))
	}
	if c, _ := rows[0]["todo_count"].(int64); c != 6 {
		t.Errorf("todo_count = %v, want 6", rows[0]["todo_count"])
	}
}

func TestIngestTodosIncremental(t *testing.T) {
	s, path := ingestSampleTodos(t)

	// Re-ingest unchanged → no change in count.
	if n := s.ingestTodoFile(path, "myrepo"); n != 0 {
		t.Errorf("unchanged re-ingest wrote %d, want 0 (skipped)", n)
	}

	// Modify the file → re-ingest replaces.
	writeDoc(t, path, "# X\n- [ ] only one\n")
	if n := s.ingestTodoFile(path, "myrepo"); n != 1 {
		t.Errorf("changed re-ingest wrote %d, want 1", n)
	}
	todos, _ := s.SearchTodos(TodoQuery{Now: fixedNow})
	if len(todos) != 1 || todos[0].Text != "only one" {
		t.Errorf("after replace: %+v", todos)
	}
}

func TestSearchTodosFilters(t *testing.T) {
	s, _ := ingestSampleTodos(t)

	cases := []struct {
		name string
		q    TodoQuery
		want int
	}{
		{"all", TodoQuery{}, 6},
		{"open", TodoQuery{Status: "open"}, 4},
		{"done", TodoQuery{Status: "done"}, 1},
		{"cancelled", TodoQuery{Status: "cancelled"}, 1},
		{"tag core", TodoQuery{Tag: "core"}, 3},
		{"tag ideas", TodoQuery{Tag: "ideas"}, 2},
		{"priority medium", TodoQuery{Priority: "medium"}, 1},
		// done (✅) and cancelled (❌) tasks carry completion dates, not
		// 📅 due dates, so they have no due date — 3 tasks total.
		{"no date", TodoQuery{NoDate: true}, 3},
		{"no date open", TodoQuery{NoDate: true, Status: "open"}, 1},
		{"overdue", TodoQuery{Overdue: true, Now: fixedNow}, 1},      // due 06-10, still open
		{"due soon 7d", TodoQuery{DueSoonDays: 7, Now: fixedNow}, 1}, // due 06-20 within 06-16..06-23
		{"due before", TodoQuery{DueBefore: "2026-06-15"}, 1},        // 06-10 (06-08/06-09 are done/cancelled? no, they have ✅/❌ not 📅)
		{"fts parser", TodoQuery{Query: "parser"}, 1},
		{"section someday", TodoQuery{Section: "Someday"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.q.Now = orNow(c.q.Now)
			got, err := s.SearchTodos(c.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != c.want {
				t.Errorf("%s: got %d, want %d: %+v", c.name, len(got), c.want, got)
			}
		})
	}
}

func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return fixedNow
	}
	return t
}

func TestSearchTodosFieldsPopulated(t *testing.T) {
	s, _ := ingestSampleTodos(t)
	// relaxQuery ORs the terms, so "the" also matches "ship the parser";
	// the full match ranks first.
	got, err := s.SearchTodos(TodoQuery{Query: "write the tool", Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no results")
	}
	task := got[0]
	if task.Text != "write the tool" {
		t.Fatalf("top result %q, want %q", task.Text, "write the tool")
	}
	if task.Due != "2026-06-20" || task.Priority != "medium" || task.Section != "Roadmap" {
		t.Errorf("fields: due=%q priority=%q section=%q", task.Due, task.Priority, task.Section)
	}
	if len(task.Tags) != 1 || task.Tags[0] != "core" {
		t.Errorf("tags: %v", task.Tags)
	}
	if task.RepoOrEmpty() == "" {
		t.Errorf("repo empty")
	}
}

// RepoOrEmpty is a tiny test convenience to make the intent explicit.
func (t TodoInfo) RepoOrEmpty() string { return t.Repo }
