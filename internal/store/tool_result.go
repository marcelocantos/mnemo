// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "database/sql"

// ToolResultPayload holds the raw text body of a tool-result message.
type ToolResultPayload struct {
	Text      string `json:"text"`
	IsError   bool   `json:"is_error"`
	TotalLen  int    `json:"total_len"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ToolResult returns the raw text body for a tool-result message identified
// by session_id and tool_use_id. Supports session_id prefix matching.
// truncateLen <= 0 means no truncation. offset skips the first N bytes.
func (s *Store) ToolResult(sessionID, toolUseID string, offset, truncateLen int) (*ToolResultPayload, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	resolvedID, err := s.resolveSessionID(sessionID)
	if err != nil {
		return nil, err
	}

	var text string
	var isError int
	err = s.db.QueryRow(
		`SELECT text, is_error FROM messages
		 WHERE session_id = ? AND content_type = 'tool_result' AND tool_use_id = ?
		 LIMIT 1`,
		resolvedID, toolUseID,
	).Scan(&text, &isError)
	if err == sql.ErrNoRows {
		return nil, &ToolResultNotFoundError{SessionID: resolvedID, ToolUseID: toolUseID}
	}
	if err != nil {
		return nil, err
	}

	total := len(text)

	// Apply byte offset.
	if offset > 0 {
		if offset >= len(text) {
			text = ""
		} else {
			text = text[offset:]
		}
	}

	truncated := false
	if truncateLen > 0 && len(text) > truncateLen {
		text = text[:truncateLen]
		truncated = true
	}

	return &ToolResultPayload{
		Text:      text,
		IsError:   isError != 0,
		TotalLen:  total,
		Truncated: truncated,
	}, nil
}

// ToolResultNotFoundError is returned when no matching tool-result is found.
type ToolResultNotFoundError struct {
	SessionID string
	ToolUseID string
}

func (e *ToolResultNotFoundError) Error() string {
	return "no tool_result found for tool_use_id " + e.ToolUseID + " in session " + e.SessionID
}
