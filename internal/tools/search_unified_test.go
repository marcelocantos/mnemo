// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strings"
	"testing"
)

// Tool-level tests for 🎯T144's unified search.
//
// The store tests prove the ranking and the corpora. These prove the
// TOOL exposes them: that `kinds` reaches the unified path, that the
// output names the corpus and the ranking method, and — most
// importantly — that omitting `kinds` leaves the pre-🎯T144 behaviour
// exactly as it was. mnemo_search is 55% of all agent calls; a
// regression there costs more than this feature adds.

// TestSearchToolDeclaresKinds pins that the parameter exists and that
// the description states what a caller needs to decide with: which
// corpora, how ranking works, and what it costs.
func TestSearchToolDeclaresKinds(t *testing.T) {
	var found bool
	for _, tool := range Definitions() {
		if tool.Name != "mnemo_search" {
			continue
		}
		found = true
		desc := tool.Description

		for _, want := range []string{
			"kinds",      // the scoping parameter
			"calibrated", // how hits are ranked
			"fusion",     // and how they are ranked when they cannot be
			"COST",       // the per-call cost, stated
		} {
			if !strings.Contains(desc, want) {
				t.Errorf("mnemo_search description does not mention %q — a caller "+
					"cannot choose corpora sensibly without it", want)
			}
		}
		// The removed per-corpus tools must be discoverable here, since
		// subsumption is the entire justification for having deleted them.
		for _, corpus := range []string{"doc", "target", "commit", "memory"} {
			if !strings.Contains(desc, corpus) {
				t.Errorf("description does not name the %q corpus; the tools removed "+
					"in 🎯T143 are meant to be reachable through this one", corpus)
			}
		}
		if _, ok := tool.InputSchema.Properties["kinds"]; !ok {
			t.Error("mnemo_search has no kinds parameter in its input schema")
		}
	}
	if !found {
		t.Fatal("mnemo_search is not registered")
	}
}

// TestSegmentsAndDecisionsAreSubsumed is the retirement oracle: those
// two tools are gone, and their corpora must be reachable through
// search instead. Removing a tool without its capability landing
// somewhere is deletion, not subsumption — and 🎯T144 exists precisely
// because 🎯T143 did the former.
func TestSegmentsAndDecisionsAreSubsumed(t *testing.T) {
	registered := map[string]bool{}
	for _, tool := range Definitions() {
		registered[tool.Name] = true
	}
	for _, gone := range []string{"mnemo_segments", "mnemo_decisions"} {
		if registered[gone] {
			t.Errorf("%s is still registered; 🎯T144 subsumes it into mnemo_search", gone)
		}
	}

	var desc string
	for _, tool := range Definitions() {
		if tool.Name == "mnemo_search" {
			desc = tool.Description
		}
	}
	for _, corpus := range []string{"segment", "decision"} {
		if !strings.Contains(desc, corpus) {
			t.Errorf("mnemo_search does not offer the %q corpus, so retiring its tool "+
				"lost the capability rather than moving it", corpus)
		}
	}
}
