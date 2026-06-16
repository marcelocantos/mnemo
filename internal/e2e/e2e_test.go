// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSmoke launches a daemon with an empty MNEMO_HOME tempdir,
// confirms the MCP transport is reachable, and round-trips one tool
// call (mnemo_stats — a read-only call that works against an empty
// index). Proves the harness end-to-end. Skip on -short.
func TestSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e smoke test skipped under -short")
	}
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := d.Call(ctx, "mnemo_stats", nil)
	if err != nil {
		t.Fatalf("mnemo_stats: %v\n--- daemon log ---\n%s", err, d.Log())
	}
	if out == "" {
		t.Fatal("mnemo_stats returned empty body")
	}
	// mnemo_stats response should mention "sessions" or "entries"
	// somewhere in its rendering. Loose check; we're verifying the
	// channel works, not the tool's specific output shape.
	if !strings.Contains(strings.ToLower(out), "session") &&
		!strings.Contains(strings.ToLower(out), "entries") {
		t.Errorf("mnemo_stats output unexpected:\n%s", out)
	}
}

// TestIsolation verifies the 🎯T73 keystone invariant: a daemon
// launched with MNEMO_HOME=<tempdir> writes its state under that
// tempdir, NOT under the real ~/.mnemo. Belt-and-braces against the
// "test accidentally hits live data" failure mode.
func TestIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e isolation test skipped under -short")
	}
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Issue any tool call so the daemon definitely touches its
	// data root; mnemo_stats is cheap.
	if _, err := d.Call(ctx, "mnemo_stats", nil); err != nil {
		t.Fatalf("mnemo_stats: %v\n%s", err, d.Log())
	}
	d.Stop() // flush + close DB so the file is observable

	// The daemon's database must live under Home/.mnemo/mnemo.db.
	dbPath := filepath.Join(d.Home, ".mnemo", "mnemo.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("expected daemon DB at %q (under MNEMO_HOME): %v", dbPath, err)
	}
}
