// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func gitShow(cwd, rev, rel string) ([]byte, string, error) {
	cmd := exec.Command("git", "-C", cwd, "show", rev+":"+filepath.ToSlash(rel))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", err
	}
	return stdout.Bytes(), rev, nil
}

func seedFromGitBefore(cwd, abs string, t0 time.Time) *Seed {
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	rel = filepath.ToSlash(rel)
	rev := "HEAD"
	if !t0.IsZero() {
		cmd := exec.Command("git", "-C", cwd, "rev-list", "-1", "--before="+t0.UTC().Format(time.RFC3339), "HEAD")
		out, err := cmd.Output()
		if err == nil {
			if r := strings.TrimSpace(string(out)); r != "" {
				rev = r
			}
		}
	}
	body, used, err := gitShow(cwd, rev, rel)
	if err != nil || len(body) == 0 || containsNUL(body) {
		return nil
	}
	return &Seed{Body: body, Source: SeedGitCommit, Captured: t0, Detail: used + ":" + rel}
}

func seedFromGitStash(cwd, abs string, t0 time.Time) *Seed {
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	rel = filepath.ToSlash(rel)
	cmd := exec.Command("git", "-C", cwd, "stash", "list", "--date=iso-strict")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	// stash@{0}: WIP on branch: ...
	// With --date=iso-strict: stash@{0}: On branch: msg (may include date in different formats)
	// Prefer: git stash list --format='%gd %ci %s'
	cmd = exec.Command("git", "-C", cwd, "stash", "list", "--format=%gd%x09%cI%x09%s")
	out, err = cmd.Output()
	if err != nil {
		return nil
	}
	type entry struct {
		ref string
		ts  time.Time
	}
	var entries []entry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			continue
		}
		entries = append(entries, entry{ref: parts[0], ts: ts})
	}
	// Prefer newest stash at or before t0; else newest stash overall.
	var pick *entry
	for i := range entries {
		e := &entries[i]
		if !t0.IsZero() && e.ts.After(t0) {
			continue
		}
		if pick == nil || e.ts.After(pick.ts) {
			pick = e
		}
	}
	if pick == nil && len(entries) > 0 {
		pick = &entries[0] // stash list is newest-first
	}
	if pick == nil {
		return nil
	}
	body, _, err := gitShow(cwd, pick.ref, rel)
	if err != nil || len(body) == 0 {
		return nil
	}
	return &Seed{Body: body, Source: SeedGitStash, Captured: pick.ts, Detail: pick.ref + ":" + rel}
}

func seedFromGrokRewind(grokHome string, ops []Op, abs string, t0 time.Time) *Seed {
	sid := sessionForPath(ops, abs)
	if sid == "" {
		return nil
	}
	cwd := cwdForPath(ops, abs)
	dir := findGrokSessionDir(grokHome, cwd, sid)
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, "rewind_points.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	rel := abs
	if cwd != "" {
		if r, err := filepath.Rel(cwd, abs); err == nil {
			rel = filepath.ToSlash(r)
		}
	}
	// Also try ui/-style and absolute keys as stored
	candidates := []string{rel, abs, filepath.Base(abs)}

	var best *Seed
	dec := jsonDecoder(f)
	for {
		var o struct {
			CreatedAt      string                `json:"created_at"`
			FileSnapshots  map[string]rewindSnap `json:"file_snapshots"`
			AfterSnapshots map[string]rewindSnap `json:"after_snapshots"`
		}
		if err := dec.Decode(&o); err != nil {
			break
		}
		ts, _ := time.Parse(time.RFC3339Nano, o.CreatedAt)
		for _, snap := range []map[string]rewindSnap{o.FileSnapshots, o.AfterSnapshots} {
			for key, v := range snap {
				if !rewindPathMatch(key, candidates) {
					continue
				}
				body := []byte(v.Content)
				if len(body) == 0 {
					continue
				}
				cap := ts
				if t, err := time.Parse(time.RFC3339Nano, v.CapturedAt); err == nil {
					cap = t
				}
				// Prefer latest snapshot at or before first op; else latest overall.
				s := &Seed{Body: body, Source: SeedGrokRewind, Captured: cap, Detail: "rewind:" + key}
				if best == nil {
					best = s
					continue
				}
				beforeOK := t0.IsZero() || !cap.After(t0)
				bestBefore := t0.IsZero() || !best.Captured.After(t0)
				switch {
				case beforeOK && !bestBefore:
					best = s
				case beforeOK == bestBefore && cap.After(best.Captured):
					best = s
				case beforeOK == bestBefore && cap.Equal(best.Captured) && len(body) > len(best.Body):
					best = s
				}
			}
		}
	}
	return best
}

type rewindSnap struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	CapturedAt string `json:"captured_at"`
}

func rewindPathMatch(key string, candidates []string) bool {
	key = filepath.ToSlash(key)
	for _, c := range candidates {
		c = filepath.ToSlash(c)
		if key == c || strings.HasSuffix(key, "/"+c) || strings.HasSuffix(c, "/"+key) {
			return true
		}
		if filepath.Base(key) == filepath.Base(c) && (strings.Contains(key, "ui/") && strings.Contains(c, "ui/")) {
			return true
		}
	}
	return false
}

func findGrokSessionDir(grokHome, cwd, sessionID string) string {
	if grokHome == "" {
		grokHome = os.Getenv("GROK_HOME")
	}
	if grokHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		grokHome = filepath.Join(home, ".grok")
	}
	sessions := filepath.Join(grokHome, "sessions")
	// Common layout: sessions/<urlencode(cwd)>/<sessionID>/
	var hits []string
	_ = filepath.WalkDir(sessions, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == sessionID {
			hits = append(hits, path)
			return filepath.SkipDir
		}
		return nil
	})
	if len(hits) == 0 {
		return ""
	}
	if cwd == "" {
		return hits[0]
	}
	enc := filepath.Base(filepath.Dir(hits[0]))
	for _, h := range hits {
		if strings.Contains(filepath.Base(filepath.Dir(h)), "jevons") || strings.Contains(h, cwd) {
			return h
		}
		_ = enc
	}
	return hits[0]
}
