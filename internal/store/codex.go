// Codex transcript ingest (🎯T99). OpenAI's Codex CLI records sessions
// as "rollout" JSONL under ~/.codex/sessions/<YYYY>/<MM>/<DD>/ (and
// ~/.codex/archived_sessions/). The format is the OpenAI Responses API
// item stream wrapped in a thin {timestamp, type, payload} envelope —
// structurally unlike Claude Code's schema. This file transforms each
// rollout into the same parsedFile intermediate the Claude path
// produces, so the shared writer (writeParsedFile) and the entire
// search/session machinery are reused. See docs/design/codex-ingest.md.
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

// codexLine is the rollout envelope: every line is one of these.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"` // session_meta | response_item | event_msg | turn_context | ...
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is the union of the payload fields we read across the
// record types we care about. Unknown fields are ignored — the format
// has no version stamp and evolves additively, so we pin to a tolerant
// subset and skip what we don't recognise.
type codexPayload struct {
	Type string `json:"type"` // for response_item: message | function_call | reasoning | ...

	// message
	Role    string         `json:"role"`
	Content []codexContent `json:"content"`

	// tool calls (function-call shape, not Chat tool_calls)
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // function_call / tool_search_call
	Input     json.RawMessage `json:"input"`     // custom_tool_call
	CallID    string          `json:"call_id"`

	// tool outputs
	Output json.RawMessage `json:"output"`

	// reasoning
	Summary          []codexContent `json:"summary"`
	EncryptedContent string         `json:"encrypted_content"`

	// session_meta / turn_context
	ID  string    `json:"id"`
	Cwd string    `json:"cwd"`
	Git *codexGit `json:"git"`
}

type codexContent struct {
	Type string `json:"type"` // input_text | output_text | text
	Text string `json:"text"`
}

type codexGit struct {
	Branch        string `json:"branch"`
	RepositoryURL string `json:"repository_url"`
}

var codexUUIDRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// CodexRootsFor returns the candidate Codex rollout roots under a user's
// home (honouring CODEX_HOME). These are passed to Store.SetCodexRoots by
// registry.ForUser; existence is checked lazily by Store.codexDirs, so
// roots that appear later (Codex installed after mnemo starts) are still
// indexed.
func CodexRootsFor(home string) []string {
	base := os.Getenv("CODEX_HOME")
	if base == "" {
		base = filepath.Join(home, ".codex")
	}
	return []string{
		filepath.Join(base, "sessions"),
		filepath.Join(base, "archived_sessions"),
	}
}

