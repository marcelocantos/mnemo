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
		{"source_state", s.sourceStateDivergence},
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
// (🎯T72's token-volume fixed point). The budget mirrors the watcher's
// default; this is an estimate of the gap, not an assertion of exactly
// which sessions a scan will pick.
func (s *Store) compactionDivergence() StreamDivergence {
	const maxBudgetRatio = 0.10
	// Exclude quarantined sessions (🎯T77) so the backlog reflects what a
	// scan can actually pick, not sessions stuck failing.
	since := time.Now().Add(-DefaultQuarantineCooldown)
	_, backlog, err := s.SelectCompactionCandidates(
		DefaultAddendaBudgetTokens, maxBudgetRatio, DefaultQuarantineThreshold, since, 1)
	if err != nil {
		return StreamDivergence{Stream: "compactions", Known: false,
			Note: "backlog query failed: " + err.Error()}
	}
	var last string
	_ = s.readDB.QueryRow(`SELECT COALESCE(MAX(generated_at), '') FROM compactions`).Scan(&last)
	return StreamDivergence{
		Stream: "compactions", Known: true,
		Gap: int64(backlog), Unit: "sessions", LastReconciled: last,
		Note: "owed sessions (addenda token volume past the latest compaction meets the budget)",
	}
}

// transcriptIndexDivergence reports un-ingested transcript bytes: for
// every .jsonl under the project dirs, max(0, size - ingested offset).
// The offsets map is guarded by s.mu; we snapshot it under
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

// vaultDivergence reports the vault-orphan backlog (🎯T68.6 vault GC):
// the count of manifest rows whose file is gone plus on-disk notes
// with no manifest entry. The path is mirrored from registry/config
// via SetVaultPath; vault-unconfigured installs report Known=false
// with no fabricated gap.
func (s *Store) vaultDivergence() StreamDivergence {
	vp := s.getVaultPath()
	if vp == "" {
		return StreamDivergence{
			Stream: "vault", Known: false,
			Note: "vault not configured; no orphan backlog to compute",
		}
	}
	gap := s.VaultOrphanBacklog(vp)
	return StreamDivergence{
		Stream: "vault", Known: true,
		Gap: int64(gap), Unit: "orphans",
		Note: "manifest rows whose note is gone + *.md files under the vault with no manifest entry",
	}
}

// sourceStateDivergence reports the state-convergence gap (Law 2 of
// 🎯T68.6): sessions whose source has drifted (file gone or shrunk
// below the ingested offset) but whose source_status tag is still
// 'live' — i.e. work the next ReconcileSourceState pass will pick up.
// In steady state this is 0 because the reconciler runs each minute.
// A non-zero gap means recent drift not yet tagged, or the worker is
// stalled.
func (s *Store) sourceStateDivergence() StreamDivergence {
	s.mu.Lock()
	offsets := make(map[string]int64, len(s.offsets))
	for p, o := range s.offsets {
		offsets[p] = o
	}
	s.mu.Unlock()

	gap := 0
	for path, off := range offsets {
		var drifted bool
		info, err := os.Stat(path)
		switch {
		case os.IsNotExist(err):
			drifted = true
		case err != nil:
			continue
		case info.Size() < off:
			drifted = true
		}
		if !drifted {
			continue
		}
		sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if sessionID == "" || sessionID == filepath.Base(path) {
			continue
		}
		var status string
		_ = s.readDB.QueryRow(
			`SELECT source_status FROM session_meta WHERE session_id = ?`,
			sessionID).Scan(&status)
		if status == "" || status == "live" {
			gap++
		}
	}

	var lastTagged string
	_ = s.readDB.QueryRow(
		`SELECT COALESCE(MAX(source_state_at), '') FROM session_meta
		 WHERE source_status != '' AND source_status != 'live'`).Scan(&lastTagged)
	return StreamDivergence{
		Stream: "source_state", Known: true,
		Gap: int64(gap), Unit: "sessions", LastReconciled: lastTagged,
		Note: "sessions whose source has drifted (deleted/truncated) but whose source_status is still live — Law-2 state-convergence gap",
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
		Note: "repos with a stale/missing mirror reconcile cursor across the ci/github/commits streams",
	}
}
