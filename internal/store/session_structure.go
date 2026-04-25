// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// SessionStructure summarises the entry types and content-block shapes
// present in a single session transcript. It is the structured answer to
// "what is in this session?" — replacing inline-Python JSONL inspection.
type SessionStructure struct {
	// SessionID is the resolved (full) session ID.
	SessionID string `json:"session_id"`

	// TotalEntries is the count of all JSONL lines (entries) in the session.
	TotalEntries int `json:"total_entries"`

	// EntryTypes maps entry type ("user", "assistant", "system",
	// "progress", "file-history-snapshot", …) to occurrence count.
	EntryTypes map[string]int `json:"entry_types"`

	// AssistantStopReasons maps stop_reason values ("end_turn", "tool_use",
	// "max_tokens", …) to count. Only populated for assistant entries.
	AssistantStopReasons map[string]int `json:"assistant_stop_reasons,omitempty"`

	// SystemSubtypes maps the $.subtype field of system entries to count.
	// Common values: "prompt", "reminder", "tool_list", etc.
	SystemSubtypes map[string]int `json:"system_subtypes,omitempty"`

	// ContentBlockKinds maps content-block type strings ("text",
	// "tool_use", "tool_result", "thinking") to count across all messages.
	ContentBlockKinds map[string]int `json:"content_block_kinds,omitempty"`

	// ToolNames maps tool names from tool_use content blocks to count.
	// Gives an at-a-glance view of which tools were exercised.
	ToolNames map[string]int `json:"tool_names,omitempty"`
}

// SessionStructure returns a structural summary of the session identified
// by sessionID (exact or prefix). Counts are aggregated from the entries
// and messages tables using SQLite JSON1 extraction.
func (s *Store) SessionStructure(sessionID string) (*SessionStructure, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	resolvedID, err := s.resolveSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	result := &SessionStructure{
		SessionID:            resolvedID,
		EntryTypes:           make(map[string]int),
		AssistantStopReasons: make(map[string]int),
		SystemSubtypes:       make(map[string]int),
		ContentBlockKinds:    make(map[string]int),
		ToolNames:            make(map[string]int),
	}

	// --- Entry type counts ---
	entryRows, err := s.db.Query(`
		SELECT type, COUNT(*) AS cnt
		FROM entries
		WHERE session_id = ?
		GROUP BY type
		ORDER BY cnt DESC
	`, resolvedID)
	if err != nil {
		return nil, err
	}
	defer entryRows.Close()
	for entryRows.Next() {
		var typ string
		var cnt int
		if err := entryRows.Scan(&typ, &cnt); err != nil {
			continue
		}
		result.TotalEntries += cnt
		result.EntryTypes[typ] = cnt
	}
	if err := entryRows.Err(); err != nil {
		return nil, err
	}

	// --- Assistant stop_reason counts ---
	stopRows, err := s.db.Query(`
		SELECT COALESCE(stop_reason, '(null)') AS sr, COUNT(*) AS cnt
		FROM entries
		WHERE session_id = ? AND type = 'assistant'
		GROUP BY sr
		ORDER BY cnt DESC
	`, resolvedID)
	if err != nil {
		return nil, err
	}
	defer stopRows.Close()
	for stopRows.Next() {
		var sr string
		var cnt int
		if err := stopRows.Scan(&sr, &cnt); err != nil {
			continue
		}
		result.AssistantStopReasons[sr] = cnt
	}
	if err := stopRows.Err(); err != nil {
		return nil, err
	}

	// --- System subtype counts ---
	// subtype is not a virtual column; extract directly from JSONB.
	subtypeRows, err := s.db.Query(`
		SELECT COALESCE(json_extract(raw, '$.subtype'), '(none)') AS st, COUNT(*) AS cnt
		FROM entries
		WHERE session_id = ? AND type = 'system'
		GROUP BY st
		ORDER BY cnt DESC
	`, resolvedID)
	if err != nil {
		return nil, err
	}
	defer subtypeRows.Close()
	for subtypeRows.Next() {
		var st string
		var cnt int
		if err := subtypeRows.Scan(&st, &cnt); err != nil {
			continue
		}
		result.SystemSubtypes[st] = cnt
	}
	if err := subtypeRows.Err(); err != nil {
		return nil, err
	}

	// --- Content-block kind counts (from messages table) ---
	blockRows, err := s.db.Query(`
		SELECT content_type, COUNT(*) AS cnt
		FROM messages
		WHERE session_id = ?
		GROUP BY content_type
		ORDER BY cnt DESC
	`, resolvedID)
	if err != nil {
		return nil, err
	}
	defer blockRows.Close()
	for blockRows.Next() {
		var ct string
		var cnt int
		if err := blockRows.Scan(&ct, &cnt); err != nil {
			continue
		}
		result.ContentBlockKinds[ct] = cnt
	}
	if err := blockRows.Err(); err != nil {
		return nil, err
	}

	// --- Tool name counts ---
	toolRows, err := s.db.Query(`
		SELECT tool_name, COUNT(*) AS cnt
		FROM messages
		WHERE session_id = ? AND content_type = 'tool_use' AND tool_name IS NOT NULL
		GROUP BY tool_name
		ORDER BY cnt DESC
	`, resolvedID)
	if err != nil {
		return nil, err
	}
	defer toolRows.Close()
	for toolRows.Next() {
		var name string
		var cnt int
		if err := toolRows.Scan(&name, &cnt); err != nil {
			continue
		}
		result.ToolNames[name] = cnt
	}
	if err := toolRows.Err(); err != nil {
		return nil, err
	}

	// Prune empty maps so output stays compact.
	if len(result.AssistantStopReasons) == 0 {
		result.AssistantStopReasons = nil
	}
	if len(result.SystemSubtypes) == 0 {
		result.SystemSubtypes = nil
	}
	if len(result.ContentBlockKinds) == 0 {
		result.ContentBlockKinds = nil
	}
	if len(result.ToolNames) == 0 {
		result.ToolNames = nil
	}

	return result, nil
}
