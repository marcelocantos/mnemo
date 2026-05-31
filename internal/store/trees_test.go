// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// insertDocRow inserts a minimal docs row for a given file_path and
// returns its id. Used by trees tests to exercise the doc_tree_refs
// table without going through the full ingest path.
func insertDocRow(t *testing.T, s *Store, repo, filePath string) int64 {
	t.Helper()
	now := time.Now().Format(time.RFC3339)
	res, err := s.writeDB.Exec(`
		INSERT INTO docs (repo, file_path, kind, title, content,
			content_hash, size, mtime, indexed_at)
		VALUES (?, ?, 'md', '', '', '', 0, ?, ?)`,
		repo, filePath, now, now)
	if err != nil {
		t.Fatalf("insert doc row: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// TestTreesOfInterestOverlap verifies the four T53 acceptance
// criteria using the tree-of-interest primitives directly:
//   - A file in the overlap of two trees is stored once in docs.
//   - Both trees declare a reference; tree-scoped queries return it.
//   - A query for a file's reachable trees returns all referring trees.
//   - Removing a tree updates references without rewriting content rows.
func TestTreesOfInterestOverlap(t *testing.T) {
	projectDir := t.TempDir()
	s := newTestStore(t, projectDir)

	parentRoot := "/abs/parent"
	childRoot := "/abs/parent/inner"
	overlapPath := "/abs/parent/inner/file.md"

	parentID, err := s.UpsertTreeOfInterest(parentRoot, "parent")
	if err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	childID, err := s.UpsertTreeOfInterest(childRoot, "child")
	if err != nil {
		t.Fatalf("upsert child: %v", err)
	}

	docID := insertDocRow(t, s, "test-repo", overlapPath)
	if err := s.LinkDocToTree(docID, parentID); err != nil {
		t.Fatalf("link parent: %v", err)
	}
	if err := s.LinkDocToTree(docID, childID); err != nil {
		t.Fatalf("link child: %v", err)
	}

	// Acceptance #1: one row in docs for the overlap file.
	rows, err := s.Query(`SELECT COUNT(*) AS cnt FROM docs WHERE file_path = '` + overlapPath + `'`)
	if err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if cnt, _ := rows[0]["cnt"].(int64); cnt != 1 {
		t.Fatalf("expected exactly 1 docs row for overlap path, got %v", rows[0]["cnt"])
	}

	// Acceptance #2: queries scoped to either tree return the file.
	parentDocs, err := s.DocsInTree(parentRoot)
	if err != nil {
		t.Fatalf("docs in parent tree: %v", err)
	}
	if len(parentDocs) != 1 || parentDocs[0].FilePath != overlapPath {
		t.Fatalf("parent tree: want [%s], got %+v", overlapPath, parentDocs)
	}
	childDocs, err := s.DocsInTree(childRoot)
	if err != nil {
		t.Fatalf("docs in child tree: %v", err)
	}
	if len(childDocs) != 1 || childDocs[0].FilePath != overlapPath {
		t.Fatalf("child tree: want [%s], got %+v", overlapPath, childDocs)
	}

	// Acceptance #3: reverse lookup returns both trees.
	trees, err := s.TreesForDoc(docID)
	if err != nil {
		t.Fatalf("trees for doc: %v", err)
	}
	if len(trees) != 2 {
		t.Fatalf("want 2 trees for overlap doc, got %d: %+v", len(trees), trees)
	}
	gotRoots := map[string]bool{trees[0].RootPath: true, trees[1].RootPath: true}
	if !gotRoots[parentRoot] || !gotRoots[childRoot] {
		t.Fatalf("trees for doc missing expected roots: %+v", trees)
	}

	// Acceptance #4: deleting a tree updates references without rewriting
	// content. The doc row must survive; the surviving tree's refs intact.
	if err := s.DeleteTreeOfInterest(parentRoot); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	rows, err = s.Query(`SELECT COUNT(*) AS cnt FROM docs WHERE file_path = '` + overlapPath + `'`)
	if err != nil {
		t.Fatalf("post-delete count docs: %v", err)
	}
	if cnt, _ := rows[0]["cnt"].(int64); cnt != 1 {
		t.Fatalf("doc row should survive parent-tree deletion; got %v", rows[0]["cnt"])
	}
	postTrees, err := s.TreesForDoc(docID)
	if err != nil {
		t.Fatalf("post-delete trees for doc: %v", err)
	}
	if len(postTrees) != 1 || postTrees[0].RootPath != childRoot {
		t.Fatalf("after deleting parent tree, want only child; got %+v", postTrees)
	}

	// Idempotent re-upsert returns the same id.
	childID2, err := s.UpsertTreeOfInterest(childRoot, "child-renamed")
	if err != nil {
		t.Fatalf("re-upsert child: %v", err)
	}
	if childID2 != childID {
		t.Fatalf("re-upsert should return same id; got %d vs %d", childID2, childID)
	}
}

// TestTreesOfInterestIngest verifies that IngestDocs registers each
// discovered repo root as a tree-of-interest and links every indexed
// doc into that tree. Demonstrates the forward path: ingest produces
// trees automatically, so the primitives are populated by the existing
// walker without requiring caller plumbing.
func TestTreesOfInterestIngest(t *testing.T) {
	projectDir := t.TempDir()
	repoRoot := filepath.Join(t.TempDir(), "org", "repo")
	s := newTestStore(t, projectDir)
	setupDocRepo(t, s, repoRoot)

	docsDir := filepath.Join(repoRoot, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(docsDir, "architecture.md")
	writeDoc(t, docPath, "# Architecture\n\nDescribes the layered design.\n")

	if err := s.IngestDocs(); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// One tree-of-interest at the repo root.
	rows, err := s.Query(`SELECT root_path FROM trees_of_interest WHERE root_path = '` + repoRoot + `'`)
	if err != nil {
		t.Fatalf("query trees: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 tree-of-interest for repo root, got %d", len(rows))
	}

	// The doc was linked into that tree.
	docs, err := s.DocsInTree(repoRoot)
	if err != nil {
		t.Fatalf("docs in tree: %v", err)
	}
	if len(docs) != 1 || docs[0].FilePath != docPath {
		t.Fatalf("want [%s] linked to repo tree, got %+v", docPath, docs)
	}

	// Re-ingest with unchanged content still keeps the link (skip path).
	if err := s.IngestDocs(); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	docs2, err := s.DocsInTree(repoRoot)
	if err != nil {
		t.Fatalf("docs in tree after re-ingest: %v", err)
	}
	if len(docs2) != 1 {
		t.Fatalf("re-ingest should keep one link, got %d", len(docs2))
	}
}
