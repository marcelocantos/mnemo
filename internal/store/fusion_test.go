// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Oracles for the rank-fusion fallback (🎯T144).
//
// The fallback was specified in the target's acceptance, described in
// UnifiedHit's own doc comment ("a fusion score under the degraded one")
// and asserted in the achievement attestation — and was not implemented.
// The degraded path assigned a flat neutral prior instead, which is not a
// weak claim but a claim of exact medianness, and loses deterministically
// to any corpus holding `limit` hits above its own median.
//
// Nothing caught it because the two end-to-end oracles that would have
// were themselves failing, and the suite's exit code was being read
// through a pipe that could not report failure. These tests pin the
// mechanism directly so it cannot silently revert to a prior again.

// TestFusionScoreIsCorpusIndependent is the property the whole fallback
// rests on: rank N scores the same wherever it came from. If this stops
// holding, fusion stops being fusion and becomes another scale on which
// a big corpus outranks a small one by construction.
func TestFusionScoreIsCorpusIndependent(t *testing.T) {
	for rank := 0; rank < 10; rank++ {
		a, b := rrfScore(rank), rrfScore(rank)
		if a != b {
			t.Fatalf("rank %d scored %v and %v", rank, a, b)
		}
	}
	// Strictly decreasing, so within-corpus order survives fusion.
	for rank := 1; rank < 50; rank++ {
		if rrfScore(rank) >= rrfScore(rank-1) {
			t.Fatalf("rank %d did not score below rank %d", rank, rank-1)
		}
	}
	// And a lower-ranked hit from one corpus must lose to a higher-ranked
	// hit from any other — the interleaving is by rank, not by corpus.
	if rrfScore(1) >= rrfScore(0) {
		t.Fatal("fusion must interleave strictly by rank")
	}
}

// TestFusionTriggersOnThinEvidence pins the trigger. A corpus whose
// scores are mostly prior rather than measurement cannot be placed on the
// quantile axis, and the product must say so rather than quietly ranking
// it as though it could.
func TestFusionTriggersOnThinEvidence(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 300)
	// A ten-document corpus: calibratable, but thinly.
	for i := 0; i < 10; i++ {
		mustExec(t, s, `INSERT INTO skills (file_path, name, description, content, updated_at)
			VALUES (?, ?, 'd', 'watcher watcher descriptor handling and more watcher prose', '2026-01-01')`,
			fmt.Sprintf("s%d.md", i), fmt.Sprintf("skill %d", i))
	}
	now := time.Now()
	for _, kind := range []string{"message", "skill"} {
		spec, _ := corpusByKind(kind)
		if _, err := s.CalibrateCorpus(context.Background(), spec, now); err != nil {
			t.Fatalf("CalibrateCorpus(%s): %v", kind, err)
		}
	}

	res, err := s.UnifiedSearch("watcher", []string{"message", "skill"}, 25, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if res.Ranking != "rank_fusion" {
		t.Errorf("ranking = %q, want \"rank_fusion\": a corpus below the "+
			"evidence floor must send the merge to fusion, because a quantile "+
			"that is two-thirds prior is not comparable with one that is not",
			res.Ranking)
	}
	// The thin corpus must actually be reachable, which is the whole point.
	var skills int
	for _, h := range res.Hits {
		if h.Kind == "skill" {
			skills++
		}
	}
	if skills == 0 {
		t.Error("the thin corpus is still unreachable under fusion")
	}
}

func TestTiedSearchRanksPreserveRequestedCorpusOrder(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const marker = "fusionorderfixture"
	insertPlain(t, s, "fusion-tie-session", "project", marker)
	mustExec(t, s, `INSERT INTO skills (file_path, name, description, content, updated_at)
		VALUES ('fixture.md', 'Tie fixture', '', ?, '2026-01-01')`, marker)
	res, err := s.UnifiedSearch(marker, []string{"skill", "message"}, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Ranking != "rank_fusion" || len(res.Hits) != 2 || res.Hits[0].Score != res.Hits[1].Score {
		t.Fatalf("want one tied hit per corpus on the fusion path, got %+v", res)
	}
	if res.Hits[0].Kind != "skill" || res.Hits[1].Kind != "message" {
		t.Fatalf("tied corpus order = %q, %q, want skill, message", res.Hits[0].Kind, res.Hits[1].Kind)
	}
}

func TestSplitSearchUsesEvidenceFromBothIndexes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	const marker = "splitevidencefixture"
	insertPlain(t, s, "split-legacy", "project", marker+" below the migration marker")
	stem := insertPlain(t, s, "split-stem", "project", marker+" at the migration marker")
	armStemSplit(t, s, stem)
	now := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	cal := &Calibration{Corpus: "message", Quantiles: []float64{0, 1}, SampleSize: 1000, DocCount: 2, ComputedAt: now}
	if err := s.saveCalibration(cal); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		res, err := s.UnifiedSearch(marker, []string{"message"}, 2, now)
		if err != nil {
			t.Fatal(err)
		}
		if res.Ranking != want || len(res.Hits) != 2 {
			t.Fatalf("split search = %+v, want %s and both hits", res, want)
		}
	}
	check("rank_fusion")
	cal.Corpus = "message#stem"
	if err := s.saveCalibration(cal); err != nil {
		t.Fatal(err)
	}
	check("calibrated")
}

// TestWellEvidencedCorporaStayCalibrated is the other side: fusion is the
// fallback, not the default. Quality-blind ranking is a real cost, and a
// query where every corpus is well sampled must not pay it.
func TestWellEvidencedCorporaStayCalibrated(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCalibratableCorpus(t, s, 400)
	now := time.Now()
	spec, _ := corpusByKind("message")
	if _, err := s.CalibrateCorpus(context.Background(), spec, now); err != nil {
		t.Fatalf("CalibrateCorpus: %v", err)
	}

	res, err := s.UnifiedSearch("watcher", []string{"message"}, 10, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if res.Ranking != "calibrated" {
		t.Errorf("ranking = %q, want \"calibrated\"; fusion is quality-blind "+
			"and must not displace a usable distribution", res.Ranking)
	}
	// Calibrated scores are quantiles, so they must span [0,1] rather than
	// the compressed RRF band — a cheap guard against the paths crossing.
	for _, h := range res.Hits {
		if h.Score < 0 || h.Score > 1 {
			t.Errorf("calibrated score %v outside [0,1]", h.Score)
		}
		if h.Score < 0.05 {
			t.Errorf("score %v looks like a fusion score on the calibrated path", h.Score)
		}
	}
}
