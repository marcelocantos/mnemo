// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Synthesis docs are analysis/research/design/planning artifacts that
// follow a four-dir taxonomy under docs/ (see the "Synthesis documents"
// section of ~/.claude/CLAUDE.md). mnemo indexes them alongside
// ordinary docs, populating the taxonomy column so they can be queried
// independently via mnemo_synthesis.
//
// Taxonomy values:
//
//	paper              — docs/papers/**/*.md  (debugging & research papers)
//	design             — docs/design/**/*.md  (architectural decisions)
//	analysis           — docs/analysis/**/*.md (summaries of external material)
//	plans              — docs/plans/**/*.md   (forward-looking work pinboards)
//	audit-log          — docs/audit-log.md    (maintenance chronicle)
//	convergence-report — docs/convergence-report.md (target-cycle snapshot)

// synthesisDirs maps a taxonomy subdirectory name to its taxonomy tag.
var synthesisDirs = map[string]string{
	"papers":   "paper",
	"design":   "design",
	"analysis": "analysis",
	"plans":    "plans",
}

// synthesisSingletons maps a docs/-relative filename to its taxonomy tag
// for the top-level snapshot artifacts that are not themselves under a
// taxonomy subdirectory.
var synthesisSingletons = map[string]string{
	"audit-log.md":          "audit-log",
	"convergence-report.md": "convergence-report",
}

// synthesisJunkDirs are directory names the synthesis walker skips
// entirely. These are never valid synthesis-doc containers and scanning
// them is expensive.
var synthesisJunkDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"target":       true,
	"build":        true,
	".build":       true,
	"dist":         true,
	".next":        true,
	".output":      true,
	".cache":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
}

// classifyTaxonomy inspects an absolute file path and returns the
// taxonomy tag it belongs to (paper, design, analysis, plans,
// audit-log, convergence-report) and true if the path matches the
// synthesis-doc layout. Paths that do not match return ("", false) —
// callers should leave the taxonomy column empty in that case.
//
// The taxonomy is determined by walking from the filename up: the
// closest ancestor directory named "docs" anchors the match, and:
//   - docs/papers/**/*.md        -> "paper"
//   - docs/design/**/*.md        -> "design"
//   - docs/analysis/**/*.md      -> "analysis"
//   - docs/plans/**/*.md         -> "plans"
//   - docs/audit-log.md          -> "audit-log"
//   - docs/convergence-report.md -> "convergence-report"
//
// Non-markdown files are rejected. The check is purely syntactic; no
// filesystem access.
func classifyTaxonomy(path string) (string, bool) {
	if strings.ToLower(filepath.Ext(path)) != ".md" {
		return "", false
	}
	// Split into components and find the rightmost "docs" directory.
	parts := strings.Split(filepath.ToSlash(path), "/")
	docsIdx := -1
	for i := len(parts) - 2; i >= 0; i-- { // -2: skip the filename itself
		if parts[i] == "docs" {
			docsIdx = i
			break
		}
	}
	if docsIdx < 0 {
		return "", false
	}
	rel := parts[docsIdx+1:] // everything after "docs/"
	// Singleton at docs/ root: docs/audit-log.md, docs/convergence-report.md.
	if len(rel) == 1 {
		if tag, ok := synthesisSingletons[rel[0]]; ok {
			return tag, true
		}
		return "", false
	}
	// Taxonomy subdir: docs/<dir>/**/*.md.
	if tag, ok := synthesisDirs[rel[0]]; ok {
		return tag, true
	}
	return "", false
}

// inlineMetadata holds the small set of front-of-file fields that
// synthesis docs may declare (see ~/.claude/CLAUDE.md). All fields are
// optional; the zero value is valid.
type inlineMetadata struct {
	Date   string // "2026-04-24"
	Status string // "draft" | "stable" | "superseded"
	Target string // "🎯T34"
	Source string // URL or citation (required for analysis/ docs by convention)
}

// parseInlineMetadata scans the first metadataScanLines of a markdown
// document for "Field: value" lines near the top. Stops at the first
// blank line that follows at least one recognised field, or at the
// scan-line limit. Unrecognised fields are ignored.
//
// The convention is looser than YAML frontmatter: fields may sit on
// their own lines ("Date: 2026-04-24") or as bold markdown
// ("**Date:** 2026-04-24"). Both forms are accepted.
func parseInlineMetadata(content string) inlineMetadata {
	const metadataScanLines = 30
	var m inlineMetadata
	var sawField bool
	lines := strings.SplitN(content, "\n", metadataScanLines+1)
	if len(lines) > metadataScanLines {
		lines = lines[:metadataScanLines]
	}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		// Strip markdown bold wrappers: "**Date:**", "*Date:*".
		line = strings.TrimPrefix(line, "**")
		line = strings.TrimPrefix(line, "*")
		if line == "" {
			if sawField {
				break
			}
			continue
		}
		// Heading line — tolerate; synthesis docs typically open with one.
		if strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 || colon >= len(line)-1 {
			if sawField {
				break
			}
			continue
		}
		key := strings.TrimSpace(strings.ToLower(strings.TrimSuffix(line[:colon], "**")))
		val := strings.TrimSpace(line[colon+1:])
		// Strip bold markers from either side of the value: "**Date:** 2026"
		// leaves "** 2026" after the colon split; both leading and trailing
		// "**" are noise here.
		val = strings.TrimPrefix(val, "**")
		val = strings.TrimSuffix(val, "**")
		val = strings.TrimSpace(val)
		switch key {
		case "date":
			m.Date = val
			sawField = true
		case "status":
			m.Status = val
			sawField = true
		case "target":
			m.Target = val
			sawField = true
		case "source":
			m.Source = val
			sawField = true
		}
	}
	return m
}

