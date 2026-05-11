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

// belowVaultFence extracts the content below the generated fence in a vault
// note, trimming leading/trailing whitespace. Returns "" when the file has no
// fence or no content below it.
func belowVaultFence(raw string) string {
	idx := strings.LastIndex(raw, vaultGeneratedFence)
	if idx < 0 {
		return ""
	}
	below := strings.TrimSpace(raw[idx+len(vaultGeneratedFence):])
	return below
}

// IngestVaultAnnotations walks vaultPath and indexes the human-authored
// content below the <!-- mnemo:generated --> fence in each .md file. Only
// below-fence content is indexed, so generated blocks (sessions, decisions,
// plans, …) are never re-ingested — there is no feedback loop.
//
// Files with no fence or no human content below the fence are removed from
// the docs table (kind='vault') so stale rows don't accumulate.
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

	walkErr := filepath.WalkDir(vaultPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		human := belowVaultFence(string(data))

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

	slog.Info("vault: annotations ingested",
		"path", vaultPath,
		"indexed", indexed, "skipped", skipped, "removed", removed)
	return walkErr
}
