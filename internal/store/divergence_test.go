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

	// A never-compacted session above the addenda budget → owed →
	// compaction backlog ≥ 1. compactionDivergence measures against
	// DefaultAddendaBudgetTokens (50k), so the assistant turn must carry
	// at least that much output volume.
	writeJSONL(t, dir, "p", "sess-owed", []map[string]any{
		msg("user", "q1 a question with enough text", now.Add(-40*time.Minute).Format(time.RFC3339)),
		asstTok("a1 a sizeable answer", now.Add(-39*time.Minute).Format(time.RFC3339), 60000, 0, 500),
		msg("user", "q2 a follow-up question", now.Add(-38*time.Minute).Format(time.RFC3339)),
	})

	s := newTestStore(t, dir)
	if err := s.IngestAll(); err != nil {
		t.Fatalf("IngestAll: %v", err)
	}

	// Seed an ingest_status row so a docs:* divergence shows drift.
	s.recordBackfillStatus("targets", 8, 10)

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

	// source_state is instrumented (🎯T68.6 Law 2). On a fresh store
	// with no source drift, gap = 0.
	if ss, ok := byStream["source_state"]; !ok || !ss.Known {
		t.Errorf("source_state should be known (T68.6), got %+v", ss)
	} else if ss.Gap != 0 {
		t.Errorf("expected source_state gap=0 on intact corpus, got %d", ss.Gap)
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
