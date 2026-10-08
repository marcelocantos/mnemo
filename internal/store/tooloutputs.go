// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Persisted tool output (🎯T192).
//
// Claude Code keeps a tool result over ~2 KB on disk — a sidecar under the
// session's tool-results/ directory — and writes a <persisted-output>
// preview into the transcript in its place. The transcript entry usually
// also carries a toolUseResult object with the tool's own view of the
// result (Bash: stdout capped near 30,000 characters plus the sidecar
// path and size; Grep/Glob, WebFetch and MCP tools each have their own
// shape; about one preview in eight has none). Only the preview reached
// messages, so a search could not see the body of anything large.
//
// The body goes in tool_outputs, one row per preview message, and never
// into messages.text: mnemo_read_session and the compactor read that
// column and must not grow by megabytes per tool call. Resolution order
// is the sidecar when it is still on disk and is a text file, else the
// string leaves of toolUseResult, else nothing — in which case a row
// with source 'none' records that the preview was examined.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// persistedOutputTag opens every preview Claude Code writes in place of
// a large tool result.
const persistedOutputTag = "<persisted-output>"

// bashStdoutTag wraps the output of a shell command the user ran with
// the `!` prefix, which Claude Code feeds back as a plain user message
// rather than a tool_result. When that output was large the preview
// sits inside the wrapper, so the message is a preview too — on the
// owner's store, 2,277 of 4,678 previews were this shape.
const bashStdoutTag = "<bash-stdout>"

// toolOutputMaxBytes caps a stored body. The largest observed sidecars
// are a few MB; beyond this the index gains little and the backup, the
// WAL and every read of the row pay for it.
const toolOutputMaxBytes = 4 << 20

// Where a body came from. Stored in tool_outputs.source.
const (
	ToolOutputSourceSidecar       = "sidecar"
	ToolOutputSourceToolUseResult = "tool_use_result"
	ToolOutputSourceNone          = "none"
)

// sidecarPathRE finds the path in the preview's own sentence when the
// entry has no toolUseResult.persistedOutputPath.
var sidecarPathRE = regexp.MustCompile(`Full output saved to: (\S+)`)

// toolOutputText is a resolved body ready to store.
type toolOutputText struct {
	text   string
	source string
}

// toolUseResultMetaKeys are the toolUseResult keys that describe the
// result rather than carry it; their values never become searchable
// text.
var toolUseResultMetaKeys = map[string]bool{
	"persistedOutputPath": true,
	"persistedOutputSize": true,
	"isImage":             true,
	"interrupted":         true,
	"noOutputExpected":    true,
	"agentId":             true,
	"type":                true,
}

// resolveToolOutput returns the full body for a tool_result whose text
// is a persisted-output preview, and nil for any other tool_result.
func resolveToolOutput(preview string, toolUseResult json.RawMessage) *toolOutputText {
	if !isPersistedOutputPreview(preview) {
		return nil
	}
	var meta map[string]json.RawMessage
	if len(toolUseResult) > 0 {
		_ = json.Unmarshal(toolUseResult, &meta) // non-object shapes are handled below
	}

	path := ""
	if raw, ok := meta["persistedOutputPath"]; ok {
		_ = json.Unmarshal(raw, &path)
	}
	if path == "" {
		if m := sidecarPathRE.FindStringSubmatch(preview); m != nil {
			path = m[1]
		}
	}
	if text, ok := readSidecar(path); ok {
		return &toolOutputText{text: text, source: ToolOutputSourceSidecar}
	}
	if text := toolUseResultLeaves(toolUseResult); text != "" {
		return &toolOutputText{text: capText(text), source: ToolOutputSourceToolUseResult}
	}
	return &toolOutputText{source: ToolOutputSourceNone}
}

// bashInputClose ends the <bash-input> block that precedes <bash-stdout>
// when the user's command is recorded in the same message.
const bashInputClose = "</bash-input>"

// isPersistedOutputPreview reports whether a block's text is a preview:
// the tag opens the text, or opens the <bash-stdout> block (itself
// optionally preceded by the <bash-input> block). A preview quoted
// deeper inside a prompt — the compactor's opening prompt, say, which
// carries whole transcripts — is not one.
func isPersistedOutputPreview(text string) bool {
	if strings.HasPrefix(text, "<bash-input>") {
		if i := strings.Index(text, bashInputClose); i >= 0 {
			text = text[i+len(bashInputClose):]
		}
	}
	text = strings.TrimLeft(text, " \n")
	text = strings.TrimLeft(strings.TrimPrefix(text, bashStdoutTag), " \n")
	return strings.HasPrefix(text, persistedOutputTag)
}

