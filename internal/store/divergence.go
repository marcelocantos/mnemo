// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StreamDivergence reports the gap between a derived stream's desired
// and actual state — the per-stream "distance to the fixed point" that
// the convergence data plane (🎯T68) is meant to drive to zero, made
// observable so a gap can be reconciled rather than silently tolerated
// (🎯T68.4).
//
// Known == false means there is no cheap gap metric for this stream
// yet: the surface reports it honestly as "unknown" rather than
// fabricating a number. Each such stream becomes Known as its own
// reconciler slice instruments it (e.g. the git/PR/CI mirrors under
// 🎯T68.5).
type StreamDivergence struct {
	Stream string `json:"stream"`
	Known  bool   `json:"known"`
	// Gap is the work outstanding to reach the fixed point, in Unit.
	// Zero means converged. Meaningful only when Known.
	Gap  int64  `json:"gap"`
	Unit string `json:"unit"`
	// LastReconciled is when this stream last advanced toward its fixed
	// point (RFC3339), or "" when unknown / never.
	LastReconciled string `json:"last_reconciled"`
	Note           string `json:"note"`
}

// streamDivergenceGatherer is a single stream's gap probe. Returning a
// gatherer per stream (rather than a monolithic switch) is what lets a
// later slice instrument a now-"unknown" stream by registering one more
// gatherer — see 🎯T68.4 acceptance.
type streamDivergenceGatherer struct {
	stream string
	gather func() StreamDivergence
}

// StreamDivergences returns the actual-vs-desired gap for every derived
// stream, ordered by stream name (🎯T68.4). It acquires no lock itself;
// each gatherer takes whatever lock it needs via the methods it calls,
// so the report never blocks the daemon for longer than the slowest
// individual probe. Probes are designed to be cheap enough to call on
// demand; the transcript-index probe walks the project dirs, which is
// the one O(files) cost and still completes in well under a second on a
// realistic corpus.
func (s *Store) StreamDivergences() []StreamDivergence {
	gatherers := []streamDivergenceGatherer{
		{"compactions", s.compactionDivergence},
		{"transcript_index", s.transcriptIndexDivergence},
		{"images", s.imagesDivergence},
		{"vault", s.vaultDivergence},
		{"github_mirrors", s.githubMirrorsDivergence},
	}

	out := make([]StreamDivergence, 0, len(gatherers)+8)
	for _, g := range gatherers {
		out = append(out, g.gather())
	}
	// The repo-level document streams already track coverage in
	// ingest_status; surface each as its own divergence row (gap =
	// files on disk not yet indexed).
	for _, b := range s.BackfillStatuses() {
		out = append(out, StreamDivergence{
			Stream:         "docs:" + b.Stream,
			Known:          true,
			Gap:            int64(b.Drift()),
			Unit:           "files",
			LastReconciled: b.LastBackfill,
			Note:           "ingest_status drift (files on disk minus files indexed)",
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Stream < out[j].Stream })
	return out
}

// compactionDivergence reports the owed-but-uncompacted session backlog
// (🎯T68.1's COUNT(*) OVER () gap). The trigger thresholds mirror the
// watcher's defaults; this is an estimate of the gap, not an assertion
// of exactly which sessions a scan will pick.
func (s *Store) compactionDivergence() StreamDivergence {
	const (
		minDeltaMsgs   = 50
		idleMinutes    = 15
		maxBudgetRatio = 0.10
	)
	_, backlog, err := s.SelectCompactionCandidates(
		minDeltaMsgs, time.Now().Add(-idleMinutes*time.Minute), maxBudgetRatio, 1)
	if err != nil {
		return StreamDivergence{Stream: "compactions", Known: false,
			Note: "backlog query failed: " + err.Error()}
	}
	var last string
	s.rwmu.RLock()
	_ = s.db.QueryRow(`SELECT COALESCE(MAX(generated_at), '') FROM compactions`).Scan(&last)
	s.rwmu.RUnlock()
	return StreamDivergence{
		Stream: "compactions", Known: true,
		Gap: int64(backlog), Unit: "sessions", LastReconciled: last,
		Note: "owed sessions (new substantive messages past the latest compaction)",
	}
}

// transcriptIndexDivergence reports un-ingested transcript bytes: for
// every .jsonl under the project dirs, max(0, size - ingested offset).
// The offsets map is guarded by s.mu (not s.rwmu); we snapshot it under
// that lock and then walk the filesystem lock-free.
func (s *Store) transcriptIndexDivergence() StreamDivergence {
	s.mu.Lock()
	offsets := make(map[string]int64, len(s.offsets))
	for k, v := range s.offsets {
		offsets[k] = v
	}
	dirs := s.projectDirs()
	s.mu.Unlock()

	var pendingBytes int64
	var pendingFiles int64
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if behind := info.Size() - offsets[path]; behind > 0 {
				pendingBytes += behind
				pendingFiles++
			}
			return nil
		})
	}
	return StreamDivergence{
		Stream: "transcript_index", Known: true,
		Gap: pendingBytes, Unit: "bytes",
		Note: "un-ingested transcript bytes across project dirs" +
			" (" + strconv.FormatInt(pendingFiles, 10) + " files behind)",
	}
}

// imagesDivergence is not yet instrumented: image descriptions/OCR are
// produced by background workers, but there is no cheap stored signal
// for "images still awaiting a description" without joining the
// description store. Reported as unknown until a later slice adds the
// metric.
func (s *Store) imagesDivergence() StreamDivergence {
	return StreamDivergence{
		Stream: "images", Known: false,
		Note: "no cheap pending-description metric yet; instrument with the image-describer worker",
	}
}

// vaultDivergence is not yet instrumented: per-entity staleness (source
// timestamp newer than the rendered note's recorded entity_ts) is not
// summarised anywhere cheap. A future slice can have the vault sync
// record a high-water mark to make this Known.
func (s *Store) vaultDivergence() StreamDivergence {
	return StreamDivergence{
		Stream: "vault", Known: false,
		Note: "per-entity staleness not summarised; needs a vault-sync high-water mark",
	}
}

// githubMirrorsDivergence reports the count of repo×stream pairs whose
// mirror reconcile cursor is missing or stale (🎯T68.5). Covers the
// converted streams (ci today); github/commits join the gap as they
// convert.
func (s *Store) githubMirrorsDivergence() StreamDivergence {
	gap, last := s.MirrorBacklog(time.Now())
	return StreamDivergence{
		Stream: "github_mirrors", Known: true,
		Gap: int64(gap), Unit: "repo-streams", LastReconciled: last,
		Note: "repos with a stale/missing mirror reconcile cursor (ci converted; github/commits pending 🎯T68.5)",
	}
}
