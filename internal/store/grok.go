// Grok transcript ingest (🎯T110). xAI's Grok CLI records sessions as
// directories under ~/.grok/sessions/<url-encoded-cwd>/<session-id>/,
// with an ACP-style updates.jsonl as the durable conversation log and
// summary.json for session metadata. This file transforms updates.jsonl
// into the same parsedFile intermediate the Claude/Codex paths produce,
// so the shared writer and search machinery are reused.
// See docs/design/grok-ingest.md.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// grokLine is one updates.jsonl envelope.
type grokLine struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Method    string          `json:"method"` // session/update | _x.ai/session/update
	Params    grokParams      `json:"params"`
}

type grokParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// grokUpdate is the tolerant subset of ACP sessionUpdate payloads we
// read. Unknown fields are ignored — the format has no version stamp.
type grokUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Status        string          `json:"status"`
	Meta          json.RawMessage `json:"_meta"`
}

// grokSummary is the sibling summary.json metadata file.
type grokSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	GeneratedTitle string `json:"generated_title"`
	SessionSummary string `json:"session_summary"`
	HeadBranch     string `json:"head_branch"`
}

// GrokRootsFor returns the candidate Grok session roots under a user's
// home (honouring GROK_HOME). Passed to Store.SetGrokRoots by
// registry.ForUser; existence is checked lazily by Store.grokDirs.
func GrokRootsFor(home string) []string {
	base := os.Getenv("GROK_HOME")
	if base == "" {
		base = filepath.Join(home, ".grok")
	}
	return []string{filepath.Join(base, "sessions")}
}

// isGrokUpdates reports whether a path is a Grok durable conversation
// log (updates.jsonl). Other .jsonl siblings under a Grok session dir
// (events, chat_history, rewind_points, …) must not be routed to the
// Claude or Grok parsers.
func isGrokUpdates(path string) bool {
	return filepath.Base(path) == "updates.jsonl"
}

// grokSessionID extracts the session UUID from the parent directory of
// updates.jsonl: .../<encoded-cwd>/<session-id>/updates.jsonl.
func grokSessionID(path string) string {
	return filepath.Base(filepath.Dir(path))
}

// grokProject derives a coarse project label from the session cwd.
func grokProject(cwd string) string {
	if r := extractRepo(cwd); r != "" {
		return r
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return "grok"
}

// loadGrokSummary reads the sibling summary.json next to updates.jsonl.
// Missing or unreadable summary is non-fatal — path-derived session id
// still works, and cwd/branch simply stay empty.
func loadGrokSummary(updatesPath string) (grokSummary, error) {
	p := filepath.Join(filepath.Dir(updatesPath), "summary.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return grokSummary{}, err
	}
	var s grokSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		return grokSummary{}, err
	}
	return s, nil
}

