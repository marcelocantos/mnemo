// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var readChunkRe = regexp.MustCompile(`(?m)^(\d+)→`)

// ResolveSeeds builds pre-images for every absolute path touched by ops.
// Priority (first hit wins per path): --seed-from, worktree, git commit-at-t0,
// git stash-by-time, Grok rewind, Claude file-history, stitched Read results.
func ResolveSeeds(db *sql.DB, ops []Op, cfg SeedConfig) ([]Seed, []string) {
	if cfg.DisableAll || len(ops) == 0 {
		return nil, nil
	}
	paths := uniqueAbsPaths(ops)
	firstTS := firstTimestampPerPath(ops)
	var seeds []Seed
	var warns []string
	seen := make(map[string]struct{})

	try := func(path string, s *Seed, errMsg string) {
		if s == nil || len(s.Body) == 0 {
			if errMsg != "" {
				warns = append(warns, errMsg)
			}
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		s.AbsPath = path
		seeds = append(seeds, *s)
	}

	for _, path := range paths {
		t0 := firstTS[path]

		if cfg.SeedFrom != "" {
			if s, err := seedFromExplicit(cfg.SeedFrom, path, ops[0].CWD); err == nil {
				try(path, s, "")
				continue
			}
		}
		if cfg.enabled(cfg.UseWorkTree) {
			if s := seedFromWorkTree(path, t0); s != nil {
				try(path, s, "")
				continue
			}
		}
		cwd := cwdForPath(ops, path)
		if cfg.enabled(cfg.UseGitCommit) && cwd != "" {
			if s := seedFromGitBefore(cwd, path, t0); s != nil {
				try(path, s, "")
				continue
			}
		}
		if cfg.enabled(cfg.UseGitStash) && cwd != "" {
			if s := seedFromGitStash(cwd, path, t0); s != nil {
				try(path, s, "")
				continue
			}
		}
		if cfg.enabled(cfg.UseRewind) {
			if s := seedFromGrokRewind(cfg.GrokHome, ops, path, t0); s != nil {
				try(path, s, "")
				continue
			}
		}
		if cfg.enabled(cfg.UseFileHist) {
			sid := sessionForPath(ops, path)
			home := cfg.ClaudeHome
			if home == "" {
				home = ClaudeHome()
			}
			if body, ok := lookupFileHistory(home, sid, filepath.Base(path)); ok {
				try(path, &Seed{Body: body, Source: SeedClaudeHistory, Captured: t0, Detail: sid}, "")
				continue
			}
		}
		if cfg.enabled(cfg.UseReadResults) && db != nil {
			if s := seedFromReadResults(db, ops, path, t0); s != nil {
				try(path, s, "")
				continue
			}
		}
	}
	return seeds, warns
}

func uniqueAbsPaths(ops []Op) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, op := range ops {
		p := absPath(op)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func absPath(op Op) string {
	p := op.Path
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if op.CWD == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(op.CWD, p))
}

func firstTimestampPerPath(ops []Op) map[string]time.Time {
	out := make(map[string]time.Time)
	for _, op := range ops {
		p := absPath(op)
		if p == "" {
			continue
		}
		if t, ok := out[p]; !ok || op.Timestamp.Before(t) {
			out[p] = op.Timestamp
		}
	}
	return out
}

func cwdForPath(ops []Op, path string) string {
	for _, op := range ops {
		if absPath(op) == path && op.CWD != "" {
			return op.CWD
		}
	}
	return ""
}

func sessionForPath(ops []Op, path string) string {
	for _, op := range ops {
		if absPath(op) == path && op.SessionID != "" {
			return op.SessionID
		}
	}
	return ""
}

func seedFromWorkTree(path string, t0 time.Time) *Seed {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return nil
	}
	// Prefer files not clearly newer than first op (post-loss rebuilds).
	if !t0.IsZero() && fi.ModTime().After(t0.Add(time.Hour)) {
		return nil
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 || containsNUL(body) {
		return nil
	}
	return &Seed{Body: body, Source: SeedWorkTree, Captured: fi.ModTime(), Detail: path}
}

func seedFromExplicit(ref, abs, cwd string) (*Seed, error) {
	// Directory seed
	if fi, err := os.Stat(ref); err == nil && fi.IsDir() {
		// try abs path under dir via basename or rel
		candidates := []string{
			filepath.Join(ref, filepath.Base(abs)),
			abs,
		}
		if cwd != "" {
			if rel, err := filepath.Rel(cwd, abs); err == nil {
				candidates = append([]string{filepath.Join(ref, rel)}, candidates...)
			}
		}
		for _, c := range candidates {
			if body, err := os.ReadFile(c); err == nil && len(body) > 0 {
				return &Seed{Body: body, Source: SeedCLI, Captured: time.Now(), Detail: c}, nil
			}
		}
	}
	// Git rev or stash: use cwd
	if cwd != "" {
		rel := abs
		if r, err := filepath.Rel(cwd, abs); err == nil {
			rel = r
		}
		if body, rev, err := gitShow(cwd, ref, rel); err == nil && len(body) > 0 {
			return &Seed{Body: body, Source: SeedCLI, Detail: rev + ":" + rel}, nil
		}
	}
	return nil, os.ErrNotExist
}

func seedFromReadResults(db *sql.DB, ops []Op, path string, t0 time.Time) *Seed {
	sessions := map[string]struct{}{}
	for _, op := range ops {
		if absPath(op) == path && op.SessionID != "" {
			sessions[op.SessionID] = struct{}{}
		}
	}
	if len(sessions) == 0 {
		return nil
	}
	var best *Seed
	for sid := range sessions {
		chunks, err := loadReadChunks(db, sid, path)
		if err != nil || len(chunks) == 0 {
			continue
		}
		body := stitchReadChunks(chunks)
		if len(body) == 0 {
			continue
		}
		captured := t0
		for _, c := range chunks {
			if c.ts.After(captured) {
				captured = c.ts
			}
		}
		// Prefer seed captured at or before first patch when possible;
		// still accept later reads (post-image) as better than nothing.
		s := &Seed{Body: body, Source: SeedReadResult, Captured: captured, Detail: "stitched read_file"}
		if best == nil || (!t0.IsZero() && !captured.After(t0) && best.Captured.After(t0)) || len(body) > len(best.Body) {
			best = s
		}
	}
	return best
}

type readChunk struct {
	offset int // line or byte marker from provider
	text   string
	ts     time.Time
}

func loadReadChunks(db *sql.DB, sessionID, absPath string) ([]readChunk, error) {
	textExpr := "COALESCE(tr.text, '')"
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'text_z'`).Scan(&n); err == nil && n > 0 {
		textExpr = "COALESCE(mnemo_text(tr.text, tr.text_z), '')"
	}
	rows, err := db.Query(fmt.Sprintf(`
SELECT m.timestamp, %s
FROM messages m
JOIN messages tr ON tr.session_id = m.session_id
  AND tr.tool_use_id = m.tool_use_id AND tr.content_type = 'tool_result'
WHERE m.session_id = ?
  AND m.content_type = 'tool_use'
  AND m.tool_name IN ('read_file', 'Read', 'read')
  AND m.tool_file_path = ?
  AND tr.is_error = 0
ORDER BY m.timestamp`, textExpr), sessionID, absPath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []readChunk
	for rows.Next() {
		var ts, text string
		if err := rows.Scan(&ts, &text); err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		if text == "" || looksLikeToolAckJSON(text) {
			continue
		}
		t, _ := time.Parse(time.RFC3339Nano, ts)
		if t.IsZero() {
			t, _ = time.Parse(time.RFC3339, ts)
		}
		out = append(out, parseReadResult(text, t)...)
	}
	return out, rows.Err()
}

func looksLikeToolAckJSON(text string) bool {
	if !strings.HasPrefix(text, "{") {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return false
	}
	_, hasType := m["type"]
	_, hasEdits := m["EditsApplied"]
	return hasType || hasEdits
}

func parseReadResult(text string, ts time.Time) []readChunk {
	if readChunkRe.MatchString(text) {
		var chunks []readChunk
		idxs := readChunkRe.FindAllStringSubmatchIndex(text, -1)
		for i, loc := range idxs {
			off, _ := strconv.Atoi(text[loc[2]:loc[3]])
			start := loc[1] // after "N→"
			end := len(text)
			if i+1 < len(idxs) {
				end = idxs[i+1][0]
			}
			chunks = append(chunks, readChunk{offset: off, text: strings.TrimSuffix(text[start:end], "\n"), ts: ts})
		}
		return chunks
	}
	// Whole-file read
	return []readChunk{{offset: 1, text: text, ts: ts}}
}

// stitchReadChunks merges offset-labelled slices into one body.
// Offsets are treated as 1-based line numbers (Grok read_file convention).
func stitchReadChunks(chunks []readChunk) []byte {
	if len(chunks) == 0 {
		return nil
	}
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].offset != chunks[j].offset {
			return chunks[i].offset < chunks[j].offset
		}
		return chunks[i].ts.Before(chunks[j].ts)
	})
	// If single whole-file chunk (offset 1 and no other markers), return latest fat one.
	lines := map[int]string{}
	for _, c := range chunks {
		parts := strings.Split(c.text, "\n")
		for i, line := range parts {
			ln := c.offset + i
			lines[ln] = line
		}
	}
	if len(lines) == 0 {
		return nil
	}
	max := 0
	for ln := range lines {
		if ln > max {
			max = ln
		}
	}
	var b strings.Builder
	for i := 1; i <= max; i++ {
		if s, ok := lines[i]; ok {
			b.WriteString(s)
		}
		if i < max {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// SeedsToQuarantine maps seeds into quarantine-relative keys for engine preload.
func SeedsToQuarantine(seeds []Seed, ops []Op) map[string][]byte {
	out := make(map[string][]byte)
	for _, s := range seeds {
		cwd, repo := cwdForPath(ops, s.AbsPath), ""
		for _, op := range ops {
			if absPath(op) == s.AbsPath && op.Repo != "" {
				repo = op.Repo
				break
			}
		}
		if cwd == "" {
			for _, op := range ops {
				if op.CWD != "" {
					cwd = op.CWD
					break
				}
			}
		}
		if repo == "" {
			for _, op := range ops {
				if op.Repo != "" {
					repo = op.Repo
					break
				}
			}
		}
		key, ok := ResolveKey(s.AbsPath, cwd, repo)
		if !ok {
			continue
		}
		out[key] = append([]byte(nil), s.Body...)
	}
	return out
}
