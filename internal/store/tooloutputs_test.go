// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for 🎯T192: persisted tool output is searchable in full.
//
// Sentinels are nonsense words placed AFTER the first 2 KB of the body,
// so a hit can only come from the full output and never from the
// preview that reaches messages.text.

// previewFor builds the <persisted-output> text Claude Code writes in
// place of a large result.
func previewFor(path string, body string) string {
	head := body
	if len(head) > 2048 {
		head = head[:2048]
	}
	return persistedOutputTag + "\nOutput too large (90.4KB). Full output saved to: " + path +
		"\n\nPreview (first 2KB):\n" + head + "\n...\n</persisted-output>"
}

// sidecarDir makes a tool-results directory shaped like Claude Code's,
// under a .claude/projects root, so readSidecar admits its files.
func sidecarDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".claude", "projects", "-proj", "sess", "tool-results")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeSidecar(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// bigBody is a body whose sentinel sits past the 2 KB preview.
func bigBody(sentinel string) string {
	return strings.Repeat("ordinary output line that fills the preview window\n", 60) +
		"the needle is " + sentinel + " and it sits well past the preview\n"
}

func previewEntry(ts, toolUseID, preview string, toolUseResult map[string]any) map[string]any {
	e := toolResultMsg(ts, toolUseID, preview)
	if toolUseResult != nil {
		e["toolUseResult"] = toolUseResult
	}
	return e
}

func toolOutputHits(t *testing.T, s *Store, q string, opts UnifiedOpts) []UnifiedHit {
	t.Helper()
	if opts.Kinds == nil {
		opts.Kinds = []string{"tool_output"}
	}
	if opts.Limit == 0 {
		opts.Limit = 10
	}
	if opts.SessionType == "" {
		opts.SessionType = "all"
	}
	res, err := s.UnifiedSearchOpts(q, opts, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearchOpts(%q): %v", q, err)
	}
	return res.Hits
}

func toolOutputRow(t *testing.T, s *Store, toolUseID string) (source string, textLen int, messageID int64) {
	t.Helper()
	err := s.readDB.QueryRow(`SELECT source, length(mnemo_text(text, text_z)), message_id FROM tool_outputs WHERE tool_use_id = ?`, toolUseID).
		Scan(&source, &textLen, &messageID)
	if err != nil {
		t.Fatalf("tool_outputs row for %s: %v", toolUseID, err)
	}
	return
}

// TestPersistedOutputSearchableViaBulkIngest: the bulk path (IngestAll →
// parseFile) stores the sidecar body and the sentinel is found as a
// tool_output hit linked to its preview message.
func TestPersistedOutputSearchableViaBulkIngest(t *testing.T) {
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	body := bigBody("zqxbulkneedle")
	path := writeSidecar(t, dir, "tu-bulk.txt", body)
	writeJSONL(t, projectDir, "proj", "sess-bulk", []map[string]any{
		msg("user", "run the long thing", "2026-04-01T10:00:00Z"),
		toolUseMsg("2026-04-01T10:00:01Z", "Bash", "tu-bulk", map[string]any{"command": "make"}),
		previewEntry("2026-04-01T10:00:02Z", "tu-bulk", previewFor(path, body),
			map[string]any{"stdout": body[:3000], "stderr": "", "persistedOutputPath": path, "persistedOutputSize": len(body)}),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	source, textLen, messageID := toolOutputRow(t, s, "tu-bulk")
	if source != ToolOutputSourceSidecar || textLen != len(body) {
		t.Errorf("stored source=%s len=%d, want sidecar with %d bytes", source, textLen, len(body))
	}
	// The preview, not the body, is what messages holds.
	var msgLen int
	if err := s.readDB.QueryRow(`SELECT length(mnemo_text(text, text_z)) FROM messages WHERE id = ?`, messageID).Scan(&msgLen); err != nil {
		t.Fatal(err)
	}
	if msgLen >= len(body) {
		t.Errorf("messages.text grew to %d bytes; the body must not be written there", msgLen)
	}

	hits := toolOutputHits(t, s, "zqxbulkneedle", UnifiedOpts{})
	if len(hits) != 1 || hits[0].Kind != "tool_output" {
		t.Fatalf("got %d hits %+v, want one tool_output hit", len(hits), hits)
	}
	if !strings.Contains(hits[0].Title, "msg:") || hits[0].Meta != "sess-bulk" || hits[0].TS != "2026-04-01T10:00:02Z" {
		t.Errorf("hit does not link back to the preview message: title=%q meta=%q ts=%q", hits[0].Title, hits[0].Meta, hits[0].TS)
	}
	// Default set includes it.
	res, err := s.UnifiedSearch("zqxbulkneedle", nil, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Kind != "tool_output" {
		t.Errorf("default corpus set: got %+v, want the tool_output hit", res.Hits)
	}
	// And the sentinel is not reachable through the message corpus.
	if n := searchHits(t, s, "zqxbulkneedle"); n != 0 {
		t.Errorf("sentinel past the preview reached messages_fts (%d hits)", n)
	}
}

// TestPersistedOutputSearchableViaRealtimeIngest: the realtime path
// (ingestFile) does the same, and falls back to toolUseResult when the
// sidecar is gone.
func TestPersistedOutputSearchableViaRealtimeIngest(t *testing.T) {
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	body := bigBody("zqxrtneedle")
	path := writeSidecar(t, dir, "tu-rt.txt", body)
	gone := filepath.Join(dir, "tu-gone.txt") // never written
	s := newTestStore(t, projectDir)
	p := writeJSONL(t, projectDir, "proj", "sess-rt", []map[string]any{
		msg("user", "run it", "2026-04-01T10:00:00Z"),
		toolUseMsg("2026-04-01T10:00:01Z", "Bash", "tu-rt", map[string]any{"command": "make"}),
		previewEntry("2026-04-01T10:00:02Z", "tu-rt", previewFor(path, body),
			map[string]any{"stdout": body, "persistedOutputPath": path}),
		toolUseMsg("2026-04-01T10:00:03Z", "Grep", "tu-gone", map[string]any{"pattern": "x"}),
		previewEntry("2026-04-01T10:00:04Z", "tu-gone", previewFor(gone, "lots of matches"),
			map[string]any{"content": "match one\nmatch zqxfallbackneedle two", "filenames": []string{"a.go"}, "persistedOutputPath": gone}),
	})
	if err := s.ingestFile(p); err != nil {
		t.Fatal(err)
	}

	if source, textLen, _ := toolOutputRow(t, s, "tu-rt"); source != ToolOutputSourceSidecar || textLen != len(body) {
		t.Errorf("tu-rt: source=%s len=%d, want sidecar %d", source, textLen, len(body))
	}
	if source, _, _ := toolOutputRow(t, s, "tu-gone"); source != ToolOutputSourceToolUseResult {
		t.Errorf("tu-gone: source=%s, want tool_use_result when the sidecar is missing", source)
	}
	if hits := toolOutputHits(t, s, "zqxrtneedle", UnifiedOpts{}); len(hits) != 1 {
		t.Errorf("sidecar body: %d hits, want 1", len(hits))
	}
	if hits := toolOutputHits(t, s, "zqxfallbackneedle", UnifiedOpts{}); len(hits) != 1 {
		t.Errorf("toolUseResult body: %d hits, want 1", len(hits))
	}
}

// TestBashStdoutPreviewIsResolved: a shell command the user ran with `!`
// comes back as a plain user message wrapped in <bash-stdout>; when it
// was large, that message IS the preview and its sidecar is the body.
func TestBashStdoutPreviewIsResolved(t *testing.T) {
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	body := bigBody("zqxbangneedle")
	path := writeSidecar(t, dir, "bang-1.txt", body)
	writeJSONL(t, projectDir, "proj", "sess-bang", []map[string]any{
		msg("user", bashStdoutTag+previewFor(path, body)+"</bash-stdout>", "2026-04-01T10:00:00Z"),
		msg("assistant", "a prompt that merely quotes a preview: "+previewFor(path, body), "2026-04-01T10:00:01Z"),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM tool_outputs WHERE source = ?`, ToolOutputSourceSidecar).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d sidecar rows, want exactly 1 (the wrapped preview, not the quoting prompt)", n)
	}
	if hits := toolOutputHits(t, s, "zqxbangneedle", UnifiedOpts{}); len(hits) != 1 {
		t.Errorf("%d hits, want 1", len(hits))
	}
}

// TestPersistedOutputWithNoSourceKeepsPreview: neither sidecar nor
// toolUseResult — the preview stays searchable, a 'none' row records the
// examination, nothing errors.
func TestPersistedOutputWithNoSourceKeepsPreview(t *testing.T) {
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	gone := filepath.Join(dir, "tu-none.txt")
	writeJSONL(t, projectDir, "proj", "sess-none", []map[string]any{
		toolUseMsg("2026-04-01T10:00:01Z", "Bash", "tu-none", map[string]any{"command": "ls"}),
		previewEntry("2026-04-01T10:00:02Z", "tu-none", previewFor(gone, "preview carries zqxpreviewword only"), nil),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if source, textLen, _ := toolOutputRow(t, s, "tu-none"); source != ToolOutputSourceNone || textLen != 0 {
		t.Errorf("source=%s len=%d, want none with empty text", source, textLen)
	}
	if n := searchHits(t, s, "zqxpreviewword"); n != 1 {
		t.Errorf("preview is no longer searchable (%d hits)", n)
	}
	if hits := toolOutputHits(t, s, "zqxpreviewword", UnifiedOpts{}); len(hits) != 0 {
		t.Errorf("a 'none' row must not be indexed, got %d hits", len(hits))
	}
}

// TestSelfToolPersistedOutputIsNotIndexed: a large result from one of
// mnemo's own tools is noise (🎯T190) and gets no tool_outputs row.
func TestSelfToolPersistedOutputIsNotIndexed(t *testing.T) {
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	body := bigBody("zqxselfneedle")
	path := writeSidecar(t, dir, "mcp-mnemo-mnemo_search-1.txt", body)
	writeJSONL(t, projectDir, "proj", "sess-self", []map[string]any{
		toolUseMsg("2026-04-01T10:00:01Z", "mcp__mnemo__mnemo_search", "tu-self", map[string]any{"query": "x"}),
		previewEntry("2026-04-01T10:00:02Z", "tu-self", previewFor(path, body), map[string]any{"persistedOutputPath": path}),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM tool_outputs WHERE tool_use_id = 'tu-self'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("mnemo's own tool result got %d tool_outputs rows, want 0", n)
	}
	if hits := toolOutputHits(t, s, "zqxselfneedle", UnifiedOpts{}); len(hits) != 0 {
		t.Errorf("self traffic reached tool_outputs_fts (%d hits)", len(hits))
	}
}

// TestToolOutputHonoursSessionFilters: session_type and repo filters
// apply to tool_output hits as they do to messages.
func TestToolOutputHonoursSessionFilters(t *testing.T) {
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	body := bigBody("zqxfilterneedle")
	path := writeSidecar(t, dir, "tu-f.txt", body)
	writeJSONL(t, projectDir, "proj", "sess-f", []map[string]any{
		metaMsg("user", "start", "2026-04-01T10:00:00Z", "/Users/dev/work/github.com/acme/webapp", "master"),
		toolUseMsg("2026-04-01T10:00:01Z", "Bash", "tu-f", map[string]any{"command": "make"}),
		previewEntry("2026-04-01T10:00:02Z", "tu-f", previewFor(path, body), map[string]any{"persistedOutputPath": path}),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if hits := toolOutputHits(t, s, "zqxfilterneedle", UnifiedOpts{SessionType: "interactive"}); len(hits) != 1 {
		t.Errorf("interactive: %d hits, want 1", len(hits))
	}
	if hits := toolOutputHits(t, s, "zqxfilterneedle", UnifiedOpts{SessionType: "subagent"}); len(hits) != 0 {
		t.Errorf("subagent: %d hits, want 0", len(hits))
	}
	if hits := toolOutputHits(t, s, "zqxfilterneedle", UnifiedOpts{Repo: "acme/webapp"}); len(hits) != 1 {
		t.Errorf("repo match: %d hits, want 1", len(hits))
	}
	if hits := toolOutputHits(t, s, "zqxfilterneedle", UnifiedOpts{Repo: "other/repo"}); len(hits) != 0 {
		t.Errorf("repo mismatch: %d hits, want 0", len(hits))
	}
}

// TestToolOutputBackfillFromOlderSchema: a store created before
// tool_outputs existed, holding preview rows, upgrades in place and the
// startup backfill fills the table from entries.raw — preferring the
// sidecar, falling back to toolUseResult, recording 'none' — and a
// second pass inserts nothing.
func TestToolOutputBackfillFromOlderSchema(t *testing.T) {
	installTestRateCard(t, testRateCard())
	dbPath := filepath.Join(t.TempDir(), "mnemo.db")
	projectDir := t.TempDir()
	dir := sidecarDir(t)
	bodyA := bigBody("zqxoldsidecar")
	pathA := writeSidecar(t, dir, "old-a.txt", bodyA)
	pathB := filepath.Join(dir, "old-b.txt") // never on disk
	pathC := filepath.Join(dir, "old-c.txt")

	s0, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	s0.AwaitStartup()
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}

	// Roll the schema back to before 🎯T192 and write the rows an older
	// binary would have left: entries with the full line, messages with
	// the preview, no tool_outputs.
	db, err := sql.Open(writerDriverName, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS tool_outputs_ai`,
		`DROP TRIGGER IF EXISTS tool_outputs_ad`,
		`DROP TABLE IF EXISTS tool_outputs_fts`,
		`DROP INDEX IF EXISTS idx_tool_outputs_session_id`,
		`DROP TABLE IF EXISTS tool_outputs`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	type old struct {
		id, path, preview string
		tur               map[string]any
	}
	olds := []old{
		{"old-a", pathA, previewFor(pathA, bodyA), map[string]any{"stdout": "short", "persistedOutputPath": pathA}},
		{"old-b", pathB, previewFor(pathB, "gone body"), map[string]any{"stdout": "the zqxoldfallback body survived in toolUseResult", "persistedOutputPath": pathB}},
		{"old-c", pathC, previewFor(pathC, "nothing anywhere"), nil},
	}
	for i, o := range olds {
		e := previewEntry("2026-04-01T10:00:0"+string(rune('0'+i))+"Z", o.id, o.preview, o.tur)
		e["uuid"] = "u-" + o.id
		raw, _ := json.Marshal(e)
		res, err := db.Exec(`INSERT INTO entries (session_id, project, type, timestamp, raw) VALUES ('sess-old', 'proj', 'user', ?, jsonb(?))`,
			e["timestamp"], string(raw))
		if err != nil {
			t.Fatal(err)
		}
		entryID, _ := res.LastInsertId()
		if _, err := db.Exec(`INSERT INTO messages (entry_id, session_id, project, role, text, timestamp, type, is_noise, content_type, tool_use_id)
			VALUES (?, 'sess-old', 'proj', 'user', ?, ?, 'user', 0, 'tool_result', ?)`, entryID, o.preview, e["timestamp"], o.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the schema diff recreates tool_outputs and the backfill
	// phase runs behind it.
	s, err := New(dbPath, projectDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AwaitStartup()

	counts, err := s.ToolOutputCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[ToolOutputSourceSidecar] != 1 || counts[ToolOutputSourceToolUseResult] != 1 || counts[ToolOutputSourceNone] != 1 {
		t.Fatalf("backfill counts %v, want one of each source", counts)
	}
	if hits := toolOutputHits(t, s, "zqxoldsidecar", UnifiedOpts{}); len(hits) != 1 {
		t.Errorf("sidecar body from backfill: %d hits, want 1", len(hits))
	}
	if hits := toolOutputHits(t, s, "zqxoldfallback", UnifiedOpts{}); len(hits) != 1 {
		t.Errorf("toolUseResult body from backfill: %d hits, want 1", len(hits))
	}

	again, err := s.ToolOutputBackfill(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again.Total() != 0 {
		t.Errorf("second pass stored %+v, want nothing", again)
	}
}

// TestResolveToolOutputGuards covers the trust boundary on the sidecar
// path and the size cap.
func TestResolveToolOutputGuards(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not for the index zqxoutside"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := resolveToolOutput(previewFor(outside, "x"), mustJSON(map[string]any{"persistedOutputPath": outside}))
	if out == nil || out.source != ToolOutputSourceNone {
		t.Errorf("a sidecar path outside .claude/projects/**/tool-results was read: %+v", out)
	}

	dir := sidecarDir(t)
	pdf := writeSidecar(t, dir, "doc.pdf", "%PDF-1.4 binary")
	out = resolveToolOutput(previewFor(pdf, "x"), mustJSON(map[string]any{"persistedOutputPath": pdf, "result": "extracted zqxpdftext"}))
	if out == nil || out.source != ToolOutputSourceToolUseResult || !strings.Contains(out.text, "zqxpdftext") {
		t.Errorf("binary sidecar should fall back to toolUseResult leaves: %+v", out)
	}
	if strings.Contains(out.text, pdf) {
		t.Errorf("metadata key value (the path) leaked into the body")
	}

	if out := resolveToolOutput("an ordinary tool result", nil); out != nil {
		t.Errorf("non-preview resolved to %+v, want nil", out)
	}

	huge := strings.Repeat("y", toolOutputMaxBytes+100)
	out = resolveToolOutput(previewFor(filepath.Join(dir, "none.txt"), "x"), mustJSON(map[string]any{"stdout": huge}))
	if out == nil || len(out.text) != toolOutputMaxBytes {
		t.Errorf("cap: got %d bytes, want %d", len(out.text), toolOutputMaxBytes)
	}
	hugeFile := writeSidecar(t, dir, "huge.txt", huge)
	out = resolveToolOutput(previewFor(hugeFile, "x"), nil)
	if out == nil || out.source != ToolOutputSourceSidecar || len(out.text) != toolOutputMaxBytes {
		t.Errorf("sidecar cap: source=%v len=%d", out.source, len(out.text))
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TestPassBudgetSpent is the 🎯T193 predicate: a near deadline spends
// the budget, a far one does not, no deadline never does.
func TestPassBudgetSpent(t *testing.T) {
	near, cancel := context.WithTimeout(context.Background(), mirrorPassReserve/2)
	defer cancel()
	if !passBudgetSpent(near) {
		t.Error("deadline inside the reserve should spend the budget")
	}
	far, cancel2 := context.WithTimeout(context.Background(), mirrorPassReserve*4)
	defer cancel2()
	if passBudgetSpent(far) {
		t.Error("deadline well outside the reserve should not spend the budget")
	}
	if passBudgetSpent(context.Background()) {
		t.Error("a context without a deadline never spends the budget")
	}
	// The reconciler itself returns nil, not an error, when the budget
	// is spent before any work.
	s := newTestStore(t, t.TempDir())
	if _, err := s.ReconcileStaleMirrors(near, time.Now()); err != nil {
		t.Errorf("spent budget reported as failure: %v", err)
	}
}
