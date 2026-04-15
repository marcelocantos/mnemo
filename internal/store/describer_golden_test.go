// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build system_test

package store

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// TestDescriberGoldenBatch runs the full batched-describer path against
// the 4 golden PNG fixtures: write to a temp SQLite DB, seed the images
// table, invoke describeBatchAndStore, and assert each fixture got a
// non-empty name + description that mentions expected keywords.
//
// Run with:
//
//	go test -tags "sqlite_fts5 system_test" -run TestDescriberGoldenBatch -v -timeout 5m ./internal/store/
func TestDescriberGoldenBatch(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH — describer test skipped")
	}

	// Build a temp DB with the current schema.
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "mnemo.sqlite")
	proj := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(dbPath, proj)
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	defer s.Close()

	// Seed images + one occurrence each from the golden fixtures.
	type fixture struct {
		name     string
		png      string
		keywords []string // at least one of these must appear in desc+name
	}
	cases := []fixture{
		{
			name:     "code_snippet",
			png:      "testdata/images/code_snippet.png",
			keywords: []string{"calculateFoobarIndex", "function", "code"},
		},
		{
			name:     "error_log",
			png:      "testdata/images/error_log.png",
			keywords: []string{"ECONNREFUSED", "error", "database", "connection"},
		},
		{
			name:     "architecture_diagram",
			png:      "testdata/images/architecture_diagram.png",
			keywords: []string{"flowchart", "diagram", "pipeline", "ingest"},
		},
		{
			name:     "data_table",
			png:      "testdata/images/data_table.png",
			keywords: []string{"table", "revenue", "region", "Q1"},
		},
	}

	var imageIDs []int64
	for _, fx := range cases {
		path, err := filepath.Abs(fx.png)
		if err != nil {
			t.Fatalf("abs %s: %v", fx.png, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		id, err := storeImage(s.db, data, "image/png", path)
		if err != nil {
			t.Fatalf("storeImage %s: %v", fx.name, err)
		}
		recordOccurrence(s.db, id, 0, 0, "test-session", "path", time.Now().UTC().Format(time.RFC3339))
		imageIDs = append(imageIDs, id)
	}

	// Claim and describe the batch (exercises the real claude -p path).
	batch, err := claimPendingBatch(s.db, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(batch) != len(cases) {
		t.Fatalf("expected %d claimed, got %d", len(cases), len(batch))
	}

	start := time.Now()
	describeBatchAndStore(s.db, batch)
	t.Logf("describeBatchAndStore elapsed: %v (%d images)", time.Since(start), len(batch))

	// Verify each fixture got a non-error description containing at least
	// one expected keyword.
	for i, fx := range cases {
		id := imageIDs[i]
		var name, description, errCol sql.NullString
		err := s.db.QueryRow(
			`SELECT name, description, error FROM image_descriptions WHERE image_id = ?`,
			id,
		).Scan(&name, &description, &errCol)
		if err != nil {
			t.Errorf("[%s] query description: %v", fx.name, err)
			continue
		}
		if errCol.Valid && errCol.String != "" {
			t.Errorf("[%s] description error: %s", fx.name, errCol.String)
			continue
		}
		if !description.Valid || description.String == "" {
			t.Errorf("[%s] description is empty", fx.name)
			continue
		}
		if !name.Valid || name.String == "" {
			t.Errorf("[%s] name is empty", fx.name)
		}
		haystack := strings.ToLower(name.String + " " + description.String)
		hit := false
		for _, kw := range fx.keywords {
			if strings.Contains(haystack, strings.ToLower(kw)) {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("[%s] none of the expected keywords (%v) found in:\nname: %s\ndesc: %s",
				fx.name, fx.keywords, name.String, description.String)
		} else {
			t.Logf("[%s] name=%q description=%s", fx.name, name.String, truncateForLog(description.String, 200))
		}
	}
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
