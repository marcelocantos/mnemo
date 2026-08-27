// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"encoding/json"
	"strings"
	"time"
)

// shellTools are never reconstructed (E11).
var shellTools = map[string]struct{}{
	"Bash": {}, "Shell": {}, "shell": {}, "exec_command": {},
	"run_terminal_command": {}, "local_shell_call": {},
}

// ToolRow is one indexed tool_use candidate from the database.
type ToolRow struct {
	Timestamp   time.Time
	SessionID   string
	Source      string
	ToolName    string
	ToolUseID   string
	ToolInput   []byte
	Text        string
	FilePath    string
	ToolContent string // messages.tool_content generated col
	OldString   string // messages.tool_old_string
	NewString   string // messages.tool_new_string
	CWD         string
	Repo        string
	ResultError *bool // nil = no paired result
}

// OpFromToolRow maps an indexed tool_use to zero or one normalised op(s).
// Codex apply_patch may expand to multiple ops via PatchText.
func OpFromToolRow(row ToolRow) ([]Op, string) {
	if _, skip := shellTools[row.ToolName]; skip {
		return nil, ReasonShellMutation
	}
	if row.ResultError != nil && *row.ResultError {
		return nil, ReasonToolUseFailed
	}

	base := Op{
		Timestamp: row.Timestamp,
		SessionID: row.SessionID,
		Source:    row.Source,
		ToolUseID: row.ToolUseID,
		Path:      row.FilePath,
		CWD:       row.CWD,
		Repo:      row.Repo,
	}

	switch row.ToolName {
	case "Write":
		body, ok := writeBody(row.ToolInput)
		if !ok && row.ToolContent != "" {
			body, ok = []byte(row.ToolContent), true
		}
		if !ok || len(body) == 0 {
			return nil, ReasonTruncatedPayload
		}
		base.Kind = KindWrite
		base.Body = body
		if base.Path == "" {
			base.Path = pathFromInput(row.ToolInput)
		}
		return []Op{base}, ""

	case "Edit", "StrReplace", "search_replace":
		oldS, newS, path, ok := patchFields(row.ToolInput)
		if !ok && (row.OldString != "" || row.NewString != "") {
			oldS, newS, path, ok = row.OldString, row.NewString, row.FilePath, true
		}
		if !ok {
			return nil, ReasonTruncatedPayload
		}
		base.Kind = KindPatch
		base.OldString = oldS
		base.NewString = newS
		if path != "" {
			base.Path = path
		}
		if base.Path == "" {
			base.Path = row.FilePath
		}
		return []Op{base}, ""

	case "write":
		body, ok := writeBody(row.ToolInput)
		if !ok && row.ToolContent != "" {
			body, ok = []byte(row.ToolContent), true
		}
		if !ok || len(body) == 0 {
			return nil, ReasonTruncatedPayload
		}
		base.Kind = KindWrite
		base.Body = body
		if base.Path == "" {
			base.Path = pathFromInput(row.ToolInput)
		}
		return []Op{base}, ""

	case "Delete":
		base.Kind = KindDelete
		if base.Path == "" {
			base.Path = row.FilePath
		}
		if base.Path == "" {
			return nil, ReasonTruncatedPayload
		}
		return []Op{base}, ""

	case "apply_patch":
		text := strings.TrimSpace(row.Text)
		if text == "" {
			return nil, ReasonTruncatedPayload
		}
		base.PatchText = text
		base.Kind = KindPatch // placeholder; ExpandCodexPatch replaces
		ops := ExpandCodexPatch(base, text)
		if len(ops) == 0 {
			return nil, ReasonMalformedPatch
		}
		return ops, ""

	case "NotebookEdit":
		return nil, ReasonNotebookNotSupported

	default:
		return nil, ReasonToolNotSupported
	}
}

func writeBody(raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	for _, k := range []string{"content", "file_text", "contents", "text", "body"} {
		if v, ok := m[k].(string); ok && v != "" {
			return []byte(v), true
		}
	}
	return nil, false
}

func pathFromInput(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "path", "target_file", "filePath"} {
		if v, ok := m[k].(string); ok {
			return v
		}
	}
	return ""
}

func patchFields(raw []byte) (old, new, path string, ok bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", "", "", false
	}
	old, _ = m["old_string"].(string)
	new, _ = m["new_string"].(string)
	path = pathFromInput(raw)
	if old == "" && new == "" {
		return "", "", "", false
	}
	return old, new, path, true
}

// ReplayToolNames lists tool_name values that may produce file ops.
func ReplayToolNames() []string {
	return []string{
		"Write", "Edit", "StrReplace", "Delete",
		"write", "search_replace", "apply_patch",
	}
}
