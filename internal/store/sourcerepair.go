// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"path/filepath"
	"strings"
)

// Repairing session provenance from the path a transcript was read from
// (🎯T127).
//
// The producing agent is knowable from the file's location — the roots are
// disjoint (~/.claude/projects, ~/.codex/{sessions,archived_sessions},
// ~/.grok/sessions) — and mnemo already records that location in
// ingest_state. So a session's source never has to be guessed; it can be
// recomputed from evidence at any time.
//
// It needs recomputing because two historical facts combine badly:
//
//   - session_meta.source is written only at ingest time, and ingest is
//     offset-based. Once a file is fully consumed it is never re-parsed,
//     so a source recorded wrongly stays wrong forever.
//   - Codex ingest and the source column both arrived together (🎯T99,
//     2026-06-30). Transcripts already consumed by the generic path before
//     that date kept its defaults, and nothing since has revisited them.
//
// The generic path leaves two distinct marks. Its session_meta upsert omits
// source entirely, so the row takes the schema default 'claude' — which is
// why "produced by Claude" and "nobody said" are indistinguishable in the
// data. And it derives the session id from the FILENAME STEM, which for a
// Codex rollout is `rollout-<ts>-<uuid>` rather than the uuid — producing a
// phantom row that duplicates the real session and holds no messages.

// sourceFromPath reports the agent that produced a transcript, and the
// session id its own parser would have derived, from the path alone.
// Returns ok=false for a path under no known root.
func sourceFromPath(path string) (source, sessionID string, ok bool) {
	slashed := filepath.ToSlash(path)
	switch {
	case strings.Contains(slashed, "/.codex/"), isCodexRollout(path):
		return "codex", codexSessionID(path), true
	case strings.Contains(slashed, "/.grok/"), isGrokUpdates(path):
		return "grok", grokSessionID(path), true
	case strings.Contains(slashed, "/.claude/projects/"):
		return "claude", strings.TrimSuffix(filepath.Base(path), ".jsonl"), true
	}
	return "", "", false
}

// sessionIDFromPath returns the session id a transcript belongs to, using
// the same derivation its own parser uses. Reports false for a path under
// no known root, or one that does not name a session at all.
//
// Anything that maps a file to a session must go through this. Deriving
// the id independently — historically, by taking the filename stem — is
// what produced phantom sessions: correct for Claude, wrong for Codex
// (rollout-<ts>-<uuid>.jsonl) and Grok (<id>/updates.jsonl).
func sessionIDFromPath(path string) (string, bool) {
	if _, id, ok := sourceFromPath(path); ok && id != "" {
		return id, true
	}
	// Unrecognised root: fall back to the Claude convention (basename
	// minus .jsonl), which is what this always did. The fallback is not
	// laziness — extra_project_dirs (🎯T15) deliberately puts real Claude
	// transcripts outside ~/.claude/projects, e.g. a Windows VM's
	// directory over SMB, and silently skipping those would stop drift
	// tagging for them. Codex and Grok are recognised by filename shape
	// as well as by root, so they never reach here.
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, ".jsonl")
	if stem == "" || stem == base {
		return "", false
	}
	return stem, true
}

// RepairSessionSources recomputes session provenance from ingest paths and
// clears out phantom sessions, returning how many of each it touched.
//
// Scoped deliberately: it walks ingest_state (hundreds of rows) rather than
// session_meta (tens of thousands), because only sessions that came from a
// known root can be judged, and those are exactly the ones ingest_state
// names. Idempotent — a second run finds nothing.
func (s *Store) RepairSessionSources() (retagged, removed int, err error) {
	rows, err := s.readDB.Query(`SELECT path FROM ingest_state`)
	if err != nil {
		return 0, 0, err
	}
	type fix struct{ source, sessionID, stem string }
	var fixes []fix
	for rows.Next() {
		var path string
		if rows.Scan(&path) != nil {
			continue
		}
		source, sessionID, ok := sourceFromPath(path)
		if !ok || sessionID == "" {
			continue
		}
		fixes = append(fixes, fix{
			source:    source,
			sessionID: sessionID,
			stem:      strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, f := range fixes {
		res, err := s.writeDB.Exec(
			`UPDATE session_meta SET source = ? WHERE session_id = ? AND source != ?`,
			f.source, f.sessionID, f.source)
		if err != nil {
			return retagged, removed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			retagged += int(n)
		}

		// The phantom: a row keyed by the filename stem when the real
		// session lives under a different id, holding no messages because
		// the generic parser could make nothing of the file. Delete only
		// when all three hold, so a legitimately stem-named session (every
		// Claude transcript) is never touched.
		if f.stem == f.sessionID {
			continue
		}
		res, err = s.writeDB.Exec(`
			DELETE FROM session_meta
			WHERE session_id = ?
			  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.session_id = session_meta.session_id)
			  AND EXISTS (SELECT 1 FROM session_meta real WHERE real.session_id = ?)`,
			f.stem, f.sessionID)
		if err != nil {
			return retagged, removed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}
	if retagged > 0 || removed > 0 {
		slog.Info("repaired session provenance from ingest paths",
			"retagged", retagged, "phantoms_removed", removed)
	}
	return retagged, removed, nil
}
