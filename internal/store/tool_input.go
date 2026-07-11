// Shared tool-input normalisation for non-Claude agents (🎯T111).
// Claude stores tool_use.input with file_path / command keys that feed
// generated columns (tool_file_path, tool_command). Grok (and others)
// use target_file / path / cmd. Normalise before jsonb() so who_ran and
// file-path queries work across sources.
package store

import "encoding/json"

// normalizeAgentToolInput rewrites common alias keys onto the Claude
// vocabulary expected by messages.tool_* generated columns. Unknown
// keys are preserved. Non-object inputs pass through unchanged.
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
				m["command"] = v
				changed = true
				break
			}
		}
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
