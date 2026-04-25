// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// UUIDMatch is a single UUID lookup result. MatchKind describes which UUID
// field in the entry matched the query prefix.
//
//   - "entry_uuid"     — the entry's own $.uuid field
//   - "parent_uuid"    — the entry's $.parentUuid field
//   - "tool_use_id"    — a tool_use content block's id ($.message.content[*].id)
//   - "tool_result_id" — a tool_result content block's tool_use_id
//   - "top_tool_use_id"   — $.toolUseID (entry-level, progress entries)
//   - "parent_tool_use_id" — $.parentToolUseID (entry-level)
type UUIDMatch struct {
	EntryID   int              `json:"entry_id"`
	SessionID string           `json:"session_id"`
	Project   string           `json:"project"`
	Type      string           `json:"type"`       // entry type: user, assistant, system, …
	MatchKind string           `json:"match_kind"` // which UUID field matched
	MatchedID string           `json:"matched_id"` // the full UUID that matched
	Timestamp string           `json:"timestamp,omitempty"`
	Before    []ContextMessage `json:"before,omitempty"`
	After     []ContextMessage `json:"after,omitempty"`
}

// locateUUIDQuery runs a single lookup sub-query against the entries table.
// matchKind names the field (e.g. "entry_uuid"), expr is a SQL expression
// that extracts that field from the entries table. prefix is the user-supplied
// UUID prefix.
//
// Returns (entryID, sessionID, project, entryType, matchedID, timestamp, ok, err).
type uuidCandidate struct {
	entryID   int
	sessionID string
	project   string
	entryType string
	matchKind string
	matchedID string
	timestamp string
}

// LocateUUID searches the entries table for any entry whose UUID fields
// match the supplied prefix. The following fields are checked, in order:
//
//  1. $.uuid          (entry_uuid)
//  2. $.parentUuid    (parent_uuid)
//  3. $.toolUseID     (top_tool_use_id — virtual column)
//  4. $.parentToolUseID (parent_tool_use_id — virtual column)
//  5. any $.message.content[*].id where the element's type = "tool_use" (tool_use_id)
//  6. any $.message.content[*].tool_use_id where the element's type = "tool_result" (tool_result_id)
//
// contextBefore and contextAfter control how many surrounding messages are
// fetched from the messages table (same semantics as Search).
func (s *Store) LocateUUID(prefix string, contextBefore, contextAfter int) ([]UUIDMatch, error) {
	s.rwmu.RLock()
	defer s.rwmu.RUnlock()

	if prefix == "" {
		return nil, fmt.Errorf("uuid prefix is required")
	}
	likeArg := prefix + "%"

	// Build UNION query: each arm checks one UUID field.
	// Arms 1-4 use either a virtual column or json_extract on the raw JSONB.
	// Arms 5-6 use json_each to fan out over the content array.
	const q = `
SELECT e.id, e.session_id, e.project, e.type, e.timestamp,
       'entry_uuid'          AS match_kind,
       json_extract(e.raw, '$.uuid') AS matched_id
FROM entries e
WHERE json_extract(e.raw, '$.uuid') LIKE ?

UNION ALL

SELECT e.id, e.session_id, e.project, e.type, e.timestamp,
       'parent_uuid'         AS match_kind,
       json_extract(e.raw, '$.parentUuid') AS matched_id
FROM entries e
WHERE json_extract(e.raw, '$.parentUuid') LIKE ?

UNION ALL

SELECT e.id, e.session_id, e.project, e.type, e.timestamp,
       'top_tool_use_id'     AS match_kind,
       e.top_tool_use_id     AS matched_id
FROM entries e
WHERE e.top_tool_use_id LIKE ?

UNION ALL

SELECT e.id, e.session_id, e.project, e.type, e.timestamp,
       'parent_tool_use_id'  AS match_kind,
       e.parent_tool_use_id  AS matched_id
FROM entries e
WHERE e.parent_tool_use_id LIKE ?

UNION ALL

SELECT e.id, e.session_id, e.project, e.type, e.timestamp,
       'tool_use_id'         AS match_kind,
       jc.value->>'$.id'     AS matched_id
FROM (SELECT * FROM entries WHERE json_type(raw, '$.message.content') = 'array') e
JOIN json_each(e.raw, '$.message.content') jc
WHERE jc.value->>'$.type' = 'tool_use'
  AND jc.value->>'$.id' LIKE ?

UNION ALL

SELECT e.id, e.session_id, e.project, e.type, e.timestamp,
       'tool_result_id'      AS match_kind,
       jc.value->>'$.tool_use_id' AS matched_id
FROM (SELECT * FROM entries WHERE json_type(raw, '$.message.content') = 'array') e
JOIN json_each(e.raw, '$.message.content') jc
WHERE jc.value->>'$.type' = 'tool_result'
  AND jc.value->>'$.tool_use_id' LIKE ?

ORDER BY 1
LIMIT 50
`
	rows, err := s.db.Query(q,
		likeArg, likeArg, likeArg, likeArg, likeArg, likeArg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []uuidCandidate
	for rows.Next() {
		var c uuidCandidate
		var ts sql.NullString
		if err := rows.Scan(&c.entryID, &c.sessionID, &c.project, &c.entryType, &ts, &c.matchKind, &c.matchedID); err != nil {
			continue
		}
		if ts.Valid {
			c.timestamp = ts.String
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Deduplicate by (entryID, matchKind) — json_each can produce multiple
	// rows if the content array has several matching blocks.
	seen := map[string]bool{}
	var matches []UUIDMatch
	for _, c := range candidates {
		key := fmt.Sprintf("%d\x00%s", c.entryID, c.matchKind)
		if seen[key] {
			continue
		}
		seen[key] = true

		m := UUIDMatch{
			EntryID:   c.entryID,
			SessionID: c.sessionID,
			Project:   c.project,
			Type:      c.entryType,
			MatchKind: c.matchKind,
			MatchedID: c.matchedID,
			Timestamp: c.timestamp,
		}

		// Fetch context from messages table using the entry's first message.
		if contextBefore > 0 || contextAfter > 0 {
			firstMsgID, err := s.firstMessageIDForEntry(c.entryID, c.sessionID)
			if err == nil && firstMsgID > 0 {
				if contextBefore > 0 {
					m.Before = s.fetchContext(c.sessionID, firstMsgID, contextBefore, true, true)
				}
				if contextAfter > 0 {
					m.After = s.fetchContext(c.sessionID, firstMsgID, contextAfter, false, true)
				}
			}
		}

		matches = append(matches, m)
	}

	return matches, nil
}

// firstMessageIDForEntry returns the minimum messages.id for the given entry,
// so we can use fetchContext to pull surrounding messages.
func (s *Store) firstMessageIDForEntry(entryID int, sessionID string) (int, error) {
	var id int
	err := s.db.QueryRow(
		`SELECT MIN(id) FROM messages WHERE entry_id = ? AND session_id = ?`,
		entryID, sessionID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}
