// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCompactorStatusDefaultUserViaMCP is the 🎯T79 regression: when an
// MCP request omits ?user=, the per-user resolvers for
// mnemo_compactor_status (and the vault tools, covered by
// TestVaultSyncViaMCP) must fall back to the daemon's default user —
// the same fallback every store-backed tool already applies.
//
// Before the fix, the compactor resolver looked up CompactWatcherFor("")
// directly, which never matched a registered user, so the tool always
// reported "not available" on a single-user daemon. The daemon eagerly
// starts the default user's workers at boot, so once it is ready the
// watcher exists and the tool must surface real state.
func TestCompactorStatusDefaultUserViaMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e compactor status skipped under -short")
	}
	// No Options.User — the request carries no ?user= parameter.
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The default user's workers start eagerly at boot but the watcher
	// registration can lag daemon-readiness by a hair; poll briefly.
	var out string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		out, err = d.Call(ctx, "mnemo_compactor_status", nil)
		if err != nil {
			t.Fatalf("mnemo_compactor_status: %v\n%s", err, d.Log())
		}
		if strings.Contains(out, "Compactor watcher status:") {
			return // fell back to the default user and reported real state
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("mnemo_compactor_status did not fall back to the default user "+
		"without ?user= (🎯T79) — got:\n%s\n--- daemon log ---\n%s", out, d.Log())
}
