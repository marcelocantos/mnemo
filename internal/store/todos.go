// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/todo"
)

// TodoInfo is one indexed task, returned by SearchTodos and the
// mnemo_todos tool. Dates are ISO "YYYY-MM-DD" or empty.
type TodoInfo struct {
	ID         int64    `json:"id"`
	Repo       string   `json:"repo,omitempty"`
	FilePath   string   `json:"file_path"`
	Line       int      `json:"line"`
	Indent     int      `json:"indent,omitempty"`
	Status     string   `json:"status"`
	Text       string   `json:"text"`
	Section    string   `json:"section,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Due        string   `json:"due,omitempty"`
	Scheduled  string   `json:"scheduled,omitempty"`
	Start      string   `json:"start,omitempty"`
	Created    string   `json:"created,omitempty"`
	Done       string   `json:"done,omitempty"`
	Cancelled  string   `json:"cancelled,omitempty"`
	Recurrence string   `json:"recurrence,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Links      []string `json:"links,omitempty"`
}

// TodoQuery carries the filters for SearchTodos. The zero value lists
// recent tasks across all repos. Date predicates that depend on "today"
// (Overdue, DueSoonDays) resolve against Now, which defaults to the
// current date when zero.
type TodoQuery struct {
	Query       string // FTS over text/tags/section/repo
	Repo        string // substring match on repo
	Status      string // open / done / cancelled / in_progress
	Tag         string // exact whole-tag match
	Priority    string // priority name; exact match
	Section     string // substring match on section
	DueBefore   string // due_date <= this ISO date (and present)
	DueAfter    string // due_date >= this ISO date (and present)
	DueOn       string // due_date == this ISO date
	Overdue     bool   // due in the past and not done/cancelled
	DueSoonDays int    // due within N days from Now and not done/cancelled
	NoDate      bool   // no due date
	Limit       int
	Now         time.Time // reference "today"; zero → time.Now()
}

// IngestTodos discovers TODO files across all known repos, parses them
// in the Obsidian Tasks dialect, and indexes their tasks. Incremental:
// a file whose content hash is unchanged since last ingest is skipped.
func (s *Store) IngestTodos() error {
	s.rootsMu.RLock()
	roots := s.knownRepoRootsLocked()
	globs := append([]string(nil), s.todoGlobs...)
	s.rootsMu.RUnlock()

	indexed, onDisk := 0, 0
	for _, rr := range roots {
		n, od := s.ingestTodosForRepo(rr.root, rr.repo, globs)
		indexed += n
		onDisk += od
	}
	s.recordBackfillStatus("todos", indexed, onDisk)
	slog.Info("ingested todos", "tasks_indexed", indexed, "files_on_disk", onDisk)
	return nil
}

// isTodoFileName reports whether a base file name is a default TODO file.
func isTodoFileName(name string) bool {
	switch strings.ToLower(name) {
	case "todo.md", "todos.md":
		return true
	}
	return false
}

// ingestTodosForRepo walks one repo root, ingesting every TODO file it
// finds (default names plus configured globs), honouring .gitignore, the
// shared doc-exclude dirs, and the loop-safety exclusion fence.
func (s *Store) ingestTodosForRepo(repoRoot, repo string, globs []string) (indexed, onDisk int) {
	gitignore := parseGitignorePatterns(filepath.Join(repoRoot, ".gitignore"))

	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if s.IsExcluded(path) {
				return filepath.SkipDir
			}
			if path != repoRoot {
				name := d.Name()
				rel, _ := filepath.Rel(repoRoot, path)
				if docExcludeDirs[name] || matchesGitignore(gitignore, name, rel) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		name := d.Name()
		rel, _ := filepath.Rel(repoRoot, path)
		if matchesGitignore(gitignore, name, rel) {
			return nil
		}
		if !isTodoFileName(name) && !matchesTodoGlob(globs, rel) {
			return nil
		}
		onDisk++
		indexed += s.ingestTodoFile(path, repo)
		return nil
	})
	return
}

// matchesTodoGlob reports whether a repo-relative path matches any
// configured glob. Patterns use filepath.Match semantics against the
// slash-normalised relative path.
func matchesTodoGlob(globs []string, rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, g := range globs {
		if ok, _ := filepath.Match(g, rel); ok {
			return true
		}
		// Also match against the base name so "TASKS.md" works without
		// a leading path.
		if ok, _ := filepath.Match(g, filepath.Base(rel)); ok {
			return true
		}
	}
	return false
}

