// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SourceDriftEntry is one indexed transcript source that has drifted
// from what was ingested.
type SourceDriftEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"` // "deleted" | "truncated"
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"` // -1 when the file is gone
}

// SourceDriftReport summarises lost-source state across the indexed
// transcript files (🎯T68.6).
type SourceDriftReport struct {
	Deleted   int                `json:"deleted"`
	Truncated int                `json:"truncated"`
	Examples  []SourceDriftEntry `json:"examples"`
}

// SourceDrift detects indexed transcript sources that have been pruned
// or truncated out from under the index (🎯T68.6). It is read-only and
// touches no ingest state — detection uses the already-recorded ingest
// offset as a high-water mark:
//
//   - the file no longer exists            → "deleted"
//   - the file's current size < the offset → "truncated" (pruned, or
//     rewritten to something shorter)
//
// Under the durable-tier model this is NOT an error: Claude Code prunes
// transcripts, and the index is the authoritative durable copy of that
// content (backups per 🎯T61 are the reconcile-from-cold path). The
// report exists so operators — and the orphan GC — can see how much
// indexed content no longer has a live source, NOT so the rows get
// deleted. A same-size in-place rewrite is not detected here; that
// needs a stored content fingerprint and is deferred (see
// docs/design/convergence-source-tier-gc.md).
func (s *Store) SourceDrift() SourceDriftReport {
	s.mu.Lock()
	offsets := make(map[string]int64, len(s.offsets))
	for p, o := range s.offsets {
		offsets[p] = o
	}
	s.mu.Unlock()

	keys := make([]string, 0, len(offsets))
	for p := range offsets {
		keys = append(keys, p)
	}
	sort.Strings(keys)

	const maxExamples = 20
	var rep SourceDriftReport
	for _, p := range keys {
		off := offsets[p]
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				rep.Deleted++
				if len(rep.Examples) < maxExamples {
					rep.Examples = append(rep.Examples,
						SourceDriftEntry{Path: p, Kind: "deleted", Offset: off, Size: -1})
				}
			}
			continue
		}
		if info.Size() < off {
			rep.Truncated++
			if len(rep.Examples) < maxExamples {
				rep.Examples = append(rep.Examples,
					SourceDriftEntry{Path: p, Kind: "truncated", Offset: off, Size: info.Size()})
			}
		}
	}
	return rep
}

// ReconcileSourceState propagates the current state of each indexed
// transcript source onto session_meta as a valid-time tag — the Law 2
// (state convergence) reconciler of 🎯T68.6's bitemporal model. It
// never removes rows; it only writes the tag.
//
// For each indexed source path:
//   - file gone                → tag the session "deleted_at=<now>"
//   - size < ingested offset   → tag the session "truncated_at=<now>"
//   - otherwise                → no-op (session stays "live")
//
// Idempotent within a status class: a session already tagged
// "deleted_at=…" is not re-tagged just because the file is still gone;
// only a status-class transition (e.g. live → deleted, truncated →
// deleted) writes. Re-livening (deleted → live when a source returns)
// is intentionally NOT modelled here — a tag is informational and a
// once-pruned source coming back is rare; conservative defer.
//
// Maps source path → session_id via the Claude Code convention
// (basename without ".jsonl"). Paths that don't match the convention
// are skipped.
//
// Returns the number of sessions whose tag was newly written or
// transitioned class.
func (s *Store) ReconcileSourceState(now time.Time) (int, error) {
	s.mu.Lock()
	offsets := make(map[string]int64, len(s.offsets))
	for p, o := range s.offsets {
		offsets[p] = o
	}
	s.mu.Unlock()

	nowStr := now.UTC().Format(time.RFC3339)
	type tagUpdate struct {
		sessionID string
		status    string
	}
	var candidates []tagUpdate
	for path, off := range offsets {
		var newStatus string
		info, err := os.Stat(path)
		switch {
		case os.IsNotExist(err):
			newStatus = "deleted_at=" + nowStr
		case err != nil:
			continue
		case info.Size() < off:
			newStatus = "truncated_at=" + nowStr
		default:
			continue // live; no tag change
		}
		sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if sessionID == "" || sessionID == filepath.Base(path) {
			continue // didn't match the .jsonl convention
		}
		candidates = append(candidates, tagUpdate{sessionID, newStatus})
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	s.rwmu.Lock()
	defer s.rwmu.Unlock()
	tagged := 0
	for _, c := range candidates {
		var current string
		_ = s.db.QueryRow(
			`SELECT source_status FROM session_meta WHERE session_id = ?`,
			c.sessionID).Scan(&current)
		if sourceStatusClass(current) == sourceStatusClass(c.status) {
			continue // idempotent within class
		}
		// UPSERT: session_meta only gets a row when ingest sees
		// cwd/branch/topic, so a tag may be the first row for a
		// session. The row's other defaults ('' for repo/cwd/etc) are
		// fine — they reflect "unknown" and are populated if ingest
		// later sees the metadata.
		if _, err := s.db.Exec(`
			INSERT INTO session_meta (session_id, source_status, source_state_at)
			VALUES (?, ?, ?)
			ON CONFLICT(session_id) DO UPDATE SET
				source_status = excluded.source_status,
				source_state_at = excluded.source_state_at
		`, c.sessionID, c.status, nowStr); err != nil {
			return tagged, err
		}
		tagged++
	}
	return tagged, nil
}

// sourceStatusClass returns the status class — the portion before the
// "=" timestamp. "live" / "" stay as-is; "deleted_at=…" → "deleted_at";
// "truncated_at=…" → "truncated_at".
func sourceStatusClass(s string) string {
	if i := strings.Index(s, "="); i >= 0 {
		return s[:i]
	}
	return s
}
