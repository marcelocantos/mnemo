// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds runtime configuration loaded from ~/.mnemo/config.json.
//
// This file is optional. If absent, sensible defaults apply. Its purpose
// is to let the daemon discover repos and project directories that live
// outside the places mnemo would guess on its own (~/work for repos,
// ~/.claude/projects for transcripts).
type Config struct {
	// WorkspaceRoots are filesystem roots under which repo-level streams
	// (targets, audit logs, plans, CLAUDE.md, CI) discover repositories.
	// Each root is walked for .git entries to identify repos. An empty
	// list falls back to DefaultWorkspaceRoots.
	WorkspaceRoots []string `json:"workspace_roots,omitempty"`

	// ExtraProjectDirs lists extra Claude Code project directories to
	// index beyond ~/.claude/projects/. Reserved for cross-platform
	// transcript ingest (see 🎯T15).
	ExtraProjectDirs []string `json:"extra_project_dirs,omitempty"`
}

// LoadConfig reads ~/.mnemo/config.json. Returns a zero Config if the
// file doesn't exist. Returns an error only on parse failure, so a
// missing config never prevents startup.
func LoadConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	return loadConfigFrom(filepath.Join(home, ".mnemo", "config.json"))
}

func loadConfigFrom(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// DefaultWorkspaceRoots returns the default workspace roots: [~/work].
// This matches the convention used across the global CLAUDE.md for
// Go-style repo layouts (~/work/github.com/org/repo).
func DefaultWorkspaceRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "work")}
}

// ResolvedWorkspaceRoots returns the WorkspaceRoots as configured, or
// DefaultWorkspaceRoots if none are set.
func (c Config) ResolvedWorkspaceRoots() []string {
	if len(c.WorkspaceRoots) == 0 {
		return DefaultWorkspaceRoots()
	}
	return c.WorkspaceRoots
}