// ingestTodoFile parses one TODO file and replaces its tasks in the
// index. Returns the number of tasks written (0 when skipped unchanged
// or on read error). The todos for a file are replaced atomically: a
// stale parse never half-overwrites the previous one.
func (s *Store) ingestTodoFile(path, repo string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("read todo file failed", "file", path, "err", err)
		return 0
	}
	hash := contentHash(raw)

	var existing string
	_ = s.readDB.QueryRow("SELECT content_hash FROM todo_files WHERE file_path = ?", path).Scan(&existing)
	if existing == hash {
		return 0
	}

	tasks := todo.Parse(string(raw))
	size, mtime := statFingerprint(path)
	now := time.Now().Format(time.RFC3339)

	tx, err := s.writeDB.Begin()
	if err != nil {
		slog.Error("todo tx begin failed", "file", path, "err", err)
		return 0
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM todos WHERE file_path = ?", path); err != nil {
		slog.Error("todo delete failed", "file", path, "err", err)
		return 0
	}
	for _, t := range tasks {
		links, _ := json.Marshal(t.Links)
		if _, err := tx.Exec(`
			INSERT INTO todos (repo, file_path, line, indent, status, text, raw_line,
				section, priority, due_date, scheduled_date, start_date, created_date,
				done_date, cancelled_date, recurrence, tags, links, indexed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			repo, path, t.Line, t.Indent, string(t.Status), t.Text, t.RawLine,
			t.Section, int(t.Priority), t.Due, t.Scheduled, t.Start, t.Created,
			t.Done, t.Cancelled, t.Recurrence, strings.Join(t.Tags, " "), string(links), now,
		); err != nil {
			slog.Error("todo insert failed", "file", path, "err", err)
			return 0
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO todo_files (file_path, repo, content_hash, size, mtime, todo_count, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			repo         = excluded.repo,
			content_hash = excluded.content_hash,
			size         = excluded.size,
			mtime        = excluded.mtime,
			todo_count   = excluded.todo_count,
			indexed_at   = excluded.indexed_at`,
		path, repo, hash, size, mtime, len(tasks), now); err != nil {
		slog.Error("todo_files upsert failed", "file", path, "err", err)
		return 0
	}
	if err := tx.Commit(); err != nil {
		slog.Error("todo tx commit failed", "file", path, "err", err)
		return 0
	}
	return len(tasks)
}

// SearchTodos returns tasks matching q, newest-indexed first (or by FTS
// rank when a text query is supplied).
func (s *Store) SearchTodos(q TodoQuery) ([]TodoInfo, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	today := now.Format("2006-01-02")

	var where []string
	var args []any

	if q.Repo != "" {
		where = append(where, "t.repo LIKE ?")
		args = append(args, "%"+q.Repo+"%")
	}
	if q.Status != "" {
		where = append(where, "t.status = ?")
		args = append(args, q.Status)
	}
	if q.Priority != "" {
		where = append(where, "t.priority = ?")
		args = append(args, int(todo.PriorityFromString(q.Priority)))
	}
	if q.Section != "" {
		where = append(where, "t.section LIKE ?")
		args = append(args, "%"+q.Section+"%")
	}
	if q.Tag != "" {
		// Exact whole-tag match against the space-joined tags column.
		where = append(where, "(' ' || t.tags || ' ') LIKE ?")
		args = append(args, "% "+strings.TrimPrefix(q.Tag, "#")+" %")
	}
	if q.NoDate {
		where = append(where, "t.due_date = ''")
	}
	if q.DueOn != "" {
		where = append(where, "t.due_date = ?")
		args = append(args, q.DueOn)
	}
	if q.DueBefore != "" {
		where = append(where, "t.due_date != '' AND t.due_date <= ?")
		args = append(args, q.DueBefore)
	}
	if q.DueAfter != "" {
		where = append(where, "t.due_date != '' AND t.due_date >= ?")
		args = append(args, q.DueAfter)
	}
	if q.Overdue {
		where = append(where, "t.due_date != '' AND t.due_date < ? AND t.status NOT IN ('done','cancelled')")
		args = append(args, today)
	}
	if q.DueSoonDays > 0 {
		soon := now.AddDate(0, 0, q.DueSoonDays).Format("2006-01-02")
		where = append(where, "t.due_date != '' AND t.due_date >= ? AND t.due_date <= ? AND t.status NOT IN ('done','cancelled')")
		args = append(args, today, soon)
	}

	cols := `t.id, t.repo, t.file_path, t.line, t.indent, t.status, t.text, t.section,
		t.priority, t.due_date, t.scheduled_date, t.start_date, t.created_date,
		t.done_date, t.cancelled_date, t.recurrence, t.tags, t.links`

	var sql string
	if q.Query != "" {
		args = append([]any{relaxQuery(q.Query)}, args...)
		sql = `SELECT ` + cols + ` FROM todos t
			JOIN todos_fts f ON f.rowid = t.id
			WHERE todos_fts MATCH ?`
		if len(where) > 0 {
			sql += " AND " + strings.Join(where, " AND ")
		}
		sql += " ORDER BY rank LIMIT ?"
	} else {
		sql = `SELECT ` + cols + ` FROM todos t`
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		// Undated tasks sort last; otherwise by due date ascending then
		// priority descending so the most urgent work surfaces first.
		sql += ` ORDER BY (t.due_date = '') ASC, t.due_date ASC, t.priority DESC, t.id ASC LIMIT ?`
	}
	args = append(args, limit)

	return s.queryTodos(sql, args...)
}

func (s *Store) queryTodos(sql string, args ...any) ([]TodoInfo, error) {
	rows, err := s.readDB.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TodoInfo
	for rows.Next() {
		var t TodoInfo
		var prio int
		var tags, links string
		if err := rows.Scan(&t.ID, &t.Repo, &t.FilePath, &t.Line, &t.Indent, &t.Status,
			&t.Text, &t.Section, &prio, &t.Due, &t.Scheduled, &t.Start, &t.Created,
			&t.Done, &t.Cancelled, &t.Recurrence, &tags, &links); err != nil {
			continue
		}
		t.Priority = todo.Priority(prio).String()
		if f := strings.Fields(tags); len(f) > 0 {
			t.Tags = f
		}
		if links != "" {
			_ = json.Unmarshal([]byte(links), &t.Links)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
