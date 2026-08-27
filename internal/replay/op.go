// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package replay reconstructs agent file writes from normalised ops into a
// quarantine tree (🎯T150). See docs/design/transcript-file-replay.md.
package replay

import (
	"sort"
	"time"
)

// Kind is a normalised file mutation.
type Kind string

const (
	KindWrite  Kind = "write"
	KindPatch  Kind = "patch"
	KindDelete Kind = "delete"
)

// Outcome records how one op was handled.
type Outcome string

const (
	OutcomeApplied Outcome = "applied"
	OutcomeSkipped Outcome = "skipped"
	OutcomeFailed  Outcome = "failed"
)

// Reason codes align with docs/design/transcript-file-replay.md matrix rows.
const (
	ReasonPatchNoBase              = "patch_no_base"
	ReasonPatchAnchorMissing       = "patch_anchor_missing"
	ReasonToolUseFailed            = "tool_use_failed"
	ReasonToolResultMissing        = "tool_result_missing"
	ReasonTruncatedPayload         = "truncated_payload"
	ReasonDeleteMissing            = "delete_missing"
	ReasonRenameNotSupported       = "rename_not_supported"
	ReasonBinaryNotSupported       = "binary_not_supported"
	ReasonFileTooLarge             = "file_too_large"
	ReasonShellMutation            = "shell_mutation_not_reconstructed"
	ReasonFileHistoryMiss          = "file_history_miss"
	ReasonPathOutsideScope         = "path_outside_scope"
	ReasonPathEscape               = "path_escape"
	ReasonLiveTreeRefused          = "live_tree_refused"
	ReasonMalformedPatch           = "malformed_patch"
	ReasonDeleteNoBase             = "delete_no_base"
	ReasonToolNotSupported         = "tool_not_supported"
	ReasonSymlinkInPath            = "symlink_in_path"
	ReasonConflictSameTS           = "conflict_same_ts"
	ReasonNotebookNotSupported     = "notebook_not_supported"
)

// Op is one normalised file mutation in global timeline order.
type Op struct {
	Timestamp time.Time
	SessionID string
	Source    string
	ToolUseID string
	Path      string // absolute or relative path from transcript
	CWD       string
	Repo      string
	Kind      Kind
	Body      []byte
	OldString string
	NewString string
	PatchText string // codex apply_patch raw text; engine may expand to KindWrite/Patch/Delete
}

// OpResult is the per-op execution record.
type OpResult struct {
	Op       Op
	Outcome  Outcome
	Reason   string
	QuarKey  string // resolved quarantine-relative key when known
	FullPath string // absolute path under quarantine root when known
}

// SortOps orders ops for sequential application.
func SortOps(ops []Op) {
	sort.Slice(ops, func(i, j int) bool {
		a, b := ops[i], ops[j]
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.Before(b.Timestamp)
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.ToolUseID < b.ToolUseID
	})
}
