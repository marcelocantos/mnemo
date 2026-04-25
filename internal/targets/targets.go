// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package targets reads a repo's bullseye.yaml so the compactor can
// anchor its output in the target graph (🎯T1.4 in bullseye). Only a
// minimal slice of the schema is parsed — the compactor needs target
// IDs, names, and statuses, not the full bullseye type system. Both
// schema_version 1 and 2 produce the same shape at this depth.
package targets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the on-disk name of a repo's targets file.
const FileName = "bullseye.yaml"

// Status mirrors bullseye's status enum at the field level. Other
// values may appear in newer schemas; unknown values are preserved
// verbatim so downstream rendering doesn't lose information.
type Status string

const (
	StatusIdentified Status = "identified"
	StatusActive     Status = "active"
	StatusAchieved   Status = "achieved"
	StatusBlocked    Status = "blocked"
)

// Target is the projection of a bullseye target the compactor cares
// about. Fields outside this projection (acceptance, context, value,
// cost, etc.) are intentionally dropped — they belong to bullseye's
// own tooling, not to compaction prompts.
type Target struct {
	ID        string
	Name      string
	Status    Status
	DependsOn []string
}

// State is a snapshot of a repo's targets at compaction time. Active
// captures the targets a session is most likely to be moving on
// (anything not yet achieved); Achieved is provided so the compactor
// can recognise "T3 was retired earlier this session". FrontierIDs is
// the cheapest possible "what's next" hint — leaves of the active
// subgraph (no active depends_on).
type State struct {
	RepoRoot    string
	All         []Target
	Active      []Target
	Achieved    []Target
	FrontierIDs []string
}

// LoadFromCWD walks up from cwd looking for bullseye.yaml and returns
// a State, or (nil, nil) if no file is found. A malformed file is
// reported as an error — callers may choose to log and continue.
func LoadFromCWD(cwd string) (*State, error) {
	if cwd == "" {
		return nil, nil
	}
	path, err := discover(cwd)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	return load(path)
}

// discover walks up from start to the filesystem root searching for
// FileName. Returns "" with no error when the file is absent — a
// session whose CWD isn't under a bullseye-using repo is the common
// case, not an error.
func discover(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("abs cwd: %w", err)
	}
	for {
		candidate := filepath.Join(dir, FileName)
		info, err := os.Stat(candidate)
		switch {
		case err == nil && !info.IsDir():
			return candidate, nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// rawFile mirrors the on-disk YAML shape narrowly. Top-level fields
// outside `targets` are ignored; per-target fields outside the
// projection are dropped.
type rawFile struct {
	SchemaVersion int                  `yaml:"schema_version"`
	Targets       map[string]rawTarget `yaml:"targets"`
}

type rawTarget struct {
	Name      string   `yaml:"name"`
	Status    string   `yaml:"status"`
	DependsOn []string `yaml:"depends_on"`
}

func load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	state := &State{
		RepoRoot: filepath.Dir(path),
	}
	for id, rt := range raw.Targets {
		t := Target{
			ID:        id,
			Name:      rt.Name,
			Status:    Status(rt.Status),
			DependsOn: append([]string(nil), rt.DependsOn...),
		}
		state.All = append(state.All, t)
		if t.Status == StatusAchieved {
			state.Achieved = append(state.Achieved, t)
		} else {
			state.Active = append(state.Active, t)
		}
	}
	state.FrontierIDs = computeFrontier(state.Active)
	return state, nil
}

// computeFrontier returns IDs of active targets whose depends_on are
// either empty or reference only achieved targets. Cheap heuristic;
// bullseye's own frontier ranking (distance-to-checkpoint, fanout) is
// not duplicated here — the compactor is hinting, not deciding.
func computeFrontier(active []Target) []string {
	activeSet := make(map[string]struct{}, len(active))
	for _, t := range active {
		activeSet[t.ID] = struct{}{}
	}
	var ids []string
	for _, t := range active {
		blocked := false
		for _, dep := range t.DependsOn {
			if _, isActive := activeSet[dep]; isActive {
				blocked = true
				break
			}
		}
		if !blocked {
			ids = append(ids, t.ID)
		}
	}
	return ids
}
