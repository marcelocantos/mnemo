// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"testing"
)

func TestNormalizeAgentToolInput(t *testing.T) {
	in := []byte(`{"target_file":"/tmp/a.go","limit":10}`)
	out := normalizeAgentToolInput(in)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["file_path"] != "/tmp/a.go" {
		t.Errorf("file_path = %v, want /tmp/a.go", m["file_path"])
	}
	if m["target_file"] != "/tmp/a.go" {
		t.Errorf("original target_file should be preserved, got %v", m["target_file"])
	}

	// command alias
	in2 := []byte(`{"cmd":"go test"}`)
	out2 := normalizeAgentToolInput(in2)
	var m2 map[string]any
	_ = json.Unmarshal(out2, &m2)
	if m2["command"] != "go test" {
		t.Errorf("command = %v", m2["command"])
	}

	// already has file_path — no clobber
	in3 := []byte(`{"file_path":"/a","target_file":"/b"}`)
	out3 := normalizeAgentToolInput(in3)
	var m3 map[string]any
	_ = json.Unmarshal(out3, &m3)
	if m3["file_path"] != "/a" {
		t.Errorf("should keep existing file_path, got %v", m3["file_path"])
	}
}
