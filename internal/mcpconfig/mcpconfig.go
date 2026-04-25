// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package mcpconfig patches a Claude Code user config file
// (~/.claude.json) to register or unregister mnemo as an MCP server.
//
// The Windows installer uses this to avoid forcing non-technical users
// to run `claude mcp add` from a terminal. It is cross-platform and
// also useful to anyone wanting a one-shot registration.
//
// The file is read, parsed as generic JSON, mutated in place, and
// written back via a temp-file rename so a crashed write cannot leave
// a corrupt config. All top-level keys and all other mcpServers
// entries are preserved verbatim — we only touch the "mnemo" key.
package mcpconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultURL is the HTTP MCP endpoint used when no user identity is
// available. Most callers should prefer URLForUser, which embeds an
// explicit `?user=<name>` so the daemon's per-user Registry resolves
// to the right home directory.
const DefaultURL = "http://localhost:19419/mcp"

// URLForUser returns the MCP endpoint with `?user=<username>`
// appended, suitable for writing into ~/.claude.json. The caller
// resolves the username (typically the OS current user) before
// calling. The daemon's HTTPContextFunc strips the parameter into a
// per-request ctx value used by the Registry.
func URLForUser(username string) string {
	if username == "" {
		return DefaultURL
	}
	return DefaultURL + "?user=" + username
}

// ConfigPath returns the Claude Code user config file path.
// On every supported platform this is ~/.claude.json.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// Register adds or updates the "mnemo" entry under mcpServers in the
// given config file, pointing at url. Other keys are preserved. If
// the file does not exist it is created with a minimal
// {"mcpServers": {"mnemo": ...}} shape. The operation is idempotent.
// Returns true if the file was changed.
func Register(path, url string) (bool, error) {
	raw, err := readOrEmpty(path)
	if err != nil {
		return false, err
	}

	root := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return false, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	want := map[string]any{"type": "http", "url": url}
	if existing, ok := servers["mnemo"].(map[string]any); ok && mapsEqual(existing, want) {
		return false, nil
	}
	servers["mnemo"] = want
	root["mcpServers"] = servers

	return true, writeAtomic(path, root)
}

// Unregister removes the "mnemo" entry under mcpServers. If mcpServers
// becomes empty it is removed too. Idempotent. Returns true if the
// file was changed. A missing file is not an error.
func Unregister(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	root := map[string]any{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}

	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, present := servers["mnemo"]; !present {
		return false, nil
	}
	delete(servers, "mnemo")
	if len(servers) == 0 {
		delete(root, "mcpServers")
	} else {
		root["mcpServers"] = servers
	}

	return true, writeAtomic(path, root)
}

func readOrEmpty(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, nil
}

// writeAtomic marshals root with two-space indentation (matching the
// format Claude Code uses) and replaces path atomically via a temp
// file in the same directory. File permissions default to 0600 since
// ~/.claude.json may contain OAuth tokens; existing permissions are
// preserved if the file is already there.
func writeAtomic(path string, root map[string]any) error {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	out = append(out, '\n')

	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
