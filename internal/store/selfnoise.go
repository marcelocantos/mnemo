// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Self-traffic exclusion (🎯T190).
//
// Every call an agent makes to mnemo is written back into the transcript
// mnemo indexes: the tool_use block carrying the query, and the
// tool_result block carrying mnemo's own answer. Search has no self
// filter, and it pulls the top max(limit*10, 200) FTS rows before any
// filtering, so a corpus of mnemo's echoes — thousands of rows, each
// dense with exactly the words people search for — crowds real hits out
// of the candidate set. On the owner's store, 3,501 mnemo tool_use rows
// and their 3,501 paired tool_results were indexed when this was written.
//
// This is the 🎯T52 loop-safety fence extended from file paths to
// transcript content: mnemo must not index what it said. The rows are
// still stored — mnemo_read_session shows them, agent trees count them
// — they are simply written with is_noise=1, which the messages_ai
// trigger already understands as "do not index". Rows written before
// this existed are flipped by a startup backfill that removes each one
// from the FTS index using its old values, the only way an external-
// content FTS5 table can forget a row.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// selfToolName matches mnemo's own MCP tools however a client spells
// them: Claude Code prefixes the server (`mcp__mnemo__mnemo_search`),
// while Codex, Grok and Cursor record the bare name (`mnemo_search`).
var selfToolName = regexp.MustCompile(`^(mcp__mnemo__)?mnemo_`)

// isSelfTool reports whether a tool name is one of mnemo's own.
func isSelfTool(name string) bool {
	return selfToolName.MatchString(name)
}

// selfToolUseGlobs are the SQL spelling of selfToolName, for the
// backfill's index-driven scans. GLOB rather than LIKE: LIKE is
// case-insensitive by default and so cannot use the BINARY index on
// tool_name, and `_` is a wildcard in LIKE but literal in GLOB.
const selfToolUseWhere = `content_type = 'tool_use'
	AND (tool_name GLOB 'mnemo_*' OR tool_name GLOB 'mcp__mnemo__mnemo_*')`

// pairedSelfToolResult decides whether a tool_result belongs to one of
// mnemo's own tool_use blocks. The common case — the pair lands in the
// same writer transaction — is answered from the set the writer keeps;
// a result that arrives after a batch commit (the realtime path commits
// every 200ms) is answered by a point lookup on idx_messages_tool_use_id.
func (ws *writerState) pairedSelfToolResult(toolUseID string) bool {
	if toolUseID == "" {
		return false
	}
	if _, ok := ws.selfToolUses[toolUseID]; ok {
		return true
	}
	var one int
	err := ws.tx.QueryRow(`SELECT 1 FROM messages WHERE tool_use_id = ? AND `+selfToolUseWhere+` LIMIT 1`,
		toolUseID).Scan(&one)
	return err == nil
}

// selfNoiseBatch bounds one backfill transaction. Each row costs an FTS
// 'delete' (which re-tokenises the row's text) plus an UPDATE, so the
// batch is kept small enough that a write lock never lasts long against
// live ingest.
const selfNoiseBatch = 500

// selfNoiseYield is the pause between committed batches, so a backfill
// on a large store does not pin the writer.
const selfNoiseYield = 20 * time.Millisecond

// SelfNoiseResult reports what a backfill pass flipped.
type SelfNoiseResult struct {
	ToolUses    int64 // mnemo tool_use rows flipped to is_noise=1
	ToolResults int64 // their paired tool_result rows
}

