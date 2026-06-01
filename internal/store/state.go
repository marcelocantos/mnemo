// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateVersion is the schema version the current daemon binary
// understands. A state.json file with a higher version makes the
// daemon enter read-only state mode (see LoadState).
const StateVersion = 1

// State is the daemon-managed sidecar at ~/.mnemo/state.json. It
// holds runtime-derived state that must survive daemon restarts but
// does not belong in user-editable config.json. The struct mirrors
// the schema documented under "Daemon-managed state file" in
// docs/design/vault-library-wing.md (🎯T64.2).
//
// Forward compatibility: unknown top-level keys at the same Version
// are read into Extras and written back verbatim on every rewrite, so
// a downgrade-then-upgrade cycle never silently loses a newer daemon's
// additions.
//
// Read-only state mode: a state.json with Version > StateVersion is
// loaded but never rewritten (ReadOnly == true). This protects newer
// state from being truncated by an older daemon that does not
// understand it.
type State struct {
	Version                int                    `json:"version"`
	VaultPath              string                 `json:"vault_path,omitempty"`
	VaultLayoutFirstSeen   LayoutFirstSeen        `json:"vault_layout_first_seen"`
	IndexingScopeFirstSeen IndexingScopeFirstSeen `json:"indexing_scope_first_seen"`
	EmbeddingFingerprint   *EmbeddingFingerprint  `json:"embedding_fingerprint"`
	LastClusterRunID       int64                  `json:"last_cluster_run_id,omitempty"`
	BrokenTemplates        []BrokenTemplate       `json:"broken_templates"`
	BridgeErrors           []BridgeError          `json:"bridge_errors"`

	// LastSoakWarnAt is the timestamp of the most recent soak-window
	// warning emit. The exporter consults this to enforce a weekly
	// cadence: a warning fires at most every 7 days, never on every
	// sync. Zero means no warning has been emitted yet. Additive to
	// the design schema (🎯T64.2; future slices may rename if a
	// general warning-cadence table is introduced).
	LastSoakWarnAt time.Time `json:"last_soak_warn_at,omitempty"`

	// MigrationDocWrittenAt records when _mnemo/MIGRATION.md was
	// first written by mnemo. Non-zero means "do not write again" —
	// the write-once rule treats deletion by the user as "I have
	// read this." mnemo_vault_migration_doc(write: true) provides
	// the only path to a subsequent regen. Additive to the design
	// schema (🎯T64.2).
	MigrationDocWrittenAt time.Time `json:"migration_doc_written_at,omitempty"`

	// Extras holds any top-level keys the current binary does not
	// recognise. They are preserved verbatim across rewrites to
	// support forward compatibility (a newer daemon writes a field,
	// the user downgrades, the older daemon stamps vault_layout_
	// first_seen but does not drop the unknown field).
	Extras map[string]json.RawMessage `json:"-"`

	// ReadOnly is set when LoadState detected Version > StateVersion.
	// Write() short-circuits to a no-op in this mode so an older
	// daemon does not corrupt newer state. Callers may inspect the
	// recorded data freely but transitions are not persisted.
	ReadOnly bool `json:"-"`
}

// LayoutFirstSeen records when each vault_layout value was first
// observed on the recorded vault_path. A nil pointer means "never
// observed yet." Stamping is idempotent — the first observation wins
// and a later sync in the same layout does not overwrite.
type LayoutFirstSeen struct {
	V1   *time.Time `json:"v1"`
	Both *time.Time `json:"both"`
	V2   *time.Time `json:"v2"`
}

// IndexingScopeFirstSeen mirrors LayoutFirstSeen for the indexing
// scope (🎯T64.1). Stamped by future slices; T64.2 only initialises
// the structure so the schema-on-disk matches the design doc.
type IndexingScopeFirstSeen struct {
	MnemoOnly *time.Time `json:"_mnemo_only"`
	Full      *time.Time `json:"full"`
	Includes  *time.Time `json:"includes"`
}

// EmbeddingFingerprint is populated when the clustering engine
// transitions from "heuristic" to "embeddings" (later slices).
type EmbeddingFingerprint struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Version  string    `json:"version"`
	LastUsed time.Time `json:"last_used"`
}

// BrokenTemplate / BridgeError shapes are placeholders for later
// slices; populated empty here so the on-disk schema matches the
// design from day one.
type BrokenTemplate struct {
	TemplatePath string `json:"template_path"`
	EntityType   string `json:"entity_type"`
	Phase        string `json:"phase"`
	Error        string `json:"error"`
}

