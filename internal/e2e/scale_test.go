// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build scale
// +build scale

// Tier 3 — real-data scale tests (🎯T73). These tests run only with
// the `scale` build tag (`go test -tags 'sqlite_fts5 scale' ./...`)
// and require MNEMO_TEST_SNAPSHOT to point at an isolated snapshot
// of the user's mnemo data. Default `go test ./...` invocations
// never see these.
//
// The cmd/mnemo-test-snapshot helper produces the snapshot; see
// docs/testing.md for the workflow.
//
// Tier 3 assertions are INVARIANTS rather than exact values — a
// snapshot's contents change daily as transcript history grows, so
// a test that asserts "exactly N compactions" would flake. The
// invariants below hold against any reasonable snapshot.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// snapshotHome reads MNEMO_TEST_SNAPSHOT and skips the test with a
// clear message when it is unset. Centralised so every Tier 3 test
// gets the same skip behaviour.
func snapshotHome(t *testing.T) string {
	t.Helper()
	home := os.Getenv("MNEMO_TEST_SNAPSHOT")
	if home == "" {
		t.Skip("MNEMO_TEST_SNAPSHOT not set; skipping Tier 3 scale test " +
			"(run cmd/mnemo-test-snapshot first, then set the env var)")
	}
	if _, err := os.Stat(home); err != nil {
		t.Skipf("MNEMO_TEST_SNAPSHOT=%q not accessible: %v", home, err)
	}
	return home
}

// TestScaleBacklogConvergence is a worked example of a Tier 3
// invariant assertion (🎯T73 acceptance #6 — backlog convergence).
// On any production-shape snapshot, the compactor's reported
// backlog must be a bounded, observable number — not an unbounded
// runaway. Concretely, mnemo_compactor_status returns a numeric
// "backlog" line and the value must be parseable, non-negative, and
// (loosely) less than the total session count (a backlog larger
// than the entire session set means the trigger is broken).
//
// This assertion is intentionally weak: it tests the SHAPE of the
// convergence model, not a particular threshold. A snapshot taken
// two months from now still passes if the compactor is healthy.
func TestScaleBacklogConvergence(t *testing.T) {
	home := snapshotHome(t)
	d := Start(t, Options{Home: home})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pull the live compactor status. Look for "backlog:" and
	// parse the integer.
	out, err := d.Call(ctx, "mnemo_compactor_status", nil)
	if err != nil {
		t.Fatalf("mnemo_compactor_status: %v\n%s", err, d.Log())
	}
	backlog, ok := parseBacklog(out)
	if !ok {
		t.Fatalf("mnemo_compactor_status: could not parse backlog from:\n%s", out)
	}
	if backlog < 0 {
		t.Errorf("compactor backlog is negative (%d) — invariant broken", backlog)
	}

	// Sanity bound: backlog must not exceed total session count.
	// Read mnemo_stats to get the total. We allow some slack for
	// race conditions between the two MCP calls — backlog ≤ 1.2 ×
	// total is enough to catch a runaway without spurious flakes.
	statsOut, err := d.Call(ctx, "mnemo_stats", nil)
	if err != nil {
		t.Fatalf("mnemo_stats: %v\n%s", err, d.Log())
	}
	total, ok := parseTotalSessions(statsOut)
	if !ok {
		t.Logf("could not parse total sessions from mnemo_stats; relying on backlog≥0 invariant alone")
		return
	}
	if total > 0 && float64(backlog) > 1.2*float64(total) {
		t.Errorf("compactor backlog %d exceeds 1.2 × total sessions %d — "+
			"convergence model is broken", backlog, total)
	}

	t.Logf("backlog=%d, total_sessions=%d — within bounds", backlog, total)
}

// TestScaleVaultStatusReachable is a second worked example: even
// against a production-scale snapshot the basic MCP surface
// (mnemo_vault_status) must remain reachable and return a parseable
// response within a few seconds. Validates the e2e harness against
// real data sizes without any specific value claims.
func TestScaleVaultStatusReachable(t *testing.T) {
	home := snapshotHome(t)
	d := Start(t, Options{Home: home})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	out, err := d.Call(ctx, "mnemo_vault_status", nil)
	if err != nil {
		t.Fatalf("mnemo_vault_status: %v\n%s", err, d.Log())
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("mnemo_vault_status took %v — exceeds 10s budget at scale", elapsed)
	}
	if len(out) == 0 {
		t.Errorf("mnemo_vault_status returned empty body at scale")
	}
}

// parseBacklog extracts the integer following the literal "backlog:"
// in the mnemo_compactor_status output. The status tool formats it
// like "backlog: 8633" (possibly indented).
func parseBacklog(s string) (int, bool) {
	const key = "backlog:"
	i := strings.Index(s, key)
	if i < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(s[i+len(key):])
	// Take the first whitespace-bounded token, drop any trailing
	// "(...)" hint the tool may append.
	tok := strings.Fields(rest)
	if len(tok) == 0 {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(tok[0], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// parseTotalSessions looks for a "TotalSessions" or "sessions" field
// in mnemo_stats output. The exact key varies by version — try a
// couple of candidates and fall back to JSON decoding if the output
// happens to be JSON.
func parseTotalSessions(s string) (int, bool) {
	for _, key := range []string{"total_sessions:", "sessions:", "TotalSessions:"} {
		if i := strings.Index(s, key); i >= 0 {
			rest := strings.TrimSpace(s[i+len(key):])
			tok := strings.Fields(rest)
			if len(tok) == 0 {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(tok[0], "%d", &n); err == nil {
				return n, true
			}
		}
	}
	// JSON fallback.
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err == nil {
		for _, key := range []string{"total_sessions", "TotalSessions", "sessions"} {
			if v, ok := doc[key].(float64); ok {
				return int(v), true
			}
		}
	}
	return 0, false
}
