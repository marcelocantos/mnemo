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

// vaultTreeLabelPrefix is the label prefix used for trees_of_interest
// rows registered by the vault ingest path. The prefix lets the next
// ingest pass prune trees that left the configured scope (e.g. an
// includes-mode include path the user removed from config).
const vaultTreeLabelPrefix = "vault:"

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

// VaultIngestOptions configures the read surface for the vault ingest
// walker. The zero value selects scope "full" with no includes and no
// ignore patterns — the same effective behaviour as v1, kept available
// for tests and for callers that have already resolved scope upstream.
//
// Callers in production code compose this from store.Config helpers:
// EffectiveVaultIndexingScope, ResolvedVaultIndexingIncludes, and
// LoadVaultIgnore(ResolvedVaultIgnoreFile(...)).
type VaultIngestOptions struct {
	// Scope is one of VaultScopeMnemoOnly, VaultScopeFull,
	// VaultScopeIncludes, or "" (treated as VaultScopeFull for
	// continuity with the v1 single-arg signature).
	Scope string

	// Includes lists absolute paths walked in addition to
	// <vault>/_mnemo/ when Scope == VaultScopeIncludes. Entries
	// must already be vault-rooted (filepath.Rel of the vault is
	// not "..") — callers use Config.ResolvedVaultIndexingIncludes
	// to produce safe values.
	Includes []string

	// Ignore is a parsed .mnemoignore matcher applied at every
	// path tested by the walker, in every scope. nil means "no
	// extra exclusions".
	Ignore *VaultIgnore
}

// IngestVaultAnnotations walks vaultPath under the configured scope and
// indexes human-authored content from every .md file it visits:
//
//   - mnemo-generated notes (with a <!-- mnemo:generated --> fence): only
//     content BELOW the fence is indexed, so generated blocks (sessions,
//     decisions, plans, …) are never re-ingested. No feedback loop.
//   - User-created standalone files (no fence): the whole file is indexed
//     as human knowledge.
//
// Files with no human content are removed from the docs table (kind='vault')
// so stale rows don't accumulate. Each indexed doc is linked into one of
// the registered trees_of_interest (T53) so configuration changes —
// dropping an include path or narrowing scope — can be reflected by
// pruning the tree without touching unrelated content.
//
// Indexed rows use kind='vault' and repo=basename(vaultPath). They appear
// in SearchDocs(kind="vault") and in the main Search results alongside
// transcript messages.
func (s *Store) IngestVaultAnnotations(vaultPath string, opts VaultIngestOptions) error {
	fi, err := os.Stat(vaultPath)
	if err != nil {
		return fmt.Errorf("vault: stat %s: %w", vaultPath, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("vault: %s is not a directory", vaultPath)
	}

	roots, err := scopeRoots(vaultPath, opts)
	if err != nil {
		return err
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	vaultRepo := filepath.Base(vaultPath)
	indexed, skipped, removed := 0, 0, 0

	// Snapshot existing vault rows so we can prune any whose source file
	// has been deleted, moved, or now lives outside the active scope.
	// Without this, removing a note (or narrowing scope) would leave a
	// stale row visible in search.
	stale := map[string]int64{} // file_path -> doc id
	if rows, err := s.db.Query(
		"SELECT id, file_path FROM docs WHERE kind = 'vault'",
	); err == nil {
		for rows.Next() {
			var id int64
			var p string
			if rows.Scan(&id, &p) == nil {
				stale[p] = id
			}
		}
		rows.Close()
	}

	// Register or refresh the trees the configured scope expects.
	treeIDs := make(map[string]int64, len(roots))
	for _, root := range roots {
		id, err := s.upsertTreeOfInterestLocked(root.path, root.label)
		if err != nil {
			slog.Warn("vault: upsert tree_of_interest failed", "root", root.path, "err", err)
			continue
		}
		treeIDs[root.path] = id
	}

	// Prune any vault-labelled trees that fell out of scope (e.g. an
	// include path the user just removed from config). Doc rows the
	// disappearing tree exclusively pointed to are caught by the
	// stale-prune below; rows still inside another tree's reach keep
	// their content but lose the orphan ref.
	if err := s.pruneOrphanedVaultTreesLocked(treeIDs); err != nil {
		slog.Warn("vault: prune orphan trees", "err", err)
	}

	// Walk each root. Roots are visited in declaration order
	// (deterministic for log reproducibility). The same file
	// appearing under two overlapping roots is upserted once and
	// linked to every tree that reaches it.
	for _, root := range roots {
		treeID := treeIDs[root.path]
		err := filepath.WalkDir(root.path, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			rel, relErr := filepath.Rel(vaultPath, path)
			if relErr != nil {
				return nil
			}
			if d.IsDir() {
				// Skip hidden dirs at any depth — .obsidian/, .git/,
				// .trash/, .logseq/ caches, etc. These hold tool config
				// and aren't human knowledge. Saves IO + watcher slots.
				if path != root.path && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				if opts.Ignore != nil && opts.Ignore.Match(rel, true) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			if opts.Ignore != nil && opts.Ignore.Match(rel, false) {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}

			human := humanContentOf(string(data))
			delete(stale, path)

			if human == "" {
				if res, err := s.db.Exec(
					"DELETE FROM docs WHERE file_path = ? AND kind = 'vault'", path,
				); err == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						removed++
					}
				}
				_, _ = s.db.Exec("DELETE FROM doc_tree_refs WHERE doc_id IN (SELECT id FROM docs WHERE file_path = ?)", path)
				return nil
			}

			// If an existing row at this file_path is NOT a vault
			// annotation (e.g. a synthesis doc indexed via IngestDocs),
			// leave it alone — vault must never clobber a more
			// authoritative doc kind.
			var existingKind, existingHash string
			_ = s.db.QueryRow(
				"SELECT kind, content_hash FROM docs WHERE file_path = ?", path,
			).Scan(&existingKind, &existingHash)
			if existingKind != "" && existingKind != "vault" {
				skipped++
				return nil
			}

			hash := contentHash([]byte(human))
			title := extractTitle(human)
			if title == "" {
				title = strings.TrimSuffix(rel, ".md")
			}
			now := time.Now().Format(time.RFC3339)

			if existingHash != hash {
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
			} else {
				skipped++
			}

			if treeID != 0 {
				var docID int64
				if err := s.db.QueryRow(
					"SELECT id FROM docs WHERE file_path = ?", path,
				).Scan(&docID); err == nil && docID != 0 {
					_ = s.linkDocToTreeLocked(docID, treeID)
				}
			}
			return nil
		})
		if err != nil {
			slog.Warn("vault: walk failed", "root", root.path, "err", err)
		}
	}

	// Prune rows whose source file no longer exists under the active
	// scope. Drop any doc_tree_refs first so the FK-less schema does
	// not accumulate orphans.
	for p, id := range stale {
		_, _ = s.db.Exec("DELETE FROM doc_tree_refs WHERE doc_id = ?", id)
		if _, err := s.db.Exec(
			"DELETE FROM docs WHERE file_path = ? AND kind = 'vault'", p,
		); err == nil {
			removed++
		}
	}

	slog.Info("vault: annotations ingested",
		"path", vaultPath,
		"scope", scopeForLog(opts.Scope),
		"trees", len(roots),
		"indexed", indexed, "skipped", skipped, "removed", removed)
	return nil
}