// readSidecar reads a persisted-output file. The path comes from a
// transcript, which is data, so it is admitted only when it is an
// absolute path to a text file inside a .claude/projects tool-results
// directory. Binary sidecars (pdf, jpg) are skipped: the toolUseResult
// leaves are the searchable text for those.
func readSidecar(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	clean := filepath.Clean(path)
	slashed := filepath.ToSlash(clean)
	if !strings.Contains(slashed, "/.claude/projects/") || !strings.Contains(slashed, "/tool-results/") {
		return "", false
	}
	switch strings.ToLower(filepath.Ext(clean)) {
	case ".txt", ".md", ".json", ".log", ".csv", ".xml", ".yaml", ".yml", ".html":
	default:
		return "", false
	}
	f, err := os.Open(clean)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, toolOutputMaxBytes))
	if err != nil {
		return "", false
	}
	text := strings.ToValidUTF8(string(b), "�")
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

// toolUseResultLeaves concatenates the string leaves of a toolUseResult
// value in key order, skipping metadata keys. A bare string is itself
// the leaf; an array of content blocks yields each block's text.
func toolUseResultLeaves(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var parts []string
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 8 {
			return
		}
		switch x := v.(type) {
		case string:
			if strings.TrimSpace(x) != "" {
				parts = append(parts, x)
			}
		case []any:
			for _, e := range x {
				walk(e, depth+1)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				if !toolUseResultMetaKeys[k] {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k], depth+1)
			}
		}
	}
	walk(v, 0)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// capText bounds a body at toolOutputMaxBytes without splitting a rune.
