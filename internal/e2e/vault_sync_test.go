// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"os/user"
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
	// Pass ?user= explicitly. mnemo's vault resolver bypasses the
	// default-user fallback that the store resolver applies, so a
	// vault-touching test that omits ?user= sees "vault not
	// configured." Setting it explicitly is the documented usage and
	// avoids the asymmetry.
	cur, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}
	d := Start(t, Options{User: cur.Username})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	// in the background; an explicit mnemo_vault_sync called too
	// soon coalesces with "already in flight, skipping." Either way
	// the root index.md should appear shortly. Poll with a bounded
	// timeout so the race resolves cleanly.
	_, _ = d.Call(ctx, "mnemo_vault_sync", nil) // best-effort; coalescing is fine

	rootIndex := filepath.Join(vaultDir, "index.md")
	var body []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(rootIndex); readErr == nil {
			body = data
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if body == nil {
		t.Fatalf("vault index.md never appeared at %q:\n--- daemon log ---\n%s",
			rootIndex, d.Log())
	}
	if !strings.Contains(string(body), "mnemo:generated") {
		t.Errorf("vault index.md missing the <!-- mnemo:generated --> fence — "+
			"contract not honoured through MCP transport:\n%s", body)
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
