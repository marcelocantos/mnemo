// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	// index beyond ~/.claude/projects/. Used for cross-platform
	// transcript ingest (🎯T15) — e.g. a Windows VM's Claude projects
	// dir exposed via SMB mount. Missing or unavailable entries are
	// skipped at ingest/watch time rather than failing.
	ExtraProjectDirs []string `json:"extra_project_dirs,omitempty"`

	// SynthesisRoots are filesystem roots walked by the synthesis-doc
	// indexer (🎯T34) to index analysis/research/design/planning docs
	// under docs/{papers,design,analysis,plans}/ plus docs/audit-log.md
	// and docs/convergence-report.md. Unlike WorkspaceRoots, these roots
	// do not require a .git marker — suitable for non-repo planning
	// spaces such as ~/think. Entries support ~ for the user's home.
	// An empty list disables synthesis-doc ingest (repo-level docs are
	// still indexed via WorkspaceRoots + IngestDocs).
	SynthesisRoots []string `json:"synthesis_roots,omitempty"`
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

// ResolvedSynthesisRoots returns SynthesisRoots with ~ expanded to the
// user's home directory. Unset entries return an empty slice (the
// indexer skips synthesis ingest entirely when no roots are configured;
// there is no default, unlike WorkspaceRoots).
func (c Config) ResolvedSynthesisRoots() []string {
	if len(c.SynthesisRoots) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(c.SynthesisRoots))
	for _, r := range c.SynthesisRoots {
		if r == "" {
			continue
		}
		if home != "" {
			switch {
			case r == "~":
				r = home
			case strings.HasPrefix(r, "~/"):
				r = filepath.Join(home, r[2:])
			}
		}
		out = append(out, r)
	}
	return out
}
