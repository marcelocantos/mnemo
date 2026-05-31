// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"fmt"
	"time"
)

// TreeOfInterest is a named directory root whose contents are linked
// into the docs table via doc_tree_refs. Trees may overlap (one root
// is an ancestor of another); content is stored once in docs, with
// each tree declaring a reference via doc_tree_refs.
type TreeOfInterest struct {
	ID        int64
	RootPath  string
	Label     string
	CreatedAt string
}

// UpsertTreeOfInterest creates or refreshes a tree-of-interest at
// rootPath. Returns the tree's id. Idempotent: repeated calls with the
// same rootPath return the same id and update the label.
func (s *Store) UpsertTreeOfInterest(rootPath, label string) (int64, error) {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	return s.upsertTreeOfInterestLocked(rootPath, label)
}

// upsertTreeOfInterestLocked is the lock-held implementation. Caller
// must hold s.rwmu write lock.
func (s *Store) upsertTreeOfInterestLocked(rootPath, label string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.writeDB.Exec(`
		INSERT INTO trees_of_interest (root_path, label, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(root_path) DO UPDATE SET label = excluded.label
	`, rootPath, label, now)
	if err != nil {
		return 0, fmt.Errorf("upsert tree_of_interest: %w", err)
	}
	var id int64
	if err := s.readDB.QueryRow(`SELECT id FROM trees_of_interest WHERE root_path = ?`, rootPath).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup tree_of_interest id: %w", err)
	}
	return id, nil
}

// LinkDocToTree records that the doc identified by docID is reachable
// through the tree identified by treeID. Idempotent via primary-key
// conflict.
func (s *Store) LinkDocToTree(docID, treeID int64) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	return s.linkDocToTreeLocked(docID, treeID)
}

// linkDocToTreeLocked is the lock-held implementation. Caller must
// hold s.rwmu write lock.
func (s *Store) linkDocToTreeLocked(docID, treeID int64) error {
	_, err := s.writeDB.Exec(`
		INSERT OR IGNORE INTO doc_tree_refs (doc_id, tree_id)
		VALUES (?, ?)
	`, docID, treeID)
	if err != nil {
		return fmt.Errorf("link doc_tree_refs: %w", err)
	}
	return nil
}

// DocsInTree returns docs reachable through the tree whose root_path
// matches the given path. Returns the same DocInfo shape as SearchDocs
// for consistency with existing callers.
func (s *Store) DocsInTree(rootPath string) ([]DocInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.readDB.Query(`
		SELECT d.id, d.repo, d.file_path, d.kind, d.title, d.content,
			d.content_hash, d.size, d.mtime, d.indexed_at
		FROM docs d
		JOIN doc_tree_refs r ON r.doc_id = d.id
		JOIN trees_of_interest t ON t.id = r.tree_id
		WHERE t.root_path = ?
		ORDER BY d.file_path
	`, rootPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocInfo
	for rows.Next() {
		var d DocInfo
		if err := rows.Scan(&d.ID, &d.Repo, &d.FilePath, &d.Kind, &d.Title,
			&d.Content, &d.ContentHash, &d.Size, &d.MTime, &d.IndexedAt); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// TreesForDoc returns every tree-of-interest that references the
// given doc. Used for the reverse lookup "which trees contain this
// file?".
func (s *Store) TreesForDoc(docID int64) ([]TreeOfInterest, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.readDB.Query(`
		SELECT t.id, t.root_path, t.label, t.created_at
		FROM trees_of_interest t
		JOIN doc_tree_refs r ON r.tree_id = t.id
		WHERE r.doc_id = ?
		ORDER BY t.root_path
	`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TreeOfInterest
	for rows.Next() {
		var t TreeOfInterest
		if err := rows.Scan(&t.ID, &t.RootPath, &t.Label, &t.CreatedAt); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// DeleteTreeOfInterest removes the tree at rootPath and all of its
// doc_tree_refs rows. Doc content is preserved — the tree only owned
// references, not content. Returns sql.ErrNoRows-style behaviour
// (silent no-op) if rootPath is unknown.
func (s *Store) DeleteTreeOfInterest(rootPath string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	var id int64
	if err := s.readDB.QueryRow(`SELECT id FROM trees_of_interest WHERE root_path = ?`, rootPath).Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("lookup tree_of_interest: %w", err)
	}
	// PRAGMA foreign_keys is not enabled (see store.go init), so do the
	// cascade explicitly rather than relying on ON DELETE CASCADE.
	if _, err := s.writeDB.Exec(`DELETE FROM doc_tree_refs WHERE tree_id = ?`, id); err != nil {
		return fmt.Errorf("delete doc_tree_refs: %w", err)
	}
	if _, err := s.writeDB.Exec(`DELETE FROM trees_of_interest WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete tree_of_interest: %w", err)
	}
	return nil
}
