// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Oh My Pi (omp) transcript ingest. omp records each session as one JSONL
// file under ~/.omp/agent/sessions/<encoded-cwd>/<timestamp>_<id>.jsonl
// (PI_CODING_AGENT_DIR overrides the agent dir; OMP_HOME overrides ~/.omp).
// Lines are a session header plus a message tree (id/parentId): user and
// assistant turns, tool results, and compaction summaries. This file
// transforms that layout into the same parsedFile intermediate the
// Claude/Codex/Grok/Cursor paths produce. See docs/design/omp-ingest.md.
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

// ompIDRe matches the session id omp embeds after the last '_' in the
// filename. It is the id `omp --resume` accepts.
var ompIDRe = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// OmpRootsFor returns candidate Oh My Pi session roots for a user's home.
// Existence is checked lazily by Store.ompDirs.
//
// The default tree is ~/.omp/agent/sessions. OMP_HOME replaces ~/.omp.
// PI_CODING_AGENT_DIR replaces the agent directory itself (omp's own
// session-storage env), so its sessions child is the root. Profile trees
// under <omp-home>/profiles/<name>/agent/sessions are included too; a
// profile created after the daemon starts is picked up on the next start.
func OmpRootsFor(home string) []string {
	base := os.Getenv("OMP_HOME")
	if base == "" {
		base = filepath.Join(home, ".omp")
	}
	var roots []string
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		roots = append(roots, filepath.Join(dir, "sessions"))
	} else {
		roots = append(roots, filepath.Join(base, "agent", "sessions"))
	}
	entries, err := os.ReadDir(filepath.Join(base, "profiles"))
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			roots = append(roots, filepath.Join(base, "profiles", e.Name(), "agent", "sessions"))
		}
	}
	return dedupeStrings(roots)
}

func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// isOmpSession reports whether path is an Oh My Pi session JSONL:
// .../sessions/<encoded-cwd>/<timestamp>_<uuid>.jsonl. Other JSONL under
// ~/.omp (cache indexes, logs) must not hit the Claude parser.
func isOmpSession(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	if filepath.Base(filepath.Dir(filepath.Dir(path))) != "sessions" {
		return false
	}
	return ompSessionID(path) != ""
}

func ompSessionID(path string) string {
	stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	i := strings.LastIndex(stem, "_")
	if i < 0 || i == len(stem)-1 {
		return ""
	}
	id := stem[i+1:]
	if !ompIDRe.MatchString(id) {
		return ""
	}
	return id
}

func ompProject(cwd string) string {
	if r := extractRepo(cwd); r != "" {
		return r
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return "omp"
}

type ompLine struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Timestamp  string          `json:"timestamp"`
	Cwd        string          `json:"cwd"`
	Title      string          `json:"title"`
	Model      string          `json:"model"`
	Summary    string          `json:"summary"`
	Content    string          `json:"content"`
	CustomType string          `json:"customType"`
	Message    json.RawMessage `json:"message"`
}

type ompMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Model      string          `json:"model"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
	Synthetic  bool            `json:"synthetic"`
}

type ompBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
}

// parseOmpFile reads an Oh My Pi session JSONL from the given byte offset
// and transforms it into a parsedFile. Pure computation — no DB access.
func parseOmpFile(path string, offset int64) (parsedFile, error) {
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

	sessionID := ompSessionID(path)
	pf := parsedFile{
		path:      path,
		sessionID: sessionID,
		source:    "omp",
		project:   "omp",
	}

	reader := bufio.NewReader(f)
	lastTS := fallbackTS
	userTopic := false

	handleLine := func(line []byte, thisStart int64) {
		var ol ompLine
		if json.Unmarshal(line, &ol) != nil {
			return
		}
		if ol.Type == "" {
			return
		}
		switch ol.Type {
		case "session":
			if ol.Cwd != "" {
				pf.cwd = ol.Cwd
				pf.project = ompProject(ol.Cwd)
			}
		case "model_change":
			if ol.Model != "" {
				pf.model = ol.Model
			}
		case "title", "title_change":
			title := strings.TrimSpace(ol.Title)
			if title != "" && !userTopic {
				pf.topic = truncateRunes(title, 200)
			}
		}

		ts := strings.TrimSpace(ol.Timestamp)
		if ts == "" {
			ts = lastTS
		} else {
			lastTS = ts
		}

		// Every well-formed record is an entry. Messages are only the
		// conversational projection. Unknown and extension types stay in
		// the log so a later omp type is not dropped on the floor.
		entryType, msgs := projectOmp(&ol)
		uuid := ompEntryUUID(pf.sessionID, ol.ID, thisStart)
		raw := enrichOmpRaw(line, uuid, pf.model)
		entryIdx := len(pf.entries)
		pf.entries = append(pf.entries, parsedRawEntry{
			entryType: entryType,
			timestamp: ts,
			raw:       raw,
		})
		for _, m := range msgs {
			m.entryIdx = entryIdx
			m.timestamp = ts
			if entryType == "user" && m.contentType == "text" && m.role == "user" &&
				m.isNoise == 0 && len(m.text) >= 10 && !isBoilerplate(m.text) {
				userTopic = true
				pf.topic = truncateRunes(m.text, 200)
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

func ompEntryUUID(sessionID, recordID string, offset int64) string {
	if recordID != "" {
		return "omp-" + sessionID + "-" + recordID
	}
	return fmt.Sprintf("omp-%s-%d", sessionID, offset)
}

// projectOmp decides which records become searchable messages. The entry
// type stays omp's own type, so a compaction node is not relabelled as
// an assistant model call. Message records are the exception: they use
// user/assistant, which is what usage indexes already filter on.
//
// custom (tool starts, extension state) is not model context and is not
// projected. custom_message is. An unrecognised type is kept as an entry
// and not invented into a message.
func projectOmp(ol *ompLine) (entryType string, msgs []parsedMessage) {
	switch ol.Type {
	case "message":
		et, msgs, ok := ompMessageRecord(ol.Message)
		if !ok {
			return "message", nil
		}
		return et, msgs
	case "compaction", "branch_summary":
		summary := strings.TrimSpace(ol.Summary)
		if summary == "" {
			return ol.Type, nil
		}
		return ol.Type, []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: "[omp compaction]\n" + summary,
		}}
	case "custom_message":
		text := strings.TrimSpace(ol.Content)
		if text == "" {
			return ol.Type, nil
		}
		return ol.Type, []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: text, isNoise: 1,
		}}
	default:
		return ol.Type, nil
	}
}

func ompMessageRecord(raw json.RawMessage) (entryType string, msgs []parsedMessage, ok bool) {
	if len(raw) == 0 {
		return "", nil, false
	}
	var msg ompMessage
	if json.Unmarshal(raw, &msg) != nil {
		return "", nil, false
	}
	role := strings.ToLower(strings.TrimSpace(msg.Role))
	switch role {
	case "toolresult", "tool":
		text := ompContentText(msg.Content)
		errFlag := 0
		if msg.IsError {
			errFlag = 1
		}
		return "user", []parsedMessage{{
			role: "user", typ: "user", contentType: "tool_result",
			toolName: msg.ToolName, toolUseID: ompToolID(msg.ToolCallID),
			text: text, isError: errFlag,
		}}, true
	case "user", "assistant", "developer":
	default:
		return "", nil, false
	}

	blocks := ompBlocks(msg.Content)
	var out []parsedMessage
	for _, b := range blocks {
		switch strings.ToLower(b.Type) {
		case "thinking", "reasoning":
			text := strings.TrimSpace(b.Thinking)
			if text == "" {
				text = strings.TrimSpace(b.Text)
			}
			if text == "" {
				continue
			}
			out = append(out, parsedMessage{
				role: "assistant", typ: "assistant", contentType: "thinking",
				text: text, isNoise: 1,
			})
		case "text", "input_text", "output_text", "":
			text := strings.TrimSpace(b.Text)
			if text == "" {
				continue
			}
			noise := 0
			msgRole := role
			if role == "developer" || msg.Synthetic {
				noise = 1
				msgRole = "user"
			}
			if role == "user" {
				if isNoise(text) || isCursorInjectedContext(text) || isGrokInjectedContext(text) {
					noise = 1
				}
			}
			out = append(out, parsedMessage{
				role: msgRole, typ: msgRole, contentType: "text",
				text: text, isNoise: noise,
			})
		case "toolcall", "tool_use", "tool_call":
			input := b.Arguments
			if len(input) == 0 {
				input = b.Input
			}
			input = normalizeAgentToolInput(input)
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
				toolName: name, toolUseID: ompToolID(b.ID), toolInput: input, text: text,
			})
		}
	}
	if len(out) == 0 {
		return "", nil, false
	}
	entryType = "assistant"
	if role == "user" || role == "developer" {
		entryType = "user"
	}
	return entryType, out, true
}

func ompBlocks(raw json.RawMessage) []ompBlock {
	if len(raw) == 0 {
		return nil
	}
	var arr []ompBlock
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var one ompBlock
	if json.Unmarshal(raw, &one) == nil && (one.Type != "" || one.Text != "" || one.Thinking != "") {
		return []ompBlock{one}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
		return []ompBlock{{Type: "text", Text: s}}
	}
	return nil
}

func ompContentText(raw json.RawMessage) string {
	var parts []string
	for _, b := range ompBlocks(raw) {
		if s := strings.TrimSpace(b.Text); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// ompToolID collapses the newline omp writes between its two id forms
// so a tool call and its result still share one token.
func ompToolID(id string) string {
	id = strings.ReplaceAll(id, "\r\n", "\n")
	id = strings.ReplaceAll(id, "\n", "|")
	return strings.TrimSpace(id)
}

// enrichOmpRaw injects uuid and Claude-shaped message.model / usage so
// the entries generated columns (model, input_tokens, …) populate. omp
// records usage as input/output/cacheRead/cacheWrite; those keys are
// copied onto the Claude names without dropping the originals.
func enrichOmpRaw(line []byte, uuid, model string) []byte {
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		return injectSyntheticUUID(line, uuid)
	}
	m["uuid"] = uuid
	msg, _ := m["message"].(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	if model != "" {
		if cur, _ := msg["model"].(string); cur == "" {
			msg["model"] = model
		}
	}
	ompNormalizeUsage(msg)
	if len(msg) > 0 {
		m["message"] = msg
	}
	out, err := json.Marshal(m)
	if err != nil {
		return injectSyntheticUUID(line, uuid)
	}
	return out
}

func ompNormalizeUsage(msg map[string]any) {
	usage, _ := msg["usage"].(map[string]any)
	if usage == nil {
		return
	}
	copyKey := func(dst string, srcs ...string) {
		if _, exists := usage[dst]; exists {
			return
		}
		for _, s := range srcs {
			if v, ok := usage[s]; ok {
				usage[dst] = v
				return
			}
		}
	}
	copyKey("input_tokens", "input")
	copyKey("output_tokens", "output")
	copyKey("cache_read_input_tokens", "cacheRead", "cache_read")
	copyKey("cache_creation_input_tokens", "cacheWrite", "cache_write")
}

// ingestOmpFile ingests a single Oh My Pi session JSONL incrementally.
func (s *Store) ingestOmpFile(path string) error {
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

	pf, err := parseOmpFile(path, offset)
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