// parseGrokFile reads a Grok updates.jsonl from the given byte offset
// and transforms it into a parsedFile, mirroring parseFile/parseCodexFile.
// Pure computation — no DB access.
func parseGrokFile(path string, offset int64) (parsedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return parsedFile{}, err
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return parsedFile{}, err
		}
	}

	sessionID := grokSessionID(path)
	pf := parsedFile{
		path:      path,
		sessionID: sessionID,
		source:    "grok",
	}

	// Always load summary for cwd/branch/title. On resume (offset > 0)
	// writeParsedFile only fills empty meta fields, so re-stamping is fine.
	// summaryTopic is a provisional title; the first real user turn replaces it.
	var summaryTopic string
	if sum, err := loadGrokSummary(path); err == nil {
		if sum.Info.ID != "" {
			pf.sessionID = sum.Info.ID
		}
		pf.cwd = sum.Info.Cwd
		pf.branch = sum.HeadBranch
		if title := sum.GeneratedTitle; title != "" {
			summaryTopic = title
		} else if sum.SessionSummary != "" {
			summaryTopic = sum.SessionSummary
		}
		pf.topic = summaryTopic
	}

	reader := bufio.NewReader(f)

	handleLine := func(line []byte, thisStart int64) {
		var gl grokLine
		if json.Unmarshal(line, &gl) != nil {
			return
		}
		// Only the ACP conversation stream. _x.ai/session/update carries
		// hooks/goals/subagents/compact markers — skip for MVP.
		if gl.Method != "session/update" {
			return
		}
		if len(gl.Params.Update) == 0 {
			return
		}
		var upd grokUpdate
		if json.Unmarshal(gl.Params.Update, &upd) != nil {
			return
		}

		entryType, msgs, ok := grokRecord(&upd)
		if !ok {
			return
		}

		ts := grokTimestamp(gl.Timestamp)
		uuid := fmt.Sprintf("grok-%s-%d", pf.sessionID, thisStart)
		entryIdx := len(pf.entries)
		pf.entries = append(pf.entries, parsedRawEntry{
			entryType: entryType,
			timestamp: ts,
			raw:       injectSyntheticUUID(line, uuid),
		})

		for _, m := range msgs {
			m.entryIdx = entryIdx
			m.timestamp = ts
			// Prefer a real user turn over the generated title for topic.
			// Replace only while topic is empty or still the summary title.
			if entryType == "user" && m.contentType == "text" &&
				m.isNoise == 0 && len(m.text) >= 10 && !isBoilerplate(m.text) &&
				!isGrokInjectedContext(m.text) &&
				(pf.topic == "" || pf.topic == summaryTopic) {
				pf.topic = m.text
				if len(pf.topic) > 200 {
					pf.topic = pf.topic[:197] + "..."
				}
			}
			pf.messages = append(pf.messages, m)
		}
	}

	consumed := offset
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return parsedFile{}, fmt.Errorf("read %s: %w", path, readErr)
		}
		thisStart := consumed
		consumed += int64(len(raw))
		if line := trimLineEnding(raw); len(line) > 0 {
			handleLine(line, thisStart)
		}
		if readErr == io.EOF {
			break
		}
	}

	pf.newOffset = consumed
	pf.project = grokProject(pf.cwd)
	return pf, nil
}

// grokRecord maps one sessionUpdate to an entry type and messages.
// ok=false means skip (unknown / intermediate / empty).
func grokRecord(u *grokUpdate) (entryType string, msgs []parsedMessage, ok bool) {
	switch u.SessionUpdate {
	case "user_message_chunk":
		text := grokContentText(u.Content)
		if text == "" {
			return "", nil, false
		}
		noise := 0
		if isNoise(text) || isGrokInjectedContext(text) {
			noise = 1
		}
		return "user", []parsedMessage{{
			role: "user", typ: "user", text: text, contentType: "text", isNoise: noise,
		}}, true

	case "agent_message_chunk":
		text := grokContentText(u.Content)
		if text == "" {
			return "", nil, false
		}
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", text: text, contentType: "text",
		}}, true

	case "agent_thought_chunk":
		text := grokContentText(u.Content)
		if text == "" {
			return "", nil, false
		}
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", text: text, contentType: "thinking",
		}}, true

	case "tool_call":
		name := grokToolName(u)
		if name == "" && u.Title != "" {
			name = u.Title
		}
		toolInput, text := grokToolInput(u.RawInput)
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "tool_use",
			toolName: name, toolUseID: u.ToolCallID, toolInput: toolInput, text: text,
		}}, true

	case "tool_call_update":
		// Only completed updates carry the result body.
		if u.Status != "completed" {
			return "", nil, false
		}
		return "user", []parsedMessage{{
			role: "user", typ: "user", contentType: "tool_result",
			toolUseID: u.ToolCallID, text: grokToolResultText(u),
		}}, true
	}
	return "", nil, false
}

