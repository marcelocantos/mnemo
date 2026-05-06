// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DocInfo is a single indexed documentation file. Synthesis-specific
// fields (Taxonomy, DocDate, DocStatus, DocTarget, DocSource) are
// populated when the file_path matches the synthesis-doc layout (see
// classifyTaxonomy); otherwise they are empty strings.
type DocInfo struct {
	ID          int64  `json:"id"`
	Repo        string `json:"repo"`
	FilePath    string `json:"file_path"`
	Kind        string `json:"kind"` // md, txt, pdf
	Title       string `json:"title"`
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	Size        int64  `json:"size"`
	MTime       string `json:"mtime"`
	IndexedAt   string `json:"indexed_at"`
	Taxonomy    string `json:"taxonomy,omitempty"`
	DocDate     string `json:"doc_date,omitempty"`
	DocStatus   string `json:"doc_status,omitempty"`
	DocTarget   string `json:"doc_target,omitempty"`
	DocSource   string `json:"doc_source,omitempty"`
}

// docExcludeDirs are directory names to skip entirely during the doc walk.
var docExcludeDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"build":        true,
	".build":       true,
	"target":       true,
	"vendor":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".next":        true,
	".output":      true,
	".cache":       true,
	"coverage":     true,
	"tmp":          true,
}

// pdfExtractOnce guards the one-time tool detection for PDF extraction.
var pdfExtractOnce sync.Once
var pdfExtractCmd string // empty = no PDF tool found

func detectPDFTool() string {
	for _, tool := range []string{"pdftotext", "mutool"} {
		if p, err := exec.LookPath(tool); err == nil {
			slog.Info("PDF extraction tool found", "tool", p)
			return tool
		}
	}
	slog.Warn("no PDF extraction tool found (pdftotext or mutool); PDFs will be skipped")
	return ""
}

// extractPDFText extracts plain text from a PDF file using the best available tool.
// Returns "" and an error if extraction fails or no tool is available.
func extractPDFText(path string) (string, error) {
	pdfExtractOnce.Do(func() {
		pdfExtractCmd = detectPDFTool()
	})
	if pdfExtractCmd == "" {
		return "", fmt.Errorf("no PDF extraction tool available")
	}

	var cmd *exec.Cmd
	switch pdfExtractCmd {
	case "pdftotext":
		// pdftotext -q -nopgbrk <file> - (stdout)
		cmd = exec.Command("pdftotext", "-q", "-nopgbrk", path, "-")
	case "mutool":
		// mutool draw -F txt <file>
		cmd = exec.Command("mutool", "draw", "-F", "txt", path)
	default:
		return "", fmt.Errorf("unknown PDF tool: %s", pdfExtractCmd)
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", pdfExtractCmd, err)
	}
	return out.String(), nil
}

// contentHash returns the hex SHA-256 of data.
func contentHash(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}

