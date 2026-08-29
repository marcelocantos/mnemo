// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnrichFileHistory(t *testing.T) {
	home := t.TempDir()
	sess := "abc-session"
	histDir := filepath.Join(home, claudeFileHistoryRoot, sess, "deadbeef")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(histDir, "main.go"), []byte("from sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := []Op{{
		SessionID: sess, Source: "claude", Kind: KindWrite,
		Path: "/proj/main.go", Body: nil,
	}}
	warns := EnrichFileHistory(home, ops, true)
	if len(warns) != 0 {
		t.Fatalf("warns=%v", warns)
	}
	if string(ops[0].Body) != "from sidecar" {
		t.Fatalf("body=%q", ops[0].Body)
	}
	// Off by default path
	ops2 := []Op{{Kind: KindWrite, Body: nil}}
	if w := EnrichFileHistory(home, ops2, false); len(w) != 0 {
		t.Fatal("expected no enrichment when disabled")
	}
}
