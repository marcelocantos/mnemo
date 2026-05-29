// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

// TestStreamDivergences covers the 🎯T68.4 divergence surface: streams
// with a cheap metric report a real gap; streams not yet instrumented
// report known=false (not a fabricated zero).
func TestStreamDivergences(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	// An idle, never-compacted session → owed → compaction backlog ≥ 1.
	writeJSONL(t, dir, "p", "sess-owed", []map[string]any{
		msg("user", "q1", now.Add(-40*time.Minute).Format(time.RFC3339)),
		msg("assistant", "a1", now.Add(-39*time.Minute).Format(time.RFC3339)),
		msg("user", "q2", now.Add(-38*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Seed an ingest_status row so a docs:* divergence shows drift.
	s.rwmu.Lock()
	s.recordBackfillStatusLocked("targets", 8, 10)
	s.rwmu.Unlock()

	byStream := map[string]StreamDivergence{}
	for _, d := range s.StreamDivergences() {
		byStream[d.Stream] = d
	}

	comp, ok := byStream["compactions"]
	if !ok || !comp.Known {
		t.Fatalf("compactions divergence missing/unknown: %+v", comp)
	}
	if comp.Gap < 1 {
		t.Errorf("expected compaction backlog ≥ 1 (owed session), got %d", comp.Gap)
	}

	idx, ok := byStream["transcript_index"]
	if !ok || !idx.Known {
		t.Errorf("transcript_index should be known: %+v", idx)
	}

	docs, ok := byStream["docs:targets"]
	if !ok || !docs.Known {
		t.Fatalf("docs:targets divergence missing/unknown: %+v", docs)
	}
	if docs.Gap != 2 {
		t.Errorf("expected docs:targets gap=2 (10 on disk − 8 indexed), got %d", docs.Gap)
	}

	// Not-yet-instrumented streams must be honest, not fabricated zeros.
	for _, name := range []string{"images", "vault"} {
		d, ok := byStream[name]
		if !ok {
			t.Errorf("expected %s stream to be reported", name)
			continue
		}
		if d.Known {
			t.Errorf("%s should report known=false until instrumented, got %+v", name, d)
		}
	}

	// github_mirrors is instrumented as of 🎯T68.5 (reconcile-cursor
	// backlog). With no repos in this fresh store the gap is 0.
	if gm, ok := byStream["github_mirrors"]; !ok || !gm.Known {
		t.Errorf("github_mirrors should be known (T68.5), got %+v", gm)
	} else if gm.Gap != 0 {
		t.Errorf("expected github_mirrors gap=0 in an empty store, got %d", gm.Gap)
	}
}