// extractTitle returns the first non-empty line of a markdown/text file as
// a title, stripping leading '#' characters.
func extractTitle(content string) string {
	for _, line := range strings.SplitN(content, "\n", 20) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Strip leading markdown heading markers.
		line = strings.TrimLeft(line, "#")
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// parseGitignorePatterns reads a .gitignore file and returns a list of
// simple patterns. Only prefix/suffix matching is implemented — the full
// gitignore spec is complex; this covers the 95% case for documentation
// discovery (ignoring generated dirs, minified files, etc.).
func parseGitignorePatterns(gitignorePath string) []string {
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Negation patterns are ignored (we're conservative: may over-skip).
		if strings.HasPrefix(line, "!") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// matchesGitignore reports whether name or relPath matches any of the
// gitignore patterns. This is a simplified match: supports trailing /**,
// directory-only patterns (trailing /), and plain name/glob matching.
func matchesGitignore(patterns []string, name, relPath string) bool {
	for _, pat := range patterns {
		// Directory-only pattern.
		isDirPat := strings.HasSuffix(pat, "/")
		if isDirPat {
			pat = strings.TrimSuffix(pat, "/")
		}
		// Match against the bare name or the relative path.
		// Simple cases: exact name, or "**"-terminated prefix.
		pat = strings.TrimSuffix(pat, "/**")
		if matched, _ := filepath.Match(pat, name); matched {
			return true
		}
		if matched, _ := filepath.Match(pat, relPath); matched {
			return true
		}
		// Also try matching the last component of the pattern against name.
		if base := filepath.Base(pat); base != pat {
			if matched, _ := filepath.Match(base, name); matched {
				return true
			}
		}
	}
	return false
}

// IngestDocs scans documentation files in all known repos and indexes them.
// Markdown > txt > pdf priority; dedup by stem. Incremental via content hash.
func (s *Store) IngestDocs() error {
	s.rwmu.Lock()
	defer s.rwmu.Unlock()

	roots := s.knownRepoRootsLocked()
	indexed, skipped, onDisk := 0, 0, 0
	for _, rr := range roots {
		n, sk, od := s.ingestDocsForRepoLocked(rr.root, rr.repo)
		indexed += n
		skipped += sk
		onDisk += od
	}
	s.recordBackfillStatusLocked("docs", indexed, onDisk)
	slog.Info("ingested docs", "indexed", indexed, "skipped_unchanged", skipped, "on_disk", onDisk)
	return nil
}

// ingestDocsForRepoLocked indexes doc files for a single repo. Walks the
// entire repo tree from repoRoot, picking up every .md/.txt/.pdf file
// outside the junk-dir skip list and .gitignore matches. The same filter
// applies uniformly at every depth, including the repo root itself.
// Returns (indexed, skipped_unchanged, on_disk).
func (s *Store) ingestDocsForRepoLocked(repoRoot, repo string) (indexed, skipped, onDisk int) {
	gitignorePatterns := parseGitignorePatterns(filepath.Join(repoRoot, ".gitignore"))
	seen := map[string]bool{} // avoid double-indexing a path

	// collectDir gathers doc candidates from one directory (non-recursive),
	// deduplicates by stem within that directory, and ingests each winner.
	collectDir := func(dirPath string) {
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return
		}
		byStem := map[string][]stemCandidate{}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".md" && ext != ".txt" && ext != ".pdf" {
				continue
			}
			fp := filepath.Join(dirPath, name)
			relFromRepo, _ := filepath.Rel(repoRoot, fp)
			if matchesGitignore(gitignorePatterns, name, relFromRepo) {
				continue
			}
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			byStem[stem] = append(byStem[stem], stemCandidate{ext: ext, path: fp})
		}
		for _, candidates := range byStem {
			selected := selectDoc(candidates)
			if selected == "" || seen[selected] {
				continue
			}
			seen[selected] = true
			onDisk++
			n, sk := s.ingestDocFileLocked(selected, repo)
			indexed += n
			skipped += sk
		}
	}

	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != repoRoot {
			name := d.Name()
			relFromRepo, _ := filepath.Rel(repoRoot, path)
			if docExcludeDirs[name] || matchesGitignore(gitignorePatterns, name, relFromRepo) {
				return filepath.SkipDir
			}
		}
		collectDir(path)
		return nil
	})
	return
}

// stemCandidate holds a file extension and path for stem-based dedup.
type stemCandidate struct {
	ext  string
	path string
}

// selectDoc picks the best file from a group of candidates with the same stem.
// Priority: .md > .txt > .pdf
func selectDoc(candidates []stemCandidate) string {
	priority := map[string]int{".md": 3, ".txt": 2, ".pdf": 1}
	best := ""
	bestPri := 0
	for _, c := range candidates {
		if p := priority[c.ext]; p > bestPri {
			bestPri = p
			best = c.path
		}
	}
	return best
}