// IngestSynthesis walks each configured SynthesisRoot and indexes any
// file whose path classifies to a taxonomy. Unlike IngestDocs, this
// walker does not require a .git marker — it is suitable for non-repo
// planning spaces such as ~/think. Files already ingested by IngestDocs
// (because they live inside a known repo) are re-indexed idempotently
// via the same file_path upsert path: the taxonomy column is set if it
// wasn't already.
//
// Returns nil if no SynthesisRoots are configured.
func (s *Store) IngestSynthesis() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	if len(s.synthesisRoots) == 0 {
		return nil
	}

	indexed, skipped, onDisk := 0, 0, 0
	for _, root := range s.synthesisRoots {
		n, sk, od := s.ingestSynthesisRootLocked(root)
		indexed += n
		skipped += sk
		onDisk += od
	}
	s.recordBackfillStatusLocked("synthesis", indexed, onDisk)
	slog.Info("ingested synthesis docs",
		"indexed", indexed, "skipped_unchanged", skipped, "on_disk", onDisk)
	return nil
}

// ingestSynthesisRootLocked walks one root and ingests taxonomy-matching
// files. Caller must hold rwmu write lock.
func (s *Store) ingestSynthesisRootLocked(root string) (indexed, skipped, onDisk int) {
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		slog.Warn("synthesis root unavailable", "root", root, "err", err)
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && synthesisJunkDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := classifyTaxonomy(path); !ok {
			return nil
		}
		onDisk++
		repo := synthesisRepoLabel(root, path)
		n, sk := s.ingestDocFileLocked(path, repo)
		indexed += n
		skipped += sk
		return nil
	})
	return
}

// synthesisRepoLabel derives a repo-like label for a synthesis doc so
// it can be filtered in the same way as ordinary docs. For a path
// inside a git repo under ~/work/<host>/<org>/<repo>/..., this is
// "<repo>". For a non-repo path (e.g. under ~/think), it falls back to
// the basename of the root. Callers that need exact provenance use
// file_path directly; the label is a convenience for filtering.
func synthesisRepoLabel(root, path string) string {
	// Prefer the enclosing git repo's directory name, if one exists
	// between root and path.
	dir := filepath.Dir(path)
	for dir != root && dir != "/" && dir != "." {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Base(root)
}

// SearchSynthesis searches the subset of the docs table whose taxonomy
// column is populated. FTS5 is used when query is non-empty; otherwise
// results are listed by indexed_at descending. Optional filters:
//
//   - taxonomy: one of paper|design|analysis|plans|audit-log|convergence-report.
//   - repo:     LIKE match on the repo column.
func (s *Store) SearchSynthesis(query, taxonomy, repo string, limit int) ([]DocInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	baseCols := `d.id, d.repo, d.file_path, d.kind, d.title, d.content,
		d.content_hash, d.size, d.mtime, d.indexed_at,
		d.taxonomy, d.doc_date, d.doc_status, d.doc_target, d.doc_source`

	var args []any

	if query != "" {
		ftsQuery := relaxQuery(query)
		q := `SELECT ` + baseCols + `
			FROM docs d
			JOIN docs_fts f ON f.rowid = d.id
			WHERE docs_fts MATCH ?
			  AND d.taxonomy != ''`
		args = []any{ftsQuery}
		if taxonomy != "" {
			q += " AND d.taxonomy = ?"
			args = append(args, taxonomy)
		}
		if repo != "" {
			q += " AND d.repo LIKE ?"
			args = append(args, "%"+repo+"%")
		}
		q += " ORDER BY rank LIMIT ?"
		args = append(args, limit)
		return s.querySynthesis(q, args...)
	}

	// No query: list newest first.
	q := `SELECT ` + baseCols + `
		FROM docs d
		WHERE d.taxonomy != ''`
	if taxonomy != "" {
		q += " AND d.taxonomy = ?"
		args = append(args, taxonomy)
	}
	if repo != "" {
		q += " AND d.repo LIKE ?"
		args = append(args, "%"+repo+"%")
	}
	q += " ORDER BY d.indexed_at DESC LIMIT ?"
	args = append(args, limit)
	return s.querySynthesis(q, args...)
}

func (s *Store) querySynthesis(q string, args ...any) ([]DocInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []DocInfo
	for rows.Next() {
		var d DocInfo
		var taxonomy, date, status, target, source string
		if err := rows.Scan(
			&d.ID, &d.Repo, &d.FilePath, &d.Kind, &d.Title, &d.Content,
			&d.ContentHash, &d.Size, &d.MTime, &d.IndexedAt,
			&taxonomy, &date, &status, &target, &source,
		); err != nil {
			continue
		}
		d.Taxonomy = taxonomy
		d.DocDate = date
		d.DocStatus = status
		d.DocTarget = target
		d.DocSource = source
		results = append(results, d)
	}
	return results, nil
}
