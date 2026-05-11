// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
)

// TestMergeBySourcePercentileInterleaves verifies bug E fix: raw BM25
// ranks from messages_fts and docs_fts are not directly comparable, so
// merging by raw rank tends to clump one source above the other. The
// percentile-based merge interleaves sources so each corpus contributes
// to the top of the result list in proportion to its own relevance
// distribution.
func TestMergeBySourcePercentileInterleaves(t *testing.T) {
	// Pre-fix behaviour: sorting by raw rank would put all transcripts
	// (-5..-3) ahead of all vault hits (-2..-1). Post-fix: percentile
	// rankings tie each source's best/mid/worst, interleaving the slice.
	in := []SearchResult{
		{MessageID: 1, Role: "user", Text: "t-best", Rank: -5.0},
		{MessageID: 2, Role: "user", Text: "t-mid", Rank: -4.0},
		{MessageID: 3, Role: "user", Text: "t-worst", Rank: -3.0},
		{MessageID: -1, Role: "vault", Text: "v-best", Rank: -2.0},
		{MessageID: -2, Role: "vault", Text: "v-mid", Rank: -1.5},
		{MessageID: -3, Role: "vault", Text: "v-worst", Rank: -1.0},
	}
	out := mergeBySourcePercentile(in)

	// Expected order (raw rank breaks percentile ties; transcript -5 beats
	// vault -2 at percentile 0.0):
	wantOrder := []string{"t-best", "v-best", "t-mid", "v-mid", "t-worst", "v-worst"}
	if len(out) != len(wantOrder) {
		t.Fatalf("len = %d, want %d", len(out), len(wantOrder))
	}
	for i, want := range wantOrder {
		if out[i].Text != want {
			gotOrder := make([]string, len(out))
			for j, r := range out {
				gotOrder[j] = r.Text
			}
			t.Errorf("position %d = %q, want %q (full order: %v)",
				i, out[i].Text, want, gotOrder)
		}
	}
}

// TestMergeBySourcePercentileOnlyOneSource verifies that single-source
// inputs round-trip unchanged: percentile assignment treats the lone
// source as one bucket with percentile 0.0 for all entries, and the
// raw-rank tiebreaker preserves input order.
func TestMergeBySourcePercentileOnlyOneSource(t *testing.T) {
	in := []SearchResult{
		{MessageID: 1, Role: "user", Text: "a", Rank: -3.0},
		{MessageID: 2, Role: "user", Text: "b", Rank: -2.0},
		{MessageID: 3, Role: "user", Text: "c", Rank: -1.0},
	}
	out := mergeBySourcePercentile(in)
	for i := range in {
		if out[i].Text != in[i].Text {
			t.Errorf("position %d = %q, want %q (no reorder expected)",
				i, out[i].Text, in[i].Text)
		}
	}
}

// TestMergeBySourcePercentileSingleEach verifies that one-hit-per-source
// inputs sort by raw rank (since both percentiles are 0.0 and the
// tiebreaker is raw rank).
func TestMergeBySourcePercentileSingleEach(t *testing.T) {
	in := []SearchResult{
		{MessageID: -1, Role: "vault", Text: "v", Rank: -10.0},
		{MessageID: 1, Role: "user", Text: "t", Rank: -1.0},
	}
	out := mergeBySourcePercentile(in)
	if out[0].Text != "v" || out[1].Text != "t" {
		t.Errorf("got [%q, %q], want [v, t] — vault has better raw rank, "+
			"percentile tie breaks to lower rank", out[0].Text, out[1].Text)
	}
}

// TestMergeBySourcePercentileEmpty and TestMergeBySourcePercentileSingle
// cover the trivial fast paths.
func TestMergeBySourcePercentileEmpty(t *testing.T) {
	out := mergeBySourcePercentile(nil)
	if len(out) != 0 {
		t.Errorf("nil input must round-trip empty, got len=%d", len(out))
	}
}

func TestMergeBySourcePercentileSingle(t *testing.T) {
	in := []SearchResult{{Role: "user", Text: "only", Rank: -1}}
	out := mergeBySourcePercentile(in)
	if len(out) != 1 || out[0].Text != "only" {
		t.Errorf("single-element input must round-trip, got %v", out)
	}
}