// scopeRoot is one filesystem root the walker will descend into. label
// is the trees_of_interest label used to register it.
type scopeRoot struct {
	path  string
	label string
}

// scopeRoots resolves opts into the ordered list of (path, label) pairs
// the walker should visit. An unrecognised scope falls back to "full"
// rather than failing: that matches the v1 behaviour callers depend on
// while still surfacing the typo via the log line on the next ingest.
func scopeRoots(vaultPath string, opts VaultIngestOptions) ([]scopeRoot, error) {
	mnemoDir := filepath.Join(vaultPath, "_mnemo")
	switch opts.Scope {
	case VaultScopeMnemoOnly:
		if fi, err := os.Stat(mnemoDir); err != nil || !fi.IsDir() {
			// Strictest scope on a vault without the wing yet:
			// nothing to walk, but we still want the prune pass to
			// run so any stale rows from a previous wider scope get
			// cleaned up. An empty root list is fine — the walker
			// loop is a no-op.
			return nil, nil
		}
		return []scopeRoot{{path: mnemoDir, label: vaultTreeLabelPrefix + "_mnemo"}}, nil
	case VaultScopeIncludes:
		roots := []scopeRoot{}
		if fi, err := os.Stat(mnemoDir); err == nil && fi.IsDir() {
			roots = append(roots, scopeRoot{path: mnemoDir, label: vaultTreeLabelPrefix + "_mnemo"})
		}
		seen := map[string]bool{mnemoDir: true}
		for _, inc := range opts.Includes {
			if seen[inc] {
				continue
			}
			if fi, err := os.Stat(inc); err != nil || !fi.IsDir() {
				// Skip missing or non-directory includes silently —
				// they get reported via mnemo_vault_status's
				// (future) include-validity field; failing the whole
				// ingest because one entry was renamed would be a
				// poor reaction to a typo.
				continue
			}
			rel, _ := filepath.Rel(vaultPath, inc)
			roots = append(roots, scopeRoot{
				path:  inc,
				label: vaultTreeLabelPrefix + "include:" + filepath.ToSlash(rel),
			})
			seen[inc] = true
		}
		return roots, nil
	case VaultScopeFull, "":
		return []scopeRoot{{path: vaultPath, label: vaultTreeLabelPrefix + "root"}}, nil
	default:
		return []scopeRoot{{path: vaultPath, label: vaultTreeLabelPrefix + "root"}}, nil
	}
}

func scopeForLog(s string) string {
	if s == "" {
		return VaultScopeFull
	}
	return s
}

// pruneOrphanedVaultTreesLocked deletes trees_of_interest rows
// previously registered by the vault ingest path whose root_path is no
// longer in the active scope. Caller must hold s.rwmu write lock.
func (s *Store) pruneOrphanedVaultTreesLocked(keep map[string]int64) error {
	rows, err := s.db.Query(
		`SELECT id, root_path FROM trees_of_interest WHERE label LIKE ?`,
		vaultTreeLabelPrefix+"%",
	)
	if err != nil {
		return err
	}
	type row struct {
		id   int64
		path string
	}
	var orphans []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.path); err != nil {
			continue
		}
		if _, kept := keep[r.path]; !kept {
			orphans = append(orphans, r)
		}
	}
	rows.Close()
	for _, o := range orphans {
		if _, err := s.db.Exec(`DELETE FROM doc_tree_refs WHERE tree_id = ?`, o.id); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM trees_of_interest WHERE id = ?`, o.id); err != nil {
			return err
		}
	}
	return nil
}
