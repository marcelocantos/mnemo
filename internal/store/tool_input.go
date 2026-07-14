// Shared tool-input normalisation for non-Claude agents (🎯T111).
// Claude stores tool_use.input with file_path / command keys that feed
// generated columns (tool_file_path, tool_command). Grok (and others)
// use target_file / path / cmd. Normalise before jsonb() so who_ran and
// file-path queries work across sources.
package store

import (
	"encoding/json"
	"strings"
)

// normalizeAgentToolInput rewrites common alias keys onto the Claude
// vocabulary expected by messages.tool_* generated columns. Unknown
// keys are preserved. Non-object inputs pass through unchanged.
//
// Also flattens command/cmd when the value is a JSON array of strings
// (Codex shell often uses {"cmd":"…"} or {"command":["go","test"]}) so
// tool_command is a single searchable string.
func normalizeAgentToolInput(raw []byte) []byte {
	if len(raw) == 0 || !isJSONObject(raw) {
		return raw
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return raw
	}
	changed := false
	if _, ok := m["file_path"]; !ok {
		for _, alias := range []string{"target_file", "path", "filePath"} {
			if v, ok := m[alias]; ok && v != nil && v != "" {
				m["file_path"] = v
				changed = true
				break
			}
		}
	}
	if _, ok := m["command"]; !ok {
		for _, alias := range []string{"cmd", "shell_command"} {
			if v, ok := m[alias]; ok && v != nil && v != "" {
				m["command"] = flattenStringOrArray(v)
				changed = true
				break
			}
		}
	} else if flat := flattenStringOrArray(m["command"]); flat != m["command"] {
		m["command"] = flat
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// flattenStringOrArray joins a string-array command into a single string;
// strings pass through. Other types return as-is.
func flattenStringOrArray(v any) any {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, el := range x {
			if s, ok := el.(string); ok {
				parts = append(parts, s)
			}
		}
		if len(parts) == len(x) {
			return strings.Join(parts, " ")
		}
	}
	return v
}
