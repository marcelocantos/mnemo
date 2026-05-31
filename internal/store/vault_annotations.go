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

// VaultIndexingOptions controls which subtree(s) of the vault
// IngestVaultAnnotations walks (🎯T64.1 — consent fix).
//
// A zero-value VaultIndexingOptions resolves to "_mnemo_only" scope
// with the default ".mnemoignore" filename and no includes. Callers
// that want to honour the user's config should fill this struct from
// Config (typically via Config.ResolvedVaultIndexingScope and
// Config.ResolvedVaultIndexingIgnoreFile).
type VaultIndexingOptions struct {
	// Scope is one of "_mnemo_only", "full", "includes". Empty resolves
	// to "_mnemo_only" inside IngestVaultAnnotations.
	Scope string

	// Includes lists vault-relative paths walked in addition to
	// <vault>/_mnemo/ when Scope == "includes". Ignored otherwise.
	Includes []string

	// IgnoreFile is the basename or vault-relative path of the
	// gitignore-syntax exclude file. Empty resolves to ".mnemoignore".
	IgnoreFile string
}

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
// from every .md file within the configured indexing scope.
//
//   - mnemo-generated notes (with a <!-- mnemo:generated --> fence): only
//     content BELOW the fence is indexed, so generated blocks (sessions,
//     decisions, plans, …) are never re-ingested. No feedback loop.
//   - User-created standalone files (no fence): the whole file is indexed
//     as human knowledge.
//
// opts selects the read surface:
//   - Scope "_mnemo_only" (default) walks only <vault>/_mnemo/.
//   - Scope "full" walks the entire vault (hidden dirs excluded).
//   - Scope "includes" walks <vault>/_mnemo/ plus each Includes path.
//
// Patterns in opts.IgnoreFile (default ".mnemoignore") at the vault
// root are honoured across every walked tree.
//
// Files with no human content are removed from the docs table
// (kind='vault') so stale rows don't accumulate. Files that fall
// outside the configured scope are NOT pruned — they remain
// indexed if a prior call captured them under a broader scope and
// the user has since narrowed it. This is by design: silent removal
// on scope narrowing would be a surprise consent issue in the
// opposite direction. Users who want to drop pre-existing rows can
// rebuild the index.
//
// Indexed rows use kind='vault' and repo=basename(vaultPath). They appear in
// SearchDocs(kind="vault") and in the main Search results alongside transcript
// messages.
func (s *Store) IngestVaultAnnotations(vaultPath string, opts VaultIndexingOptions) error {
	if fi, err := os.Stat(vaultPath); err != nil {
		return fmt.Errorf("vault: stat %s: %w", vaultPath, err)
	} else if !fi.IsDir() {
		return fmt.Errorf("vault: %s is not a directory", vaultPath)
	}

	scope := opts.Scope
	if scope == "" {
		scope = VaultIndexingScopeMnemoOnly
	}

	roots, err := resolveVaultIndexingRoots(vaultPath, scope, opts.Includes)
	if err != nil {
		return err
	}

	ignoreFileName := opts.IgnoreFile
	if ignoreFileName == "" {
		ignoreFileName = defaultVaultIgnoreFile
	}
	mi, err := LoadMnemoIgnore(filepath.Join(vaultPath, ignoreFileName))
	if err != nil {
		slog.Warn("vault: load .mnemoignore failed", "file", ignoreFileName, "err", err)
		mi = &MnemoIgnore{}
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	vaultRepo := filepath.Base(vaultPath)
	indexed, skipped, removed := 0, 0, 0

	// Snapshot existing vault rows so we can prune any whose source file
	// has been deleted, moved, or now lives outside the walked roots
	// (e.g. after vault_path is reconfigured to a different directory).
	// Files that exist but fall outside the configured scope are NOT
	// pruned — they're handed over to delete(stale, path) via the
	// inWalkedTree check below.
	stale := map[string]bool{}
	if rows, err := s.readDB.Query(
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

	// Track every path the walker visited (even when we skipped indexing
	// it for an ignore-file match or unchanged hash). Anything in the
	// stale map that wasn't visited AND no longer exists on disk is
	// pruned at the end.
	visited := map[string]bool{}

	walkOne := func(walkRoot string) error {
		return filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel := vaultRelPath(vaultPath, path)
			if d.IsDir() {
				// Skip hidden dirs (.obsidian/, .git/, .trash/, …) entirely.
				if path != walkRoot && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				if rel != "" && mi.Match(rel, true) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			if mi.Match(rel, false) {
				visited[path] = true
				delete(stale, path)
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}

			human := humanContentOf(string(data))
			visited[path] = true
			delete(stale, path)

			if human == "" {
				// No human content: remove any previously indexed row.
				if res, err := s.writeDB.Exec(
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
			_ = s.readDB.QueryRow(
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

			title := extractTitle(human)
			if title == "" {
				title = strings.TrimSuffix(rel, ".md")
			}
			now := time.Now().Format(time.RFC3339)

			_, err = s.writeDB.Exec(`
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
	}

	var walkErr error
	for _, r := range roots {
		// Missing roots are tolerated (a configured includes path may
		// not yet exist, and <vault>/_mnemo/ may not exist on a brand
		// new vault).
		if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
			continue
		}
		if err := walkOne(r); err != nil && walkErr == nil {
			walkErr = err
		}
	}

	// Prune rows whose source file no longer exists on disk AND fell
	// outside every walked tree. A row that simply moved out of scope
	// (e.g. user narrowed from "full" to "_mnemo_only" with the file
	// still present on disk) is left in place — narrowing scope does
	// not silently un-index.
	for p := range stale {
		if _, err := os.Stat(p); err == nil {
			// File still on disk; out-of-scope rows kept as-is.
			continue
		}
		if _, err := s.writeDB.Exec(
			"DELETE FROM docs WHERE file_path = ? AND kind = 'vault'", p,
		); err == nil {
			removed++
		}
	}

	slog.Info("vault: annotations ingested",
		"path", vaultPath, "scope", scope,
		"indexed", indexed, "skipped", skipped, "removed", removed)
	return walkErr
}

// resolveVaultIndexingRoots returns the absolute directory roots to
// walk for the given scope. The result preserves caller order and is
// not deduplicated — overlapping roots are tolerated by the walker
// (the visited map ensures each file is processed at most once).
func resolveVaultIndexingRoots(vaultPath, scope string, includes []string) ([]string, error) {
	mnemoSubtree := filepath.Join(vaultPath, "_mnemo")
	switch scope {
	case VaultIndexingScopeFull:
		return []string{vaultPath}, nil
	case VaultIndexingScopeIncludes:
		roots := []string{mnemoSubtree}
		for _, inc := range includes {
			cleaned := strings.TrimSpace(inc)
			if cleaned == "" {
				continue
			}
			if filepath.IsAbs(cleaned) {
				return nil, fmt.Errorf("vault_indexing_includes %q: must be vault-relative", inc)
			}
			cleaned = filepath.Clean(cleaned)
			if cleaned == "." || strings.HasPrefix(cleaned, "..") {
				return nil, fmt.Errorf("vault_indexing_includes %q: must stay inside vault", inc)
			}
			roots = append(roots, filepath.Join(vaultPath, cleaned))
		}
		return roots, nil
	case VaultIndexingScopeMnemoOnly, "":
		return []string{mnemoSubtree}, nil
	default:
		return nil, fmt.Errorf("vault_indexing_scope %q: must be one of %q, %q, %q",
			scope,
			VaultIndexingScopeMnemoOnly,
			VaultIndexingScopeFull,
			VaultIndexingScopeIncludes)
	}
}

// vaultRelPath returns path relative to vaultPath using forward
// slashes (so ignore patterns behave identically on every host OS),
// or the basename when the inputs are unrelated.
func vaultRelPath(vaultPath, path string) string {
	rel, err := filepath.Rel(vaultPath, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}
