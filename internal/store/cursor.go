// Cursor Agent transcript ingest (🎯T149). Cursor records sessions as
// JSONL under ~/.cursor/projects/<encoded-cwd>/agent-transcripts/<id>/<id>.jsonl
// — role/message/content blocks, no per-line uuid or cwd. This file
// transforms that layout into the same parsedFile intermediate the
// Claude/Codex/Grok paths produce. See docs/design/cursor-ingest.md.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CursorRootsFor returns the candidate Cursor project roots under a user's
// home (honouring CURSOR_HOME). Passed to Store.SetCursorRoots by
// registry.ForUser; existence is checked lazily by Store.cursorDirs.
func CursorRootsFor(home string) []string {
	base := os.Getenv("CURSOR_HOME")
	if base == "" {
		base = filepath.Join(home, ".cursor")
	}
	return []string{filepath.Join(base, "projects")}
}

// isCursorTranscript reports whether a path is a Cursor Agent JSONL
// transcript: .../agent-transcripts/<uuid>/<uuid>.jsonl. Other JSONL
// under ~/.cursor (if any) must not hit the Claude parser.
func isCursorTranscript(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, ".jsonl")
	if stem == "" || stem == base {
		return false
	}
	parent := filepath.Base(filepath.Dir(path))
	grand := filepath.Base(filepath.Dir(filepath.Dir(path)))
	return parent == stem && grand == "agent-transcripts"
}

func cursorSessionID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func cursorProjectDir(path string) string {
	// jsonl lives at <project>/agent-transcripts/<id>/<id>.jsonl
	return filepath.Dir(filepath.Dir(filepath.Dir(path)))
}

func cursorProject(cwd string) string {
	if r := extractRepo(cwd); r != "" {
		return r
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return "cursor"
}

// cursorCwd prefers meta.json (Agent CLI chat dir, 🎯T149.1), then
// worker.log's workspacePath=, then a conservative decode of the
// project-directory slug if that path exists on disk. The slug
// encoding maps both "/" and "." to "-", so it is lossy — a
// hyphenated repo name cannot be reconstructed, and we never guess a
// path that is not a directory.
func cursorCwd(projectDir, sessionID string) string {
	if cwd := cursorReadChatMeta(cursorChatSessionDir(sessionID)).cwd; cwd != "" {
		return cwd
	}
	if cwd := cursorCwdFromWorkerLog(projectDir); cwd != "" {
		return cwd
	}
	return cursorCwdFromSlug(filepath.Base(projectDir))
}

// cursorCwdFromWorkerLog reads workspacePath= from the project's worker.log.
// Cursor JSONL has no cwd of its own; the sibling log is the durable source
// when Cursor wrote it (observed on a minority of sessions).
func cursorCwdFromWorkerLog(projectDir string) string {
	raw, err := os.ReadFile(filepath.Join(projectDir, "worker.log"))
	if err != nil {
		return ""
	}
	const key = "workspacePath="
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, key); i >= 0 {
			rest := strings.TrimSpace(line[i+len(key):])
			if j := strings.IndexAny(rest, " \t\n"); j >= 0 {
				rest = rest[:j]
			}
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

func cursorCwdFromSlug(slug string) string {
	if slug == "" {
		return ""
	}
	// Cursor drops Claude's leading "-" (encoded "/"). Remaining "-"
	// collapsed from both separators; github.com is the one we can
	// invert because /github/com/ is not a plausible path.
	candidate := "/" + strings.ReplaceAll(slug, "-", "/")
	candidate = strings.ReplaceAll(candidate, "/github/com/", "/github.com/")
	if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
		return candidate
	}
	return ""
}

type cursorLine struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Message json.RawMessage `json:"message"`
	Content json.RawMessage `json:"content"`
}

type cursorMessage struct {
	Content json.RawMessage `json:"content"`
}

type cursorBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

var cursorUserQueryRe = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)
var cursorTimestampRe = regexp.MustCompile(`(?s)<timestamp>\s*(.*?)\s*</timestamp>`)

