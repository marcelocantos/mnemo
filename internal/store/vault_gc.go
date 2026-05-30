// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// VaultOutput is one manifest row — a note the exporter produced
// (🎯T68.6). note_path is vault-relative.
type VaultOutput struct {
	NotePath    string `json:"note_path"`
	EntityKind  string `json:"entity_kind"`
	EntityID    string `json:"entity_id"`
	ContentHash string `json:"content_hash"`
	WrittenAt   string `json:"written_at"`
}

// VaultOrphans groups the two orphan classes the GC pass distinguishes.
type VaultOrphans struct {
	// ManifestPathMissing: manifest rows whose note_path is not on
	// disk anymore (the user or another process removed the file).
	// Safe to remove the manifest row — no filesystem action.
	ManifestPathMissing []VaultOutput `json:"manifest_path_missing"`
	// DiskNotInManifest: *.md files under the vault root with no
	// manifest entry. These predate manifest tracking, are written by
	// some other tool, or are user notes. Treated conservatively at
	// the GC layer — never deleted from this layer alone; only
	// reported. A higher policy (e.g. T64's legacy-layout retire)
	// decides whether any of these are safe to remove.
	DiskNotInManifest []string `json:"disk_not_in_manifest"`
}

// HashVaultContent returns the SHA-256 of the rendered note content.
// Used to populate vault_outputs.content_hash so the GC pass can
// verify the on-disk note is still the artifact we wrote before
// removing it.
func HashVaultContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// RecordVaultOutput UPSERTs a manifest row for one note the exporter
// just wrote. Idempotent: rewriting the same path updates
// content_hash and written_at.
func (s *Store) RecordVaultOutput(notePath, entityKind, entityID, contentHash string, writtenAt time.Time) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	_, err := s.db.Exec(`
		INSERT INTO vault_outputs (note_path, entity_kind, entity_id, content_hash, written_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(note_path) DO UPDATE SET
			entity_kind  = excluded.entity_kind,
			entity_id    = excluded.entity_id,
			content_hash = excluded.content_hash,
			written_at   = excluded.written_at
	`, notePath, entityKind, entityID, contentHash, writtenAt.UTC().Format(time.RFC3339Nano))
	return err
}

// ScanVaultOrphans walks vaultPath and the vault_outputs manifest to
// produce both orphan classes (🎯T68.6 vault GC detection). Pure
// read-only — never modifies state.
//
// vaultPath is the on-disk vault root (e.g. ~/Documents/PKM). The
// detection is path-scheme-agnostic: it does not parse slugs or
// reverse-map filenames to entities. Two exact set-differences over
// the manifest's note_path key, period.
//
// Empty vaultPath returns an empty result without scanning.
func (s *Store) ScanVaultOrphans(vaultPath string) (VaultOrphans, error) {
	var out VaultOrphans
	if vaultPath == "" {
		return out, nil
	}

	manifest, err := s.listVaultManifest()
	if err != nil {
		return out, err
	}
	manifestPaths := make(map[string]VaultOutput, len(manifest))
	for _, m := range manifest {
		manifestPaths[m.NotePath] = m
	}

	onDisk := map[string]bool{}
	if err := filepath.WalkDir(vaultPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries silently
		}
		if d.IsDir() {
			// Skip hidden subtrees (e.g. .obsidian/, .git/) — never
			// owned by the exporter.
			if path != vaultPath && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, rerr := filepath.Rel(vaultPath, path)
		if rerr != nil {
			return nil
		}
		onDisk[filepath.ToSlash(rel)] = true
		return nil
	}); err != nil {
		return out, fmt.Errorf("walk vault: %w", err)
	}

	// Manifest rows whose path is not on disk.
	for path, m := range manifestPaths {
		if !onDisk[filepath.ToSlash(path)] {
			out.ManifestPathMissing = append(out.ManifestPathMissing, m)
		}
	}
	// Files on disk with no manifest row.
	for path := range onDisk {
		if _, ok := manifestPaths[path]; !ok {
			out.DiskNotInManifest = append(out.DiskNotInManifest, path)
		}
	}
	return out, nil
}

// VaultOrphanBacklog returns the total orphan count for the T68.4
// divergence surface (the gap to a converged manifest+disk).
func (s *Store) VaultOrphanBacklog(vaultPath string) int {
	rep, err := s.ScanVaultOrphans(vaultPath)
	if err != nil {
		return 0
	}
	return len(rep.ManifestPathMissing) + len(rep.DiskNotInManifest)
}

func (s *Store) listVaultManifest() ([]VaultOutput, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(
		`SELECT note_path, entity_kind, entity_id, content_hash, written_at FROM vault_outputs`)
	if err != nil {
		return nil, fmt.Errorf("query vault_outputs: %w", err)
	}
	defer rows.Close()
	var out []VaultOutput
	for rows.Next() {
		var m VaultOutput
		if err := rows.Scan(&m.NotePath, &m.EntityKind, &m.EntityID, &m.ContentHash, &m.WrittenAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetVaultPath records the configured vault root so the vault
// divergence gatherer can find it (🎯T68.6). Safe to call concurrently;
// pass "" to clear when vault is unconfigured.
func (s *Store) SetVaultPath(path string) {
	s.mu.Lock()
	s.vaultPath = path
	s.mu.Unlock()
}

// getVaultPath returns the currently-configured vault root or "".
func (s *Store) getVaultPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vaultPath
}

// RemoveVaultManifestRow drops a manifest row by note_path. Called by
// the GC pass for "manifest path missing" orphans (the file is gone;
// the manifest entry has nothing to point at). Never touches the
// filesystem — that's the caller's responsibility for disk orphans.
func (s *Store) RemoveVaultManifestRow(notePath string) error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM vault_outputs WHERE note_path = ?`, notePath)
	return err
}