func capText(s string) string {
	if len(s) <= toolOutputMaxBytes {
		return s
	}
	cut := toolOutputMaxBytes
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// toolOutputsWritable reports whether the writer may insert into
// tool_outputs. The table arrives with the schema upgrade, so a store
// serving on an older schema skips the insert; the startup backfill
// (which waits for CapSchemaCurrent) picks those previews up.
func (s *Store) toolOutputsWritable() bool {
	return s.Have(CapSchemaCurrent)
}

// insertToolOutput stores one resolved body against its preview message
// inside the writer's transaction. Idempotent through UNIQUE(message_id).
func (ws *writerState) insertToolOutput(messageID int64, sessionID, project, toolUseID string, out *toolOutputText) error {
	plain, z := ws.codec.pack(FamilyMessagesText, out.text)
	var zLen any
	if z != nil {
		zLen = len(z)
	}
	_, err := ws.tx.Exec(`
		INSERT OR IGNORE INTO tool_outputs
			(message_id, session_id, project, tool_use_id, source, text, text_z, plain_len, z_len)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		messageID, sessionID, project, toolUseID, out.source, plain, z, len(out.text), zLen)
	return err
}

// ToolOutputBackfillResult counts what a backfill pass stored, by source.
type ToolOutputBackfillResult struct {
	Sidecar       int64
	ToolUseResult int64
	None          int64
}

// Total is the number of preview rows the pass resolved.
func (r ToolOutputBackfillResult) Total() int64 { return r.Sidecar + r.ToolUseResult + r.None }

// toolOutputBackfillBatch bounds one transaction of the backfill; each
// row may carry megabytes, so the batch is small.
const toolOutputBackfillBatch = 25

// toolOutputBackfillYield is the pause between committed batches so the
// backfill does not pin the writer against live ingest.
const toolOutputBackfillYield = 20 * time.Millisecond

// toolOutputCandidatesSQL lists preview messages with no tool_outputs
// row yet, in id order from a cursor. The FTS index is the cheap way to
// find previews among millions of rows: both phrases occur in every
// preview Claude Code writes. They also occur in anything that quotes
// one — on the owner's store 2,297 of 4,671 FTS matches were prompts
// carrying whole transcripts — so the head of the decoded text is
// checked here, and the resolver's full check decides after that.
const toolOutputCandidatesSQL = `
	SELECT m.id, COALESCE(m.entry_id, 0), m.session_id, m.project, COALESCE(m.tool_use_id, ''),
		mnemo_text(m.text, m.text_z)
	FROM messages m
	WHERE m.id IN (SELECT rowid FROM messages_fts
			WHERE messages_fts MATCH '"persisted output" AND "full output saved to"')
	  AND m.content_type IN ('tool_result', 'text') AND m.role = 'user' AND m.is_noise = 0 AND m.id > ?
	  AND substr(mnemo_text(m.text, m.text_z), 1, 12) IN ('<persisted-o', '<bash-stdout', '<bash-input>')
	  AND NOT EXISTS (SELECT 1 FROM tool_outputs t WHERE t.message_id = m.id)
	ORDER BY m.id LIMIT ?`

// ToolOutputBackfill fills tool_outputs for preview messages written
// before the table existed, reading each entry's stored JSONL line for
// its toolUseResult and preferring the sidecar when it is still on
// disk. Resumable (a cursor over message id within the run, and
// UNIQUE(message_id) across runs) and idempotent: a second pass finds
// no candidates. Runs at startup once the schema is current.
func (s *Store) ToolOutputBackfill(ctx context.Context) (ToolOutputBackfillResult, error) {
	var res ToolOutputBackfillResult
	var cursor int64
	started := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n, last, batch, err := s.toolOutputBackfillBatch(ctx, cursor)
		if err != nil {
			return res, err
		}
		if n == 0 {
			break
		}
		cursor = last
		res.Sidecar += batch.Sidecar
		res.ToolUseResult += batch.ToolUseResult
		res.None += batch.None
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(toolOutputBackfillYield):
		}
	}
	if res.Total() > 0 {
		slog.Info("tool output backfill complete",
			"previews", res.Total(), "sidecar", res.Sidecar,
			"tool_use_result", res.ToolUseResult, "none", res.None,
			"elapsed", time.Since(started).Round(time.Millisecond))
	}
	return res, nil
}

// toolOutputBackfillBatch resolves one batch of candidates after cursor.
// It reports how many candidates it saw (zero means converged), the last
// id examined, and the per-source counts stored.
func (s *Store) toolOutputBackfillBatch(ctx context.Context, cursor int64) (seen int, last int64, res ToolOutputBackfillResult, err error) {
	type candidate struct {
		id, entryID        int64
		sessionID, project string
		toolUseID, preview string
	}
	var cands []candidate
	rows, err := s.readDB.QueryContext(ctx, toolOutputCandidatesSQL, cursor, toolOutputBackfillBatch)
	if err != nil {
		return 0, cursor, res, fmt.Errorf("select preview candidates: %w", err)
	}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.entryID, &c.sessionID, &c.project, &c.toolUseID, &c.preview); err != nil {
			rows.Close()
			return 0, cursor, res, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if len(cands) == 0 {
		return 0, cursor, res, nil
	}

	ws, err := s.newWriterState()
	if err != nil {
		return 0, cursor, res, err
	}
	defer ws.Close()
	committed := false
	defer func() {
		if !committed {
			ws.tx.Rollback()
		}
	}()

	for _, c := range cands {
		last = c.id
		// json() because a plain (uncompressed) row's raw is JSONB, which
		// mnemo_raw passes through as it is; json() renders either form
		// as text.
		var raw sql.NullString
		if c.entryID != 0 {
			if qerr := s.readDB.QueryRowContext(ctx, `SELECT json(raw) FROM entries_v WHERE id = ?`, c.entryID).Scan(&raw); qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
				return 0, cursor, res, fmt.Errorf("read entry %d: %w", c.entryID, qerr)
			}
		}
		var entry jsonlEntry
		if raw.Valid {
			_ = json.Unmarshal([]byte(raw.String), &entry) // an undecodable line resolves like a missing one
		}
		out := resolveToolOutput(c.preview, entry.ToolUseResult)
		if out == nil {
			// Passed the head check but is not a preview (a <bash-input>
			// block followed by something else). Not a tool output; no
			// row. The candidate filter already excludes it cheaply on
			// the next start, so nothing is re-examined at any cost.
			continue
		}
		if err := ws.insertToolOutput(c.id, c.sessionID, c.project, c.toolUseID, out); err != nil {
			return 0, cursor, res, fmt.Errorf("insert tool output for message %d: %w", c.id, err)
		}
		switch out.source {
		case ToolOutputSourceSidecar:
			res.Sidecar++
		case ToolOutputSourceToolUseResult:
			res.ToolUseResult++
		default:
			res.None++
		}
	}
	ws.Close()
	if err := ws.tx.Commit(); err != nil {
		return 0, cursor, res, err
	}
	committed = true
	return len(cands), last, res, nil
}

// ToolOutputCounts reports stored bodies by source, for proofs and ops.
func (s *Store) ToolOutputCounts() (map[string]int64, error) {
	rows, err := s.readDB.Query(`SELECT source, COUNT(*) FROM tool_outputs GROUP BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			return nil, err
		}
		out[src] = n
	}
	return out, rows.Err()
}