// SelfNoiseBackfill flips every indexed mnemo tool_use row, and every
// tool_result paired with one, to is_noise=1, removing each from
// messages_fts first. Idempotent: a second pass finds nothing. Runs at
// startup under supervision (see New) and may be called directly.
//
// session_summary.substantive_msgs was incremented for these rows by the
// insert trigger, so it is decremented here to match; otherwise the
// counter would keep a tally the index no longer agrees with.
func (s *Store) SelfNoiseBackfill(ctx context.Context) (SelfNoiseResult, error) {
	var res SelfNoiseResult
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		uses, results, err := s.selfNoiseFlipBatch(ctx)
		if err != nil {
			return res, err
		}
		if uses == 0 && results == 0 {
			break
		}
		res.ToolUses += uses
		res.ToolResults += results
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(selfNoiseYield):
		}
	}
	if res.ToolUses > 0 || res.ToolResults > 0 {
		slog.Info("self-noise backfill complete",
			"tool_uses", res.ToolUses, "tool_results", res.ToolResults)
	}
	return res, nil
}

// selfNoiseFlipBatch flips one batch and reports how many rows of each
// kind it touched. Zero for both means the backfill has converged.
//
// The results are selected by their tool_use's name rather than by the
// tool_use's is_noise, so a result whose tool_use was already flipped in
// an earlier batch — or written noisy by the fixed insert path while its
// result was not — is still found.
func (s *Store) selfNoiseFlipBatch(ctx context.Context) (uses, results int64, err error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	type victim struct {
		id        int64
		sessionID string
		isResult  bool
	}
	var victims []victim
	collect := func(q string, isResult bool) error {
		rows, qerr := tx.QueryContext(ctx, q, selfNoiseBatch-len(victims))
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.sessionID); err != nil {
				return err
			}
			v.isResult = isResult
			victims = append(victims, v)
		}
		return rows.Err()
	}
	if err = collect(`SELECT id, session_id FROM messages
		WHERE is_noise = 0 AND `+selfToolUseWhere+` LIMIT ?`, false); err != nil {
		return 0, 0, fmt.Errorf("select self tool_use rows: %w", err)
	}
	if len(victims) < selfNoiseBatch {
		if err = collect(`SELECT r.id, r.session_id FROM messages r
			WHERE r.is_noise = 0 AND r.content_type = 'tool_result' AND r.tool_use_id IN (
				SELECT u.tool_use_id FROM messages u WHERE `+strings.ReplaceAll(selfToolUseWhere, "tool_name", "u.tool_name")+`
				AND u.tool_use_id IS NOT NULL AND u.tool_use_id != '')
			LIMIT ?`, true); err != nil {
			return 0, 0, fmt.Errorf("select paired tool_result rows: %w", err)
		}
	}
	if len(victims) == 0 {
		return 0, 0, tx.Commit()
	}

	// messages_fts is an external-content index (content=messages): a
	// row leaves it only through the 'delete' command given the values
	// that were indexed, and only rows with is_noise = 0 were ever
	// indexed (the insert trigger filters on it). The WHERE on is_noise
	// is therefore load-bearing: deleting an unindexed row would corrupt
	// the index.
	const ftsDelete = `
		INSERT INTO messages_fts(messages_fts, rowid, text, role, project, session_id)
		SELECT 'delete', m.id, m.text, m.role, m.project, m.session_id
		FROM messages_v m WHERE m.id = ? AND m.is_noise = 0`
	perSession := map[string]int64{}
	for _, v := range victims {
		if _, err = tx.ExecContext(ctx, ftsDelete, v.id); err != nil {
			return 0, 0, fmt.Errorf("fts delete for message %d: %w", v.id, err)
		}
		var r sql.Result
		if r, err = tx.ExecContext(ctx, `UPDATE messages SET is_noise = 1 WHERE id = ? AND is_noise = 0`, v.id); err != nil {
			return 0, 0, fmt.Errorf("flip message %d: %w", v.id, err)
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			continue
		}
		perSession[v.sessionID] += n
		if v.isResult {
			results += n
		} else {
			uses += n
		}
	}
	for sessionID, n := range perSession {
		if _, err = tx.ExecContext(ctx, `
			UPDATE session_summary SET substantive_msgs = MAX(0, substantive_msgs - ?)
			WHERE session_id = ?`, n, sessionID); err != nil {
			return 0, 0, fmt.Errorf("adjust session_summary for %s: %w", sessionID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return uses, results, nil
}