// ingestDocFileLocked ingests a single doc file. Returns (1,0) if indexed,
// (0,1) if skipped (unchanged), (0,0) on error.
// Caller must hold rwmu write lock.
func (s *Store) ingestDocFileLocked(path, repo string) (indexed, skipped int) {
	ext := strings.ToLower(filepath.Ext(path))

	var rawBytes []byte
	var content string
	var err error

	if ext == ".pdf" {
		content, err = extractPDFText(path)
		if err != nil {
			slog.Warn("pdf text extraction failed", "file", path, "err", err)
			return
		}
		// Use the file bytes for the hash.
		rawBytes, err = os.ReadFile(path)
		if err != nil {
			slog.Warn("read pdf failed", "file", path, "err", err)
			return
		}
	} else {
		rawBytes, err = os.ReadFile(path)
		if err != nil {
			slog.Warn("read doc file failed", "file", path, "err", err)
			return
		}
		// Skip non-UTF-8 (binary) files.
		if !utf8.Valid(rawBytes) {
			return
		}
		content = string(rawBytes)
	}

	// Skip empty content.
	if strings.TrimSpace(content) == "" {
		return
	}

	hash := contentHash(rawBytes)

	// Check if already indexed with same hash.
	var existingHash string
	_ = s.db.QueryRow("SELECT content_hash FROM docs WHERE file_path = ?", path).Scan(&existingHash)
	if existingHash == hash {
		skipped = 1
		return
	}

	fi, err := os.Stat(path)
	if err != nil {
		return
	}

	kind := strings.TrimPrefix(ext, ".")
	title := extractTitle(content)
	now := time.Now().Format(time.RFC3339)
	mtime := fi.ModTime().Format(time.RFC3339)

	// Synthesis-doc metadata: taxonomy inferred from path, inline fields
	// parsed from the first lines of the content. Non-matching paths get
	// empty strings and are unaffected by mnemo_synthesis queries.
	taxonomy, _ := classifyTaxonomy(path)
	var meta inlineMetadata
	if taxonomy != "" && ext == ".md" {
		meta = parseInlineMetadata(content)
	}

	_, err = s.db.Exec(`
		INSERT INTO docs (repo, file_path, kind, title, content, content_hash,
			size, mtime, indexed_at, taxonomy, doc_date, doc_status, doc_target, doc_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET
			repo         = excluded.repo,
			kind         = excluded.kind,
			title        = excluded.title,
			content      = excluded.content,
			content_hash = excluded.content_hash,
			size         = excluded.size,
			mtime        = excluded.mtime,
			indexed_at   = excluded.indexed_at,
			taxonomy     = excluded.taxonomy,
			doc_date     = excluded.doc_date,
			doc_status   = excluded.doc_status,
			doc_target   = excluded.doc_target,
			doc_source   = excluded.doc_source
	`, repo, path, kind, title, content, hash, fi.Size(), mtime, now,
		taxonomy, meta.Date, meta.Status, meta.Target, meta.Source)
	if err != nil {
		slog.Error("insert doc failed", "file", path, "err", err)
		return
	}
	indexed = 1
	return
}

// SearchDocs searches across indexed documentation files.
func (s *Store) SearchDocs(query string, repo string, kind string, limit int) ([]DocInfo, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if limit <= 0 {
		limit = 20
	}

	var where []string
	var args []any

	if query != "" {
		ftsQuery := relaxQuery(query)
		q := `SELECT d.id, d.repo, d.file_path, d.kind, d.title, d.content, d.content_hash, d.size, d.mtime, d.indexed_at
			FROM docs d
			JOIN docs_fts f ON f.rowid = d.id
			WHERE docs_fts MATCH ?`
		args = []any{ftsQuery}
		if repo != "" {
			q += " AND d.repo LIKE ?"
			args = append(args, "%"+repo+"%")
		}
		if kind != "" {
			q += " AND d.kind = ?"
			args = append(args, kind)
		}
		q += " ORDER BY rank LIMIT ?"
		args = append(args, limit)
		return s.queryDocs(q, args...)
	}

	// No query — list with filters.
	where = append(where, "1=1")
	if repo != "" {
		where = append(where, "repo LIKE ?")
		args = append(args, "%"+repo+"%")
	}
	if kind != "" {
		where = append(where, "kind = ?")
		args = append(args, kind)
	}
	q := `SELECT id, repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at
		FROM docs WHERE ` + strings.Join(where, " AND ") + ` ORDER BY indexed_at DESC LIMIT ?`
	args = append(args, limit)
	return s.queryDocs(q, args...)
}

func (s *Store) queryDocs(q string, args ...any) ([]DocInfo, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []DocInfo
	for rows.Next() {
		var d DocInfo
		if err := rows.Scan(&d.ID, &d.Repo, &d.FilePath, &d.Kind, &d.Title,
			&d.Content, &d.ContentHash, &d.Size, &d.MTime, &d.IndexedAt); err != nil {
			continue
		}
		results = append(results, d)
	}
	return results, nil
}
