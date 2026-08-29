// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config controls engine behaviour.
type Config struct {
	DryRun         bool
	AllowLiveTree  bool
	MaxWriteBytes  int
	ContinueOnFail bool // E14: default true
}

// DefaultConfig returns MVP defaults from the design note.
func DefaultConfig() Config {
	return Config{
		MaxWriteBytes:  maxWriteBytes,
		ContinueOnFail: true,
	}
}

// Report summarises a replay run.
type Report struct {
	RunID          string
	QuarantineRoot string
	DryRun         bool
	OpsPlanned     int
	OpsApplied     int
	OpsSkipped     int
	OpsFailed      int
	FilesWritten   map[string]struct{}
	Seeds          []Seed
	Results        []OpResult
	Warnings       []string
}

// Engine applies normalised ops to a quarantine root.
type Engine struct {
	cfg Config
}

func NewEngine(cfg Config) *Engine {
	if cfg.MaxWriteBytes <= 0 {
		cfg.MaxWriteBytes = maxWriteBytes
	}
	return &Engine{cfg: cfg}
}

// Run validates the quarantine root and applies ops in order.
// Optional seeds preload quarantine keys before the op timeline (forensics pre-images).
func (e *Engine) Run(root string, ops []Op, seeds ...Seed) (*Report, error) {
	if e.cfg.MaxWriteBytes <= 0 {
		e.cfg.MaxWriteBytes = maxWriteBytes
	}
	root = filepath.Clean(root)
	report := &Report{
		QuarantineRoot: root,
		DryRun:         e.cfg.DryRun,
		FilesWritten:   make(map[string]struct{}),
		Seeds:          append([]Seed(nil), seeds...),
	}

	if inside, gitRoot := IsInsideGitWorkTree(root); inside && !e.cfg.AllowLiveTree {
		return report, fmt.Errorf("%s: quarantine root inside git work tree %s (use --allow-live-tree to override)", ReasonLiveTreeRefused, gitRoot)
	}

	expanded := e.expandOps(ops)
	SortOps(expanded)
	report.OpsPlanned = len(expanded)

	// Virtual FS for dry-run and for patch base detection.
	files := SeedsToQuarantine(seeds, ops)
	if !e.cfg.DryRun {
		for key, body := range files {
			full, err := QuarantinePath(root, key)
			if err != nil {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(full), 0o755)
			_ = os.WriteFile(full, body, 0o644)
			report.FilesWritten[key] = struct{}{}
		}
	}

	var lastTSKey = make(map[string]time.Time)

	for _, op := range expanded {
		res := e.applyOne(root, op, files, lastTSKey)
		report.Results = append(report.Results, res)
		switch res.Outcome {
		case OutcomeApplied:
			report.OpsApplied++
			if res.QuarKey != "" {
				report.FilesWritten[res.QuarKey] = struct{}{}
			}
		case OutcomeSkipped:
			report.OpsSkipped++
		case OutcomeFailed:
			report.OpsFailed++
			if !e.cfg.ContinueOnFail {
				break
			}
		}
	}

	return report, nil
}

func (e *Engine) expandOps(ops []Op) []Op {
	var out []Op
	for _, op := range ops {
		if op.Kind == KindPatch && op.PatchText != "" {
			out = append(out, ExpandCodexPatch(op, op.PatchText)...)
			continue
		}
		out = append(out, op)
	}
	return out
}

func (e *Engine) applyOne(root string, op Op, files map[string][]byte, lastTSKey map[string]time.Time) OpResult {
	res := OpResult{Op: op}

	key, ok := ResolveKey(op.Path, op.CWD, op.Repo)
	if !ok {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonPathOutsideScope
		return res
	}
	res.QuarKey = key

	full, err := QuarantinePath(root, key)
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = ReasonPathEscape
		return res
	}
	res.FullPath = full

	if !e.cfg.DryRun && pathHasSymlinkComponent(root, full) {
		res.Outcome = OutcomeFailed
		res.Reason = ReasonSymlinkInPath
		return res
	}

	ts := op.Timestamp.Truncate(time.Millisecond)
	if prev, ok := lastTSKey[key]; ok && prev.Equal(ts) {
		// recorded by caller via res — conflict is warn-only per E22
	}
	lastTSKey[key] = ts

	switch op.Kind {
	case KindWrite:
		return e.applyWrite(root, full, key, op, files, res)
	case KindPatch:
		return e.applyPatch(full, key, op, files, res)
	case KindDelete:
		return e.applyDelete(full, key, files, res)
	default:
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonToolNotSupported
		return res
	}
}

func (e *Engine) applyWrite(root, full, key string, op Op, files map[string][]byte, res OpResult) OpResult {
	if len(op.Body) == 0 {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonTruncatedPayload
		return res
	}
	if len(op.Body) > e.cfg.MaxWriteBytes {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonFileTooLarge
		return res
	}
	if containsNUL(op.Body) {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonBinaryNotSupported
		return res
	}
	files[key] = append([]byte(nil), op.Body...)
	if e.cfg.DryRun {
		res.Outcome = OutcomeApplied
		return res
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = err.Error()
		return res
	}
	if err := os.WriteFile(full, op.Body, 0o644); err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = err.Error()
		return res
	}
	res.Outcome = OutcomeApplied
	return res
}

func (e *Engine) applyPatch(full, key string, op Op, files map[string][]byte, res OpResult) OpResult {
	cur, ok := files[key]
	if !ok {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonPatchNoBase
		return res
	}
	if op.OldString == "" && op.NewString == "" {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonTruncatedPayload
		return res
	}
	s := string(cur)
	if !strings.Contains(s, op.OldString) {
		res.Outcome = OutcomeSkipped
		res.Reason = ReasonPatchAnchorMissing
		return res
	}
	updated := strings.Replace(s, op.OldString, op.NewString, 1)
	files[key] = []byte(updated)
	if e.cfg.DryRun {
		res.Outcome = OutcomeApplied
		return res
	}
	if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
		res.Outcome = OutcomeFailed
		res.Reason = err.Error()
		return res
	}
	res.Outcome = OutcomeApplied
	return res
}

func (e *Engine) applyDelete(full, key string, files map[string][]byte, res OpResult) OpResult {
	if _, ok := files[key]; !ok {
		if !e.cfg.DryRun {
			if _, err := os.Stat(full); os.IsNotExist(err) {
				res.Outcome = OutcomeApplied
				res.Reason = ReasonDeleteMissing
				return res
			}
		} else {
			res.Outcome = OutcomeApplied
			res.Reason = ReasonDeleteMissing
			return res
		}
	}
	delete(files, key)
	if e.cfg.DryRun {
		res.Outcome = OutcomeApplied
		return res
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		res.Outcome = OutcomeFailed
		res.Reason = err.Error()
		return res
	}
	res.Outcome = OutcomeApplied
	return res
}

func containsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
