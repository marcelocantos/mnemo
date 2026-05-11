// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// vaultGeneratedFence is the HTML comment line that vault.writeNote writes
// to separate generated content (above) from human annotations (below).
// Duplicated here so the store package does not import the vault package;
// must be kept in sync with vault.generatedFence.
const vaultGeneratedFence = "<!-- mnemo:generated -->"

// humanContentOf returns the human-authored portion of a vault Markdown file.
//
// Two cases:
//   - File contains a generated fence (a mnemo-written note): only the content
//     below the fence is human. Generated blocks above the fence are skipped
//     to avoid re-ingesting machine output.
//   - File contains no fence (a user-created standalone .md, e.g. their own
//     Obsidian/Logseq note dropped into the vault): the whole file is human
//     content and gets indexed in full.
//
// Fence detection is line-anchored: the fence must be a whole line by
// itself (modulo trailing whitespace). This avoids treating a user-pasted
// literal of the fence string (e.g. when quoting mnemo's own docs) as a
// real fence, which would otherwise hide everything above the pasted copy
// from indexing.
//
// Returns "" when the file is empty or only contains generated content.
func humanContentOf(raw string) string {
	end := len(raw)
	for end > 0 {
		start := strings.LastIndexByte(raw[:end], '\n') + 1
		line := strings.TrimRight(raw[start:end], " \t\r")
		if line == vaultGeneratedFence {
			return strings.TrimSpace(raw[end:])
		}
		if start == 0 {
			break
		}
		end = start - 1
	}
	return strings.TrimSpace(raw)
}

// IngestVaultAnnotations walks vaultPath and indexes human-authored content
// from every .md file:
//
//   - mnemo-generated notes (with a <!-- mnemo:generated --> fence): only
//     content BELOW the fence is indexed, so generated blocks (sessions,
//     decisions, plans, …) are never re-ingested. No feedback loop.
//   - User-created standalone files (no fence): the whole file is indexed
//     as human knowledge. This lets users drop their own .md files into the
//     vault and have them flow into mnemo_search alongside transcripts.
//
// Files with no human content are removed from the docs table (kind='vault')
// so stale rows don't accumulate.
//
// Indexed rows use kind='vault' and repo=basename(vaultPath). They appear in
// SearchDocs(kind="vault") and in the main Search results alongside transcript
// messages.
func (s *Store) IngestVaultAnnotations(vaultPath string) error {
	if fi, err := os.Stat(vaultPath); err != nil {
		return fmt.Errorf("vault: stat %s: %w", vaultPath, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("vault: %s is not a directory", vaultPath)
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	vaultRepo := filepath.Base(vaultPath)
	indexed, skipped, removed := 0, 0, 0

	// Snapshot existing vault rows so we can prune any whose source file
	// has been deleted, moved, or now lives outside this vault root (e.g.
	// after vault_path is reconfigured). Without this, deleting a note
	// would leave a stale row visible in search.
	stale := map[string]bool{}
	if rows, err := s.db.Query(
		"SELECT file_path FROM docs WHERE kind = 'vault'",
	); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				stale[p] = true
			}
		}
		rows.Close()
	}

	walkErr := filepath.WalkDir(vaultPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Skip hidden dirs (.obsidian/, .git/, .trash/, …) entirely — they
		// hold tool config and aren't human knowledge. Saves IO + watcher
		// slots on Linux inotify.
		if d.IsDir() {
			if path != vaultPath && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		human := humanContentOf(string(data))

		// This file still exists under the current vault root — don't prune it.
		delete(stale, path)

		if human == "" {
			// No human content: remove any previously indexed row.
			if res, err := s.db.Exec(
				"DELETE FROM docs WHERE file_path = ? AND kind = 'vault'", path,
			); err == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					removed++
				}
			}
			return nil
		}

		// If an existing row at this file_path is NOT a vault annotation
		// (e.g. a synthesis doc indexed via IngestDocs), leave it alone —
		// vault must never clobber a more authoritative doc kind.
		var existingKind, existingHash string
		_ = s.db.QueryRow(
			"SELECT kind, content_hash FROM docs WHERE file_path = ?", path,
		).Scan(&existingKind, &existingHash)
		if existingKind != "" && existingKind != "vault" {
			skipped++
			return nil
		}

		hash := contentHash([]byte(human))
		if existingHash == hash {
			skipped++
			return nil
		}

		rel, _ := filepath.Rel(vaultPath, path)
		title := extractTitle(human)
		if title == "" {
			title = strings.TrimSuffix(rel, ".md")
		}
		now := time.Now().Format(time.RFC3339)

		_, err = s.db.Exec(`
			INSERT INTO docs (repo, file_path, kind, title, content, content_hash,
				size, mtime, indexed_at, taxonomy, doc_date, doc_status, doc_target, doc_source)
			VALUES (?, ?, 'vault', ?, ?, ?, ?, ?, ?, '', '', '', '', '')
			ON CONFLICT(file_path) DO UPDATE SET
				repo         = excluded.repo,
				kind         = excluded.kind,
				title        = excluded.title,
				content      = excluded.content,
				content_hash = excluded.content_hash,
				size         = excluded.size,
				mtime        = excluded.mtime,
				indexed_at   = excluded.indexed_at,
				taxonomy     = '',
				doc_date     = '',
				doc_status   = '',
				doc_target   = '',
				doc_source   = ''
			WHERE docs.kind = 'vault'
		`, vaultRepo, path, title, human, hash, int64(len(human)), now, now)
		if err != nil {
			slog.Error("vault: ingest annotation failed", "file", path, "err", err)
			return nil
		}
		indexed++
		return nil
	})

	// Prune rows whose source file no longer exists under this vault root.
	for p := range stale {
		if _, err := s.db.Exec(
			"DELETE FROM docs WHERE file_path = ? AND kind = 'vault'", p,
		); err == nil {
			removed++
		}
	}

	slog.Info("vault: annotations ingested",
		"path", vaultPath,
		"indexed", indexed, "skipped", skipped, "removed", removed)
	return walkErr
}