// isCodexRollout reports whether a path is a Codex rollout transcript
// (rollout-<ts>-<uuid>.jsonl), so the watcher and batch ingest route it
// to the Codex parser rather than the Claude one.
func isCodexRollout(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

// codexSessionID extracts the session UUID from a rollout filename
// (rollout-<ts>-<uuid>.jsonl). The id is also in the session_meta line,
// but the filename is available even when ingest resumes past that
// header from a byte offset.
func codexSessionID(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if m := codexUUIDRe.FindString(base); m != "" {
		return m
	}
	return base
}

// codexProject derives a coarse project label from the session cwd.
// Repo attribution proper happens in writeParsedFile via extractRepo;
// this is just the stored project tag.
func codexProject(cwd string) string {
	if r := extractRepo(cwd); r != "" {
		return r
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return "codex"
}

// parseCodexFile reads a Codex rollout from the given byte offset and
// transforms it into a parsedFile, mirroring parseFile's contract.
// Pure computation — no DB access.
func parseCodexFile(path string, offset int64) (parsedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return parsedFile{}, err
	}
	defer f.Close()

	if offset > 0 {
		f.Seek(offset, 0)
	}

	pf := parsedFile{
		path:      path,
		sessionID: codexSessionID(path),
		source:    "codex",
	}

	reader := bufio.NewReader(f)

	// handleLine parses one rollout envelope into pf. Guard clauses skip a
	// line by returning rather than a loop `continue`, so the EOF / read-
	// error handling in the read loop always runs. thisStart is the line's
	// starting byte offset — the synthetic-uuid discriminator.
	handleLine := func(line []byte, thisStart int64) {
		var cl codexLine
		if json.Unmarshal(line, &cl) != nil {
			return // tolerate junk / unknown lines, never fail the file
		}

		switch cl.Type {
		case "session_meta", "turn_context":
			// Header / per-turn bookkeeping: harvest session metadata,
			// emit no entry. cwd and git appear here, nowhere per-record.
			var p codexPayload
			if json.Unmarshal(cl.Payload, &p) == nil {
				if p.Cwd != "" && pf.cwd == "" {
					pf.cwd = p.Cwd
				}
				if p.Git != nil && p.Git.Branch != "" && pf.branch == "" {
					pf.branch = p.Git.Branch
				}
			}
			return
		case "response_item":
			// conversation content — handled below
		default:
			return // event_msg, compacted, world_state, inter_agent_* …
		}

		var p codexPayload
		if json.Unmarshal(cl.Payload, &p) != nil {
			return
		}

		entryType, msgs, ok := codexRecord(&p)
		if !ok {
			return
		}

		ts := cl.Timestamp
		if ts == "" {
			ts = time.Now().Format(time.RFC3339)
		}

		// Store the original envelope plus an injected synthetic uuid —
		// Codex records carry none, and the entries.uuid generated
		// column (raw->>'$.uuid') drives (session_id, uuid) dedup. The
		// line's byte offset is a stable, content-independent
		// discriminator, so re-ingest is idempotent.
		uuid := fmt.Sprintf("codex-%s-%d", pf.sessionID, thisStart)
		entryIdx := len(pf.entries)
		pf.entries = append(pf.entries, parsedRawEntry{
			entryType: entryType,
			timestamp: ts,
			raw:       injectCodexUUID(line, uuid),
		})

		for _, m := range msgs {
			m.entryIdx = entryIdx
			m.timestamp = ts
			if pf.topic == "" && entryType == "user" && m.contentType == "text" &&
				m.isNoise == 0 && len(m.text) >= 10 && !isBoilerplate(m.text) &&
				!isCodexInjectedContext(m.text) {
				pf.topic = m.text
				if len(pf.topic) > 200 {
					pf.topic = pf.topic[:197] + "..."
				}
			}
			pf.messages = append(pf.messages, m)
		}
	}

	// bufio.Reader.ReadBytes has no per-line size cap, so oversized Codex
	// tool outputs are ingested rather than silently dropped (🎯T104). The
	// offset is the running count of bytes consumed (a true line boundary),
	// and a non-EOF read error aborts without advancing it.
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
	pf.project = codexProject(pf.cwd)
	return pf, nil
}

// codexRecord maps one response_item payload to an entry type and its
// content messages, in the Claude content-block vocabulary. Returns
// ok=false for records we deliberately skip (developer/system messages,
// unknown payload types).
func codexRecord(p *codexPayload) (entryType string, msgs []parsedMessage, ok bool) {
	switch p.Type {
	case "message":
		role := codexRole(p.Role)
		if role == "" {
			return "", nil, false // skip developer/system/unknown roles
		}
		text := codexJoinText(p.Content)
		if text == "" {
			return "", nil, false
		}
		noise := 0
		if isNoise(text) {
			noise = 1
		}
		return role, []parsedMessage{{
			role: role, typ: role, text: text, contentType: "text", isNoise: noise,
		}}, true

	case "function_call", "custom_tool_call", "local_shell_call", "tool_search_call", "web_search_call":
		toolInput, text := codexToolInput(p)
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "tool_use",
			toolName: p.Name, toolUseID: p.CallID, toolInput: toolInput, text: text,
		}}, true

	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		return "user", []parsedMessage{{
			role: "user", typ: "user", contentType: "tool_result",
			toolUseID: p.CallID, text: codexOutputText(p.Output),
		}}, true

	case "reasoning":
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "thinking",
			text: codexReasoningText(p),
		}}, true
	}
	return "", nil, false
}