type BridgeError struct {
	Name       string `json:"name"`
	AnchorPath string `json:"anchor_path"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
}

// knownStateKeys is the set of JSON keys the current binary writes
// itself. Anything else loaded from disk lands in Extras.
var knownStateKeys = map[string]struct{}{
	"version":                   {},
	"vault_path":                {},
	"vault_layout_first_seen":   {},
	"indexing_scope_first_seen": {},
	"embedding_fingerprint":     {},
	"last_cluster_run_id":       {},
	"broken_templates":          {},
	"bridge_errors":             {},
	"last_soak_warn_at":         {},
	"migration_doc_written_at":  {},
}

// DefaultStatePath returns ~/.mnemo/state.json for the calling user's
// home directory.
func DefaultStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mnemo", "state.json"), nil
}

// NewState returns a freshly-initialised State for a brand-new
// daemon. All first-seen fields are nil; arrays are empty (not nil)
// so the on-disk JSON shows [] rather than null.
func NewState() *State {
	return &State{
		Version:         StateVersion,
		BrokenTemplates: []BrokenTemplate{},
		BridgeErrors:    []BridgeError{},
	}
}

// LoadState reads state.json from path. A missing file returns a
// fresh State with version=StateVersion and ReadOnly=false. A file
// with version > StateVersion returns the loaded data with ReadOnly=
// true so the daemon can read but not corrupt newer state.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state.json: %w", err)
	}
	// Two-pass parse: first into the named struct, then into a
	// map[string]json.RawMessage to capture unknown keys.
	s := NewState()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse state.json (raw): %w", err)
	}
	for k, v := range raw {
		if _, known := knownStateKeys[k]; known {
			continue
		}
		if s.Extras == nil {
			s.Extras = map[string]json.RawMessage{}
		}
		s.Extras[k] = v
	}
	if s.Version > StateVersion {
		s.ReadOnly = true
	}
	// Promote nil arrays to empty so the JSON shape stays stable
	// across rewrites.
	if s.BrokenTemplates == nil {
		s.BrokenTemplates = []BrokenTemplate{}
	}
	if s.BridgeErrors == nil {
		s.BridgeErrors = []BridgeError{}
	}
	return s, nil
}

// Write persists state to path using a sibling tmp file + fsync +
// rename. In ReadOnly mode (loaded Version > StateVersion), Write
// is a no-op so newer state cannot be corrupted by an older daemon.
//
// Unknown top-level keys captured at load time are overlaid back
// into the output so forward compatibility holds.
func (s *State) Write(path string) error {
	if s.ReadOnly {
		return nil
	}
	if s.Version == 0 {
		s.Version = StateVersion
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	// Marshal the known fields, then overlay extras so they appear
	// alongside in the output.
	knownBytes, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(knownBytes, &merged); err != nil {
		return fmt.Errorf("re-parse state: %w", err)
	}
	for k, v := range s.Extras {
		if _, clash := knownStateKeys[k]; clash {
			continue
		}
		merged[k] = v
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal merged: %w", err)
	}
	out = append(out, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state.json.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename tmp: %w", err)
	}
	return nil
}

// StampLayoutFirstSeen records `now` as the first observation of
// `layout` if no observation exists yet. The first stamp wins; later
// syncs in the same layout do not overwrite.
//
// Returns true if a new stamp was written, false if the field was
// already populated.
func (s *State) StampLayoutFirstSeen(layout string, now time.Time) bool {
	field := s.layoutFieldPtr(layout)
	if field == nil || *field != nil {
		return false
	}
	t := now.UTC()
	*field = &t
	return true
}

// LayoutFirstSeen returns the first-observed timestamp for `layout`,
// or zero time if never observed.
func (s *State) LayoutFirstSeen(layout string) time.Time {
	field := s.layoutFieldPtr(layout)
	if field == nil || *field == nil {
		return time.Time{}
	}
	return **field
}

func (s *State) layoutFieldPtr(layout string) **time.Time {
	switch layout {
	case VaultLayoutV1:
		return &s.VaultLayoutFirstSeen.V1
	case VaultLayoutBoth:
		return &s.VaultLayoutFirstSeen.Both
	case VaultLayoutV2:
		return &s.VaultLayoutFirstSeen.V2
	}
	return nil
}

// ResetVaultLayoutFirstSeen clears all first-seen timestamps. Used
// when the vault_path changes (different vault → counters reset).
func (s *State) ResetVaultLayoutFirstSeen() {
	s.VaultLayoutFirstSeen = LayoutFirstSeen{}
}
