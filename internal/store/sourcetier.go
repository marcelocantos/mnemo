// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"sort"
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
