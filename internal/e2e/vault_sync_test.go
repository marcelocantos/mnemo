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
//  1. Launch a daemon under MNEMO_HOME=<tempdir>.
//  2. Hot-reload via mnemo_config(op=write, patch={vault_path: ...}).
//  3. Invoke mnemo_vault_sync via MCP.
//  4. Assert <vault>/index.md exists with the standard fence
//     contract (every mnemo-owned vault note carries the
//     <!-- mnemo:generated --> marker).
//  5. Assert mnemo_vault_status reports the vault path back.
func TestVaultSyncViaMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e vault sync skipped under -short")
	}
	// 🎯T79: omit ?user= — the vault resolver now falls back to the
	// default user just like every other tool, so the explicit-user
	// workaround this test used to need is gone.
	d := Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Tempdir for the vault, outside MNEMO_HOME so the test verifies
	// the daemon honours the configured path rather than a hidden
	// default.
	vaultDir := filepath.Join(d.Home, "vault")

	// Hot-reload via mnemo_config(op=write). The patch shape is the
	// same as ~/.mnemo/config.json — passed as a nested JSON object,
	// not a string.
	cfgOut, err := d.Call(ctx, "mnemo_config", map[string]any{
		"op":    "write",
		"patch": map[string]any{"vault_path": vaultDir},
	})
	if err != nil {
		t.Fatalf("mnemo_config write: %v\n%s", err, d.Log())
	}
	if !strings.Contains(cfgOut, "vault_path") && !strings.Contains(cfgOut, "applied") {
		t.Logf("mnemo_config output (informational):\n%s", cfgOut)
	}

	// The mnemo_config hot-reload kicks off an automatic vault sync
	// in the background. An explicit mnemo_vault_sync called too soon
	// may coalesce with "already in flight, skipping." To handle both
	// the coalescing case and the case where the background sync has
	// not yet started, we retry mnemo_vault_sync until index.md
	// appears with the expected fence. Each retry attempt that returns
	// a coalescing message is followed by a short sleep to let the
	// in-flight sync finish before we look for the file.
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
			_, _ = d.Call(ctx, "mnemo_vault_sync", nil)
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

	// mnemo_vault_status must reach the configured path through the
	// same MCP transport.
	statusOut, err := d.Call(ctx, "mnemo_vault_status", nil)
	if err != nil {
		t.Fatalf("mnemo_vault_status: %v\n%s", err, d.Log())
	}
	if !strings.Contains(statusOut, vaultDir) {
		t.Errorf("mnemo_vault_status did not surface vault_path %q:\n%s",
			vaultDir, statusOut)
	}
}
