// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterCreatesFileWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")

	changed, err := Register(path, DefaultURL)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true on fresh create")
	}

	root := readJSON(t, path)
	servers, _ := root["mcpServers"].(map[string]any)
	mnemo, _ := servers["mnemo"].(map[string]any)
	if mnemo["url"] != DefaultURL || mnemo["type"] != "http" {
		t.Fatalf("unexpected entry: %#v", mnemo)
	}
}

func TestRegisterPreservesOtherKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	initial := map[string]any{
		"theme": "dark",
		"mcpServers": map[string]any{
			"other": map[string]any{"type": "stdio", "command": "/bin/other"},
		},
		"projects": map[string]any{"foo": map[string]any{"bar": 1.0}},
	}
	writeJSON(t, path, initial)

	if _, err := Register(path, DefaultURL); err != nil {
		t.Fatalf("Register: %v", err)
	}

	root := readJSON(t, path)
	if root["theme"] != "dark" {
		t.Errorf("theme not preserved: %v", root["theme"])
	}
	if _, ok := root["projects"].(map[string]any); !ok {
		t.Errorf("projects not preserved")
	}
	servers := root["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Errorf("other MCP server dropped")
	}
	if _, ok := servers["mnemo"]; !ok {
		t.Errorf("mnemo MCP server not added")
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if _, err := Register(path, DefaultURL); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	changed, err := Register(path, DefaultURL)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false on second call with same URL")
	}
}

func TestRegisterUpdatesURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if _, err := Register(path, "http://localhost:19419/mcp"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	changed, err := Register(path, "http://localhost:8080/mcp")
	if err != nil {
		t.Fatalf("Register update: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when URL differs")
	}
	root := readJSON(t, path)
	servers := root["mcpServers"].(map[string]any)
	if servers["mnemo"].(map[string]any)["url"] != "http://localhost:8080/mcp" {
		t.Errorf("URL not updated")
	}
}

func TestUnregisterRemovesEntryAndEmptyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if _, err := Register(path, DefaultURL); err != nil {
		t.Fatalf("Register: %v", err)
	}
	changed, err := Unregister(path)
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	root := readJSON(t, path)
	if _, ok := root["mcpServers"]; ok {
		t.Errorf("expected empty mcpServers to be removed, got %v", root["mcpServers"])
	}
}

func TestUnregisterPreservesOtherServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"type": "stdio", "command": "/bin/other"},
			"mnemo": map[string]any{"type": "http", "url": DefaultURL},
		},
	})
	if _, err := Unregister(path); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	root := readJSON(t, path)
	servers := root["mcpServers"].(map[string]any)
	if _, ok := servers["mnemo"]; ok {
		t.Errorf("mnemo not removed")
	}
	if _, ok := servers["other"]; !ok {
		t.Errorf("other server removed")
	}
}

func TestUnregisterMissingFileIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	changed, err := Unregister(path)
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false for missing file")
	}
}

func TestUnregisterWhenAbsentIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"type": "stdio", "command": "/bin/other"},
		},
	})
	changed, err := Unregister(path)
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false when mnemo entry absent")
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root
}

func writeJSON(t *testing.T, path string, root map[string]any) {
	t.Helper()
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}
