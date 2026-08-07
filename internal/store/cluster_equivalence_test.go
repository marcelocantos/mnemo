// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// Equivalence and cost tests for the clustering rewrite.
//
// singleLinkThemes was replaced with union-find over the threshold
// graph. That is not an approximation: single-link clustering with a
// hard threshold cut IS the connected-components partition of that
// graph, so the two algorithms must agree exactly. This file proves it
// against the old implementation, kept here as a reference.

// naiveSingleLinkPartition is the ALGORITHM THAT SHIPPED BEFORE, reduced
// to the partition it computes. Retained only as a test oracle: it is
// O(n^3) and unusable in production (a 5000-doc run burned a core for
// 5h43m without finishing), but it defines the correct answer, and a
// rewrite that claims to be exact has to be measured against something.
func naiveSingleLinkPartition(docs []clusterDoc, threshold float64) [][]int {
	n := len(docs)
	if n == 0 {
		return nil
	}
	type agg struct{ members []int }
	all := make([]agg, n)
	for i := range all {
		all[i] = agg{members: []int{i}}
	}
	active := make([]int, n)
	for i := range active {
		active[i] = i
	}
	pairSim := func(a, b agg) float64 {
		best := 0.0
		for _, i := range a.members {
			for _, j := range b.members {
				if s := cosine(docs[i].vec, docs[j].vec); s > best {
					best = s
				}
			}
		}
		return best
	}
	for len(active) >= 2 {
		bi, bj, sim := -1, -1, -1.0
		for i := 0; i < len(active); i++ {
			for j := i + 1; j < len(active); j++ {
				if s := pairSim(all[active[i]], all[active[j]]); s > sim {
					sim = s
					bi, bj = i, j
				}
			}
		}
		if bi < 0 || sim < threshold {
			break
		}
		if bj < bi {
			bi, bj = bj, bi
		}
		ai, aj := active[bi], active[bj]
		merged := append(append([]int{}, all[ai].members...), all[aj].members...)
		sort.Ints(merged)
		all = append(all, agg{members: merged})
		active[bi] = len(all) - 1
		active = append(active[:bj], active[bj+1:]...)
	}
	out := make([][]int, 0, len(active))
	for _, ci := range active {
		m := append([]int{}, all[ci].members...)
		sort.Ints(m)
		out = append(out, m)
	}
	return normalisePartition(out)
}

// normalisePartition puts a partition in a canonical form so two
// partitions can be compared regardless of cluster ordering.
func normalisePartition(p [][]int) [][]int {
	out := make([][]int, 0, len(p))
	for _, c := range p {
		m := append([]int{}, c...)
		sort.Ints(m)
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) == 0 || len(out[j]) == 0 {
			return len(out[i]) < len(out[j])
		}
		return out[i][0] < out[j][0]
	})
	return out
}

// partitionFromThemes extracts the member partition from the real
// clustering entry point.
func partitionFromThemes(themes []docTheme) [][]int {
	out := make([][]int, 0, len(themes))
	for _, th := range themes {
		out = append(out, th.members)
	}
	return normalisePartition(out)
}

// randomDocs builds a corpus with planted structure: `groups` clusters
// of mutually-similar vectors plus some noise, so the partition is
// non-trivial rather than all-one-cluster or all-singletons.
func randomDocs(rng *rand.Rand, groups, perGroup, dims int) []clusterDoc {
	var docs []clusterDoc
	for g := 0; g < groups; g++ {
		base := map[string]float64{}
		for d := 0; d < dims/2; d++ {
			base[fmt.Sprintf("t%d_%d", g, d)] = 1
		}
		for m := 0; m < perGroup; m++ {
			vec := map[string]float64{}
			for k, v := range base {
				if rng.Float64() < 0.85 { // most shared terms present
					vec[k] = v
				}
			}
			for d := 0; d < 2; d++ { // a little private vocabulary
				vec[fmt.Sprintf("n%d_%d_%d", g, m, d)] = 1
			}
			docs = append(docs, clusterDoc{
				ClusterCorpusDoc: ClusterCorpusDoc{
					DocID:  fmt.Sprintf("d_%d_%d", g, m),
					Weight: 1,
				},
				vec: vec,
			})
		}
	}
	return docs
}

// TestSingleLinkMatchesNaiveReference is the equivalence proof: the
// union-find implementation must produce exactly the partition the old
// O(n^3) algorithm produced, across many random corpora and thresholds.
func TestSingleLinkMatchesNaiveReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cfg := VaultClusteringConfig{}
	for trial := 0; trial < 25; trial++ {
		docs := randomDocs(rng, 2+rng.Intn(4), 2+rng.Intn(5), 6)
		threshold := 0.1 + rng.Float64()*0.7

		want := naiveSingleLinkPartition(docs, threshold)
		themes, err := singleLinkThemes(context.Background(), docs, threshold, cfg, nil)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		got := partitionFromThemes(themes)

		if len(got) != len(want) {
			t.Fatalf("trial %d (n=%d, threshold=%.3f): got %d clusters, reference "+
				"produced %d\ngot  %v\nwant %v",
				trial, len(docs), threshold, len(got), len(want), got, want)
		}
		for i := range want {
			if len(got[i]) != len(want[i]) {
				t.Fatalf("trial %d: cluster %d differs\ngot  %v\nwant %v",
					trial, i, got, want)
			}
			for j := range want[i] {
				if got[i][j] != want[i][j] {
					t.Fatalf("trial %d: cluster %d differs\ngot  %v\nwant %v",
						trial, i, got, want)
				}
			}
		}
	}
}

// TestClusteringIsCancellable pins the other half of the defect: the old
// loop had no ctx check anywhere, so cancellation could not reach the
// longest-running computation in the daemon and shutdown could only
// abandon it.
func TestClusteringIsCancellable(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	docs := randomDocs(rng, 30, 30, 8) // 900 docs
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := singleLinkThemes(ctx, docs, 0.3, VaultClusteringConfig{}, nil)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancelled clustering took %s", elapsed)
	}
	if err == nil {
		t.Error("a cancelled clustering pass must report cancellation rather than " +
			"completing; without this, shutdown can only abandon it")
	}
}

// TestClusteringScalesToTheCap is the cost oracle. The production cap is
// 5000 documents, and the old algorithm could not finish that in hours.
// This runs the real cap and requires it inside a bound that would have
// been impossible before.
func TestClusteringScalesToTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("cost oracle; skipped under -short")
	}
	rng := rand.New(rand.NewSource(3))
	docs := randomDocs(rng, 100, 50, 10) // 5000 docs, the default MaxDocs
	if len(docs) != 5000 {
		t.Fatalf("fixture built %d docs, want 5000", len(docs))
	}

	start := time.Now()
	themes, err := singleLinkThemes(context.Background(), docs, 0.35, VaultClusteringConfig{}, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("clustering: %v", err)
	}
	if elapsed > 60*time.Second {
		t.Errorf("clustering 5000 documents took %s; the production cap must "+
			"complete comfortably inside a reconcile tick", elapsed)
	}
	t.Logf("clustered %d docs into %d themes in %s", len(docs), len(themes), elapsed)
}