// parseCursorFile reads a Cursor agent-transcript JSONL from the given
// byte offset and transforms it into a parsedFile. Pure computation.
func parseCursorFile(path string, offset int64) (parsedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return parsedFile{}, err
	}
	defer f.Close()

	info, statErr := f.Stat()
	fallbackTS := time.Now().UTC().Format(time.RFC3339)
	if statErr == nil {
		fallbackTS = info.ModTime().UTC().Format(time.RFC3339)
	}

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return parsedFile{}, err
		}
	}

	sessionID := cursorSessionID(path)
	chatMeta := cursorReadChatMeta(cursorChatSessionDir(sessionID))
	cwd := cursorCwd(cursorProjectDir(path), sessionID)
	pf := parsedFile{
		path:      path,
		sessionID: sessionID,
		source:    "cursor",
		cwd:       cwd,
		project:   cursorProject(cwd),
		model:     chatMeta.model,
	}
	if chatMeta.title != "" {
		pf.topic = chatMeta.title
		if len(pf.topic) > 200 {
			pf.topic = pf.topic[:197] + "..."
		}
	}

	reader := bufio.NewReader(f)
	lastTS := fallbackTS

	handleLine := func(line []byte, thisStart int64) {
		entryType, msgs, ts, ok := cursorRecord(line)
		if !ok {
			return
		}
		if ts == "" {
			ts = lastTS
		} else {
			lastTS = ts
		}
		uuid := fmt.Sprintf("cursor-%s-%d", pf.sessionID, thisStart)
		raw := enrichGrokRaw(line, uuid, pf.model, 0, 0)
		entryIdx := len(pf.entries)
		pf.entries = append(pf.entries, parsedRawEntry{
			entryType: entryType,
			timestamp: ts,
			raw:       raw,
		})
		for _, m := range msgs {
			m.entryIdx = entryIdx
			m.timestamp = ts
			if entryType == "user" && m.contentType == "text" &&
				m.isNoise == 0 && len(m.text) >= 10 && !isBoilerplate(m.text) &&
				!isCursorInjectedContext(m.text) {
				// Prefer a real user_query over the meta.json title once we have one.
				if pf.topic == "" || pf.topic == chatMeta.title {
					pf.topic = m.text
					if len(pf.topic) > 200 {
						pf.topic = pf.topic[:197] + "..."
					}
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
	return pf, nil
}

func cursorRecord(line []byte) (entryType string, msgs []parsedMessage, ts string, ok bool) {
	var cl cursorLine
	if json.Unmarshal(line, &cl) != nil {
		return "", nil, "", false
	}
	if cl.Type == "turn_ended" || cl.Type == "thinking" || cl.Type == "reasoning" {
		return "", nil, "", false
	}
	role := strings.ToLower(strings.TrimSpace(cl.Role))
	if role == "" {
		return "", nil, "", false
	}
	content := cl.Content
	if len(cl.Message) > 0 {
		var msg cursorMessage
		if json.Unmarshal(cl.Message, &msg) == nil && len(msg.Content) > 0 {
			content = msg.Content
		}
	}
	blocks := cursorBlocks(content)
	if role == "tool" {
		text := cursorBlocksText(blocks)
		id := ""
		for _, b := range blocks {
			if b.ToolUseID != "" {
				id = b.ToolUseID
				break
			}
			if b.CallID != "" {
				id = b.CallID
				break
			}
		}
		return "user", []parsedMessage{{
			role: "user", typ: "user", contentType: "tool_result",
			toolUseID: id, text: text,
		}}, "", true
	}
	if role != "user" && role != "assistant" {
		return "", nil, "", false
	}

	var out []parsedMessage
	for _, b := range blocks {
		switch b.Type {
		case "thinking", "reasoning", "redacted_thinking", "signature":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			out = append(out, parsedMessage{
				role: "assistant", typ: "assistant", contentType: "thinking",
				text: b.Text, isNoise: 1,
			})
		case "text", "input_text", "output_text":
			text := b.Text
			noise := 0
			if role == "user" {
				text, ts = cursorUserText(text)
				if text == "" {
					continue
				}
				if isNoise(text) || isCursorInjectedContext(text) {
					noise = 1
				}
			} else if isCursorGeneratedMeta(text) {
				continue
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, parsedMessage{
				role: role, typ: role, contentType: "text",
				text: text, isNoise: noise,
			})
		case "tool_use", "tool_call":
			input := b.Input
			if len(input) == 0 {
				input = b.Arguments
			}
			input = normalizeAgentToolInput(input)
			id := b.ID
			if id == "" {
				id = b.CallID
			}
			name := b.Name
			if name == "" {
				name = "unknown"
			}
			text := ""
			if len(input) > 0 {
				text = string(input)
			}
			out = append(out, parsedMessage{
				role: "assistant", typ: "assistant", contentType: "tool_use",
				toolName: name, toolUseID: id, toolInput: input, text: text,
			})
		case "tool_result", "tool_output":
			id := b.ToolUseID
			if id == "" {
				id = b.CallID
			}
			text := cursorContentText(b.Content)
			if text == "" {
				text = b.Text
			}
			errFlag := 0
			if b.IsError {
				errFlag = 1
			}
			out = append(out, parsedMessage{
				role: "user", typ: "user", contentType: "tool_result",
				toolUseID: id, text: text, isError: errFlag,
			})
		}
	}
	if len(out) == 0 {
		return "", nil, ts, false
	}
	entryType = role
	if role == "user" {
		entryType = "user"
	} else {
		entryType = "assistant"
	}
	return entryType, out, ts, true
}

func cursorBlocks(raw json.RawMessage) []cursorBlock {
	if len(raw) == 0 {
		return nil
	}
	var arr []cursorBlock
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var one cursorBlock
	if json.Unmarshal(raw, &one) == nil && (one.Type != "" || one.Text != "") {
		return []cursorBlock{one}
	}
	return nil
}

func cursorBlocksText(blocks []cursorBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text", "output_text", "":
			if s := strings.TrimSpace(b.Text); s != "" {
				parts = append(parts, s)
			}
			if s := cursorContentText(b.Content); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func cursorContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return cursorBlocksText(cursorBlocks(raw))
}

func cursorUserText(text string) (cleaned, ts string) {
	if m := cursorTimestampRe.FindStringSubmatch(text); len(m) == 2 {
		ts = cursorParseTimestamp(m[1])
	}
	if matches := cursorUserQueryRe.FindAllStringSubmatch(text, -1); len(matches) > 0 {
		var parts []string
		for _, m := range matches {
			if s := strings.TrimSpace(m[1]); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n"), ts
	}
	stripped := strings.TrimSpace(text)
	if isCursorInjectedContext(stripped) {
		return "", ts
	}
	return stripped, ts
}

func isCursorInjectedContext(text string) bool {
	t := strings.TrimLeft(text, " \t\n")
	for _, p := range []string{
		"<environment_context",
		"<user_instructions",
		"<system_reminder",
		"<system-reminder",
		"<manually_attached_skills",
		"<timestamp",
		"<mcp_meta_tools",
	} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func isCursorGeneratedMeta(text string) bool {
	t := strings.TrimSpace(text)
	return t == "[REDACTED]" || strings.HasPrefix(t, "[REDACTED]")
}

func cursorParseTimestamp(s string) string {
	s = strings.TrimSpace(s)
	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"Monday, Jan 2, 2006, 3:04 PM",
		"Monday, January 2, 2006, 3:04 PM",
	}
	// Drop parenthetical zone ("(UTC+10)") — Go won't parse it.
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[:i])
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// ingestCursorFile ingests a single Cursor agent-transcript incrementally.
func (s *Store) ingestCursorFile(path string) error {
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

	pf, err := parseCursorFile(path, offset)
	if err != nil {
		return err
	}

	ws, err := s.newWriterState()
	if err != nil {
		return err
	}
	defer func() { _ = ws.tx.Rollback() }()
	defer ws.Close()

	s.writeParsedFile(ws, pf)

	ws.Close()
	return ws.tx.Commit()
}