// isCodexInjectedContext reports whether a user-role message is Codex's
// injected session preamble (the AGENTS.md blob, environment/user
// instructions) rather than a real human turn — so it isn't mistaken for
// the session topic.
func isCodexInjectedContext(text string) bool {
	return strings.HasPrefix(text, "# AGENTS.md") ||
		strings.HasPrefix(text, "<environment_context>") ||
		strings.HasPrefix(text, "<user_instructions>")
}

// codexRole maps a Responses-API role to the Claude entry/message role
// vocabulary. Developer/system instruction turns are skipped (empty).
func codexRole(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	default:
		return ""
	}
}

// codexJoinText concatenates the text of input_text/output_text/text
// content items.
func codexJoinText(content []codexContent) string {
	var b strings.Builder
	for _, c := range content {
		switch c.Type {
		case "input_text", "output_text", "text":
			if c.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// codexToolInput resolves a tool call's structured input. function_call
// arguments are a JSON-encoded *string*; custom_tool_call input is plain
// text; tool_search_call arguments are a JSON object. Returns valid JSON
// bytes for tool_input when the input is a JSON object (so the messages
// tool_* generated columns work), otherwise surfaces it as searchable
// text and leaves tool_input nil.
func codexToolInput(p *codexPayload) (toolInput []byte, text string) {
	raw := p.Arguments
	if len(raw) == 0 {
		raw = p.Input
	}
	if len(raw) == 0 {
		return nil, ""
	}
	// A JSON string: unquote, then decide whether the inner content is a
	// JSON object (function_call args) or plain text (apply_patch input).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if isJSONObject([]byte(s)) {
			return normalizeAgentToolInput([]byte(s)), ""
		}
		return nil, s
	}
	// A bare JSON value (object/array), e.g. tool_search_call args.
	if isJSONObject(raw) {
		return normalizeAgentToolInput(append([]byte(nil), raw...)), ""
	}
	return nil, string(raw)
}

// codexOutputText flattens a tool-output payload (a JSON string, or a
// structured array/object) into searchable text.
func codexOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// codexReasoningText returns any visible reasoning summary; encrypted
// chains of thought are opaque, so they become a placeholder rather than
// indexable text.
func codexReasoningText(p *codexPayload) string {
	if t := codexJoinText(p.Summary); t != "" {
		return t
	}
	if p.EncryptedContent != "" {
		return "[encrypted reasoning]"
	}
	return ""
}

// isJSONObject reports whether b is a valid JSON object (starts with
// '{'). Only objects go into messages.tool_input, which is stored via
// jsonb() and feeds object-shaped tool_* generated columns.
func isJSONObject(b []byte) bool {
	t := strings.TrimSpace(string(b))
	return len(t) > 0 && t[0] == '{' && json.Valid(b)
}

// injectCodexUUID returns the rollout line with a synthetic "uuid" field
// inserted right after the opening brace, preserving the original bytes
// otherwise (full-fidelity raw + a dedup key).
func injectCodexUUID(line []byte, uuid string) []byte {
	if len(line) == 0 || line[0] != '{' {
		return append([]byte(nil), line...)
	}
	rest := line[1:]
	if strings.TrimSpace(string(rest)) == "}" { // empty object edge case
		return []byte(`{"uuid":"` + uuid + `"}`)
	}
	prefix := `{"uuid":"` + uuid + `",`
	out := make([]byte, 0, len(prefix)+len(rest))
	out = append(out, prefix...)
	out = append(out, rest...)
	return out
}

// ingestCodexFile ingests a single Codex rollout incrementally from its
// recorded offset, reusing the shared writer. The watcher's analogue of
// ingestFile for the Codex source.
func (s *Store) ingestCodexFile(path string) error {
	s.mu.Lock()
	offset := s.offsets[path]
	s.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // file deleted between event and stat
		}
		return err
	}
	if offset >= info.Size() {
		return nil // nothing new
	}

	pf, err := parseCodexFile(path, offset)
	if err != nil {
		return err
	}

	ws, err := newWriterState(s.writeDB)
	if err != nil {
		return err
	}
	defer func() { ws.tx.Rollback() }()
	defer ws.Close()

	s.writeParsedFile(ws, pf)

	ws.Close()
	return ws.tx.Commit()
}
