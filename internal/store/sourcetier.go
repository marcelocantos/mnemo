// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
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
	Kind   string `json:"kind"` // "deleted" | "truncated" | "rewritten"
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"` // -1 when the file is gone
}

// statFingerprint stat()s a file and returns (size, mtime) suitable
// for ingest_state.recorded_size/recorded_mtime — the 🎯T68.6
// fingerprint that lets SourceDrift detect same-size in-place
// rewrites (size unchanged, mtime moved). Returns (0, "") when the
// file can't be stat'd, which the writer stores as NULL.
func statFingerprint(path string) (int64, string) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, ""
	}
	return info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano)
}

// SourceDriftReport summarises lost-source state across the indexed
// transcript files (🎯T68.6).
type SourceDriftReport struct {
	Deleted   int                `json:"deleted"`
	Truncated int                `json:"truncated"`
	Rewritten int                `json:"rewritten"`
	Examples  []SourceDriftEntry `json:"examples"`
}

// ingestCursor is one row from ingest_state with the optional
// fingerprint columns (🎯T68.6).
type ingestCursor struct {
	offset        int64
	recordedSize  sql.NullInt64
	recordedMtime sql.NullString
}

// readIngestCursors snapshots ingest_state into an in-memory map for
// drift detection. Cheap — one query, one pass.
func (s *Store) readIngestCursors() map[string]ingestCursor {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()
	rows, err := s.db.Query(
		`SELECT path, offset, recorded_size, recorded_mtime FROM ingest_state`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]ingestCursor{}
	for rows.Next() {
		var p string
		var c ingestCursor
		if err := rows.Scan(&p, &c.offset, &c.recordedSize, &c.recordedMtime); err != nil {
			continue
		}
		out[p] = c
	}
	return out
}

// SourceDrift detects indexed transcript sources that have drifted
// out from under the index (🎯T68.6). Read-only; never modifies state.
//
//   - file gone                           → "deleted"
//   - current size < the ingested offset  → "truncated"
//   - same size, mtime newer than recorded → "rewritten" (an in-place
//     edit at the same byte count; only detectable when the fingerprint
//     was recorded by ingest)
//
// Under the durable-tier model this is NOT an error: Claude Code prunes
// transcripts, the index is the authoritative durable copy, and the
// state-reconciler (ReconcileSourceState) propagates the condition
// onto session_meta as a tag rather than removing rows.
func (s *Store) SourceDrift() SourceDriftReport {
	cursors := s.readIngestCursors()
	paths := make([]string, 0, len(cursors))
	for p := range cursors {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	const maxExamples = 20
	var rep SourceDriftReport
	for _, p := range paths {
		c := cursors[p]
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				rep.Deleted++
				if len(rep.Examples) < maxExamples {
					rep.Examples = append(rep.Examples,
						SourceDriftEntry{Path: p, Kind: "deleted", Offset: c.offset, Size: -1})
				}
			}
			continue
		}
		if info.Size() < c.offset {
			rep.Truncated++
			if len(rep.Examples) < maxExamples {
				rep.Examples = append(rep.Examples,
					SourceDriftEntry{Path: p, Kind: "truncated", Offset: c.offset, Size: info.Size()})
			}
			continue
		}
		if c.recordedSize.Valid && c.recordedMtime.Valid &&
			info.Size() == c.recordedSize.Int64 &&
			info.ModTime().UTC().Format(time.RFC3339Nano) > c.recordedMtime.String {
			rep.Rewritten++
			if len(rep.Examples) < maxExamples {
				rep.Examples = append(rep.Examples,
					SourceDriftEntry{Path: p, Kind: "rewritten", Offset: c.offset, Size: info.Size()})
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
	cursors := s.readIngestCursors()

	nowStr := now.UTC().Format(time.RFC3339)
	type tagUpdate struct {
		sessionID string
		status    string
	}
	var candidates []tagUpdate
	for path, c := range cursors {
		var newStatus string
		info, err := os.Stat(path)
		switch {
		case os.IsNotExist(err):
			newStatus = "deleted_at=" + nowStr
		case err != nil:
			continue
		case info.Size() < c.offset:
			newStatus = "truncated_at=" + nowStr
		case c.recordedSize.Valid && c.recordedMtime.Valid &&
			info.Size() == c.recordedSize.Int64 &&
			info.ModTime().UTC().Format(time.RFC3339Nano) > c.recordedMtime.String:
			newStatus = "rewritten_at=" + nowStr
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