// isGrokInjectedContext reports system/reminder/synthetic user chunks
// that should not become the session topic.
func isGrokInjectedContext(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<system-reminder>") ||
		strings.HasPrefix(t, "<user_info>") ||
		strings.HasPrefix(t, "<git_status>") ||
		strings.HasPrefix(t, "The user sent a message while you were working:") ||
		strings.Contains(t, "Background task \"") && strings.Contains(t, "completed")
}

// grokContentText extracts text from an ACP content object or array.
func grokContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// {"type":"text","text":"..."}
	var obj struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Text != "" {
		return obj.Text
	}
	// plain string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// grokToolName prefers _meta["x.ai/tool"].name, then title.
func grokToolName(u *grokUpdate) string {
	if len(u.Meta) > 0 {
		var meta map[string]json.RawMessage
		if json.Unmarshal(u.Meta, &meta) == nil {
			if tr, ok := meta["x.ai/tool"]; ok {
				var tool struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(tr, &tool) == nil && tool.Name != "" {
					return tool.Name
				}
			}
		}
	}
	return u.Title
}

// grokToolInput resolves rawInput to tool_input JSON bytes and/or text.
func grokToolInput(raw json.RawMessage) (toolInput []byte, text string) {
	if len(raw) == 0 {
		return nil, ""
	}
	if isJSONObject(raw) {
		return append([]byte(nil), raw...), ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if isJSONObject([]byte(s)) {
			return []byte(s), ""
		}
		return nil, s
	}
	return nil, string(raw)
}

// grokToolResultText flattens a completed tool_call_update's body.
func grokToolResultText(u *grokUpdate) string {
	// Prefer nested content: [{type:"content", content:{type:"text", text:"..."}}]
	if len(u.Content) > 0 {
		var blocks []struct {
			Type    string `json:"type"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &blocks) == nil {
			var b strings.Builder
			for _, bl := range blocks {
				t := bl.Content.Text
				if t == "" {
					t = bl.Text
				}
				if t == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t)
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
		if t := grokContentText(u.Content); t != "" {
			return t
		}
	}
	if len(u.RawOutput) == 0 {
		return ""
	}
	// rawOutput may be {"type":"...","content":"..."} or a string.
	var obj struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(u.RawOutput, &obj) == nil && obj.Content != "" {
		return obj.Content
	}
	var s string
	if json.Unmarshal(u.RawOutput, &s) == nil {
		return s
	}
	return string(u.RawOutput)
}

// grokTimestamp normalises a unix-seconds number or RFC3339 string to
// RFC3339. Falls back to now when unparseable.
func grokTimestamp(raw json.RawMessage) string {
	if len(raw) == 0 {
		return time.Now().UTC().Format(time.RFC3339)
	}
	// numeric unix seconds (possibly fractional)
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if i, err := n.Int64(); err == nil {
			return time.Unix(i, 0).UTC().Format(time.RFC3339)
		}
		if f, err := n.Float64(); err == nil {
			sec := int64(f)
			nsec := int64((f - float64(sec)) * 1e9)
			return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		if _, err := time.Parse(time.RFC3339, s); err == nil {
			return s
		}
		// bare integer as string
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(i, 0).UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// injectSyntheticUUID returns the JSONL line with a synthetic "uuid"
// field inserted after the opening brace (Codex + Grok share this need).
func injectSyntheticUUID(line []byte, uuid string) []byte {
	return injectCodexUUID(line, uuid)
}

// ingestGrokFile ingests a single Grok updates.jsonl incrementally from
// its recorded offset, reusing the shared writer.
func (s *Store) ingestGrokFile(path string) error {
	s.mu.Lock()
	offset := s.offsets[path]
	s.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if offset >= info.Size() {
		return nil
	}

	pf, err := parseGrokFile(path, offset)
	if err != nil {
		return err
	}

	ws, err := newWriterState(s.writeDB)
	if err != nil {
		return err
	}
	defer func() { _ = ws.tx.Rollback() }()
	defer ws.Close()

	s.writeParsedFile(ws, pf)

	ws.Close()
	return ws.tx.Commit()
}
