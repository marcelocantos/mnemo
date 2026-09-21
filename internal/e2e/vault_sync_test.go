// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestVaultSyncViaMCP is the e2e port (🎯T73 acceptance #7) of the
// vault-sync integration tests that historically lived in
// internal/vault/vault_test.go (TestExporterSyncCreatesFiles and
// friends). Those tests called the in-process Go API directly. This
// version drives the same flow through the real MCP transport
// against a running daemon — so failures in the per-user registry
// routing, the MCP serialisation layer, or the vault MCP tool
// wrapper become testable.
//
// Flow:
//  1. Write ~/.mnemo/config.json with vault_path (file-only config,
//     🎯T156) and launch a daemon under MNEMO_HOME=<tempdir>.
//  2. Invoke mnemo_vault(op=sync) via MCP.
//  3. Assert <vault>/index.md exists with the standard fence
//     contract (every mnemo-owned vault note carries the
//     <!-- mnemo:generated --> marker).
//  4. Assert mnemo_vault(op=status) reports the vault path back.
func TestVaultSyncViaMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e vault sync skipped under -short")
	}
	home := t.TempDir()
	// Tempdir for the vault, outside MNEMO_HOME/.mnemo so the test
	// verifies the daemon honours the configured path rather than a
	// hidden default.
	vaultDir := filepath.Join(home, "vault")
	cfgDir := filepath.Join(home, ".mnemo")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(map[string]string{"vault_path": vaultDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	// 🎯T79: omit ?user= — the vault resolver now falls back to the
	// default user just like every other tool, so the explicit-user
	// workaround this test used to need is gone.
	d := Start(t, Options{Home: home})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Boot with vault_path set starts an automatic vault sync in the
	// background. An explicit mnemo_vault(op=sync) called too soon
	// may coalesce with "already in flight, skipping." To handle both
	// the coalescing case and the case where the background sync has
	// not yet started, we retry until index.md appears with the
	// expected fence. Each retry attempt that returns a coalescing
	// message is followed by a short sleep to let the in-flight sync
	// finish before we look for the file.
	rootIndex := filepath.Join(vaultDir, "index.md")
	var body []byte
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(rootIndex); readErr == nil {
			if strings.Contains(string(data), "mnemo:generated") {
				body = data
				break
			}
			// File exists but fence not yet written (partial write or
			// background sync still in progress). Wait and retry.
		} else {
			// File does not exist yet — trigger a sync attempt; ignore
			// coalescing-skip responses, they mean one is already in
			// flight.
			_, _ = d.Call(ctx, "mnemo_vault", map[string]any{"op": "sync"})
		}
		time.Sleep(250 * time.Millisecond)
	}
	if body == nil {
		// One final read to capture whatever is on disk for the error
		// message, even if it lacks the fence.
		if data, readErr := os.ReadFile(rootIndex); readErr == nil {
			t.Fatalf("vault index.md appeared but is missing the <!-- mnemo:generated --> fence — "+
				"contract not honoured through MCP transport:\n%s\n--- daemon log ---\n%s",
				data, d.Log())
		}
		t.Fatalf("vault index.md never appeared at %q:\n--- daemon log ---\n%s",
			rootIndex, d.Log())
	}

	// mnemo_vault(op=status) must reach the configured path through the
	// same MCP transport.
	statusOut, err := d.Call(ctx, "mnemo_vault", map[string]any{"op": "status"})
	if err != nil {
		t.Fatalf("mnemo_vault op=status: %v\n%s", err, d.Log())
	}
	if !strings.Contains(statusOut, vaultDir) {
		t.Errorf("mnemo_vault op=status did not surface vault_path %q:\n%s",
			vaultDir, statusOut)
	}
}
