// Grok transcript ingest (🎯T110 / 🎯T111). xAI's Grok CLI records sessions
// as directories under ~/.grok/sessions/<url-encoded-cwd>/<session-id>/,
// with an ACP-style updates.jsonl as the durable conversation log and
// summary.json / signals.json for metadata richer than Claude's per-line
// envelope. This file transforms that layout into the same parsedFile
// intermediate the Claude/Codex paths produce. See docs/design/grok-ingest.md.
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

// grokUpdate is the tolerant subset of ACP + x.ai sessionUpdate payloads.
type grokUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	Status        string          `json:"status"`
	Meta          json.RawMessage `json:"_meta"`

	// plan
	Entries json.RawMessage `json:"entries"`

	// session_recap
	Summary string `json:"summary"`
	Auto    bool   `json:"auto"`

	// goal_updated (Grok-beyond-Claude)
	GoalID    string `json:"goal_id"`
	Objective string `json:"objective"`
	Phase     string `json:"phase"`
	// Status already used for tool_call_update; goal uses same JSON key.

	// subagent_*
	SubagentID      string `json:"subagent_id"`
	ChildSessionID  string `json:"child_session_id"`
	ParentSessionID string `json:"parent_session_id"`
	SubagentType    string `json:"subagent_type"`
	Description     string `json:"description"`
	// finished
	DurationMS int64  `json:"duration_ms"`
	TokensUsed int64  `json:"tokens_used"`
	Output     string `json:"output"`

	// task_completed
	TaskSnapshot json.RawMessage `json:"task_snapshot"`

	// auto_compact_*
	TokensBefore int64  `json:"tokens_before"`
	TokensAfter  int64  `json:"tokens_after"`
	Reason       string `json:"reason"`
}

// grokSummary is the sibling summary.json metadata file — often richer
// than Claude's per-line cwd/branch (model, session_kind, parent, remotes).
type grokSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	GeneratedTitle  string   `json:"generated_title"`
	SessionSummary  string   `json:"session_summary"`
	HeadBranch      string   `json:"head_branch"`
	CurrentModelID  string   `json:"current_model_id"`
	SessionKind     string   `json:"session_kind"` // "subagent" etc.
	AgentName       string   `json:"agent_name"`
	ParentSessionID string   `json:"parent_session_id"`
	GitRemotes      []string `json:"git_remotes"`
	GitRootDir      string   `json:"git_root_dir"`
	HeadCommit      string   `json:"head_commit"`
	ReasoningEffort string   `json:"reasoning_effort"`
}

// grokSignals is the subset of signals.json we map into usage / topic.
type grokSignals struct {
	ContextTokensUsed   int      `json:"contextTokensUsed"`
	ContextWindowTokens int      `json:"contextWindowTokens"`
	PrimaryModelID      string   `json:"primaryModelId"`
	ModelsUsed          []string `json:"modelsUsed"`
	ToolCallCount       int      `json:"toolCallCount"`
	TurnCount           int      `json:"turnCount"`
	CompactionCount     int      `json:"compactionCount"`
	GitCommitCount      int      `json:"gitCommitCount"`
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
// must not be routed to the Claude parser.
func isGrokUpdates(path string) bool {
	return filepath.Base(path) == "updates.jsonl"
}

func grokSessionID(path string) string {
	return filepath.Base(filepath.Dir(path))
}

func grokProject(cwd string) string {
	if r := extractRepo(cwd); r != "" {
		return r
	}
	if cwd != "" {
		return filepath.Base(cwd)
	}
	return "grok"
}

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

func loadGrokSignals(updatesPath string) (grokSignals, error) {
	p := filepath.Join(filepath.Dir(updatesPath), "signals.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		return grokSignals{}, err
	}
	var s grokSignals
	if err := json.Unmarshal(raw, &s); err != nil {
		return grokSignals{}, err
	}
	return s, nil
}

// parseGrokFile reads a Grok updates.jsonl from the given byte offset
// and transforms it into a parsedFile. Pure computation — no DB access.
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

	var summaryTopic string
	var model string
	var parentID string
	isSubagent := false
	if sum, err := loadGrokSummary(path); err == nil {
		if sum.Info.ID != "" {
			pf.sessionID = sum.Info.ID
		}
		pf.cwd = sum.Info.Cwd
		// Prefer git worktree root for attribution when present.
		if sum.GitRootDir != "" && pf.cwd == "" {
			pf.cwd = sum.GitRootDir
		}
		pf.branch = sum.HeadBranch
		model = sum.CurrentModelID
		parentID = sum.ParentSessionID
		if sum.SessionKind == "subagent" {
			isSubagent = true
		}
		if title := sum.GeneratedTitle; title != "" {
			summaryTopic = title
		} else if sum.SessionSummary != "" {
			summaryTopic = sum.SessionSummary
		}
		// Beyond Claude: agent_name + objective-ish title for topic seed.
		if summaryTopic == "" && sum.AgentName != "" {
			summaryTopic = "grok:" + sum.AgentName
		}
		pf.topic = summaryTopic
		// Prefer remotes for repo when cwd is a worktree outside github.com layout.
		if r := extractRepo(pf.cwd); r == "" && len(sum.GitRemotes) > 0 {
			if r := repoFromRemote(sum.GitRemotes[0]); r != "" {
				// stash via cwd-less project override below
				pf.project = r
			}
		}
	}

	// session_type is derived from messages.project in the SQL trigger
	// (project == "subagents" → subagent). Keep real repo in session_meta
	// via cwd; classify subagents like Claude's path convention.
	if isSubagent {
		pf.project = "subagents"
	}

	// Session-level usage (Grok has no per-turn Anthropic usage envelope).
	// Stamped onto a synthetic assistant entry so mnemo_usage sees tokens.
	var sig grokSignals
	if s, err := loadGrokSignals(path); err == nil {
		sig = s
		if model == "" {
			model = sig.PrimaryModelID
		}
		if model == "" && len(sig.ModelsUsed) > 0 {
			model = sig.ModelsUsed[0]
		}
	}

	reader := bufio.NewReader(f)

	// chainHints collected from subagent_spawned for post-write edges.
	// Stored on pf via synthetic messages; parent from summary is handled
	// in ingestGrokFile after write.

	handleLine := func(line []byte, thisStart int64) {
		var gl grokLine
		if json.Unmarshal(line, &gl) != nil {
			return
		}
		// Conversation stream + x.ai extensions (goals, recap, subagents…).
		// Skip pure noise (hooks) inside grokRecord.
		if gl.Method != "session/update" && gl.Method != "_x.ai/session/update" {
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
		// Stamp model (and optional usage) so generated entry columns fire.
		raw := enrichGrokRaw(line, uuid, model, 0, 0)
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
				!isGrokInjectedContext(m.text) &&
				(pf.topic == "" || pf.topic == summaryTopic) {
				pf.topic = m.text
				if len(pf.topic) > 200 {
					pf.topic = pf.topic[:197] + "..."
				}
			}
			pf.messages = append(pf.messages, m)
		}

		// Child spawn edge: record as message text; chain written at ingest.
		if upd.SessionUpdate == "subagent_spawned" && upd.ChildSessionID != "" {
			// parent is this session; child is successor
			_ = parentID // summary parent is for this session as child
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

	// Session-level usage snapshot (Grok-beyond-Claude: context window
	// occupancy lives in signals.json, not per-turn usage).
	// Only when parsing from the start so incremental tails don't double-count.
	if offset == 0 && (sig.ContextTokensUsed > 0 || sig.ToolCallCount > 0) {
		usageText := fmt.Sprintf(
			"[grok signals] model=%s context_tokens=%d/%d turns=%d tools=%d compactions=%d git_commits=%d",
			model, sig.ContextTokensUsed, sig.ContextWindowTokens,
			sig.TurnCount, sig.ToolCallCount, sig.CompactionCount, sig.GitCommitCount,
		)
		// Put context tokens in input_tokens so usage rollups are non-zero;
		// output_tokens left 0 (Grok does not split the same way).
		uuid := fmt.Sprintf("grok-%s-signals-usage", pf.sessionID)
		usageRaw, _ := json.Marshal(map[string]any{
			"uuid": uuid,
			"type": "assistant",
			"message": map[string]any{
				"model": model,
				"usage": map[string]any{
					"input_tokens":  sig.ContextTokensUsed,
					"output_tokens": 0,
				},
			},
			"source": "grok_signals",
			"text":   usageText,
		})
		ts := time.Now().UTC().Format(time.RFC3339)
		if len(pf.entries) > 0 && pf.entries[len(pf.entries)-1].timestamp != "" {
			ts = pf.entries[len(pf.entries)-1].timestamp
		}
		entryIdx := len(pf.entries)
		pf.entries = append(pf.entries, parsedRawEntry{
			entryType: "assistant",
			timestamp: ts,
			raw:       usageRaw,
		})
		pf.messages = append(pf.messages, parsedMessage{
			entryIdx: entryIdx, role: "assistant", typ: "assistant",
			text: usageText, timestamp: ts, contentType: "text", isNoise: 1,
		})
	}

	// Parent edge for forks/subagents — carried via synthetic note on pf.
	// writeParsedFile doesn't know chains; ingestGrokFile writes them.
	if parentID != "" {
		pf.parentSessionID = parentID
		pf.chainMechanism = "grok_parent"
	}

	pf.newOffset = consumed
	if pf.project == "" || pf.project == "subagents" {
		// project "subagents" for type classification; if we already set a
		// remote-derived project, keep it only when not a subagent.
		if isSubagent {
			pf.project = "subagents"
		} else if pf.project == "" {
			pf.project = grokProject(pf.cwd)
		}
	} else if !isSubagent {
		// remote-derived project already set
	} else {
		pf.project = "subagents"
	}
	// Stash model for callers (tests); already on entry raw.
	pf.model = model
	return pf, nil
}

// grokRecord maps one sessionUpdate to an entry type and messages.
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
		if u.Status != "completed" {
			return "", nil, false
		}
		return "user", []parsedMessage{{
			role: "user", typ: "user", contentType: "tool_result",
			toolUseID: u.ToolCallID, text: grokToolResultText(u),
		}}, true

	// --- Beyond Claude: first-class Grok / ACP signals ---

	case "session_recap":
		if strings.TrimSpace(u.Summary) == "" {
			return "", nil, false
		}
		prefix := "[grok recap]"
		if u.Auto {
			prefix = "[grok recap auto]"
		}
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: prefix + " " + u.Summary,
		}}, true

	case "plan":
		text := grokPlanText(u.Entries)
		if text == "" {
			return "", nil, false
		}
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: "[grok plan]\n" + text,
		}}, true

	case "goal_updated":
		// Grok goals have no Claude analogue — index as durable progress text.
		if u.Objective == "" && u.GoalID == "" {
			return "", nil, false
		}
		text := fmt.Sprintf("[grok goal] id=%s objective=%s status=%s phase=%s",
			u.GoalID, u.Objective, u.Status, u.Phase)
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: text, isNoise: 0,
		}}, true

	case "subagent_spawned":
		text := fmt.Sprintf("[grok subagent spawned] child=%s type=%s desc=%s parent=%s",
			u.ChildSessionID, u.SubagentType, u.Description, u.ParentSessionID)
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: text, isNoise: 1,
		}}, true

	case "subagent_finished":
		text := fmt.Sprintf("[grok subagent finished] child=%s status=%s duration_ms=%d tokens=%d output=%s",
			u.ChildSessionID, u.Status, u.DurationMS, u.TokensUsed, truncateRunes(u.Output, 200))
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "text",
			text: text, isNoise: 1,
		}}, true

	case "task_completed":
		text := grokTaskCompletedText(u.TaskSnapshot)
		if text == "" {
			return "", nil, false
		}
		return "user", []parsedMessage{{
			role: "user", typ: "user", contentType: "tool_result",
			text: text, isNoise: 0,
		}}, true

	case "auto_compact_started", "auto_compact_completed", "compaction_checkpoint":
		text := fmt.Sprintf("[grok compact] %s before=%d after=%d reason=%s",
			u.SessionUpdate, u.TokensBefore, u.TokensAfter, u.Reason)
		return "assistant", []parsedMessage{{
			role: "assistant", typ: "assistant", contentType: "thinking",
			text: text, isNoise: 1,
		}}, true

	case "hook_execution", "turn_completed", "task_backgrounded",
		"available_commands_update", "current_mode_update",
		"retry_state", "image_compressed", "rewind_marker":
		return "", nil, false
	}
	return "", nil, false
}

func isGrokInjectedContext(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<system-reminder>") ||
		strings.HasPrefix(t, "<user_info>") ||
		strings.HasPrefix(t, "<git_status>") ||
		strings.HasPrefix(t, "The user sent a message while you were working:") ||
		(strings.Contains(t, "Background task \"") && strings.Contains(t, "completed"))
}

func grokContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Text != "" {
		return obj.Text
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

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
	// Titles sometimes look like "Execute `…`" — prefer bare name when meta missing.
	t := strings.TrimSpace(u.Title)
	if strings.HasPrefix(t, "Execute ") || strings.HasPrefix(t, "Search tools:") {
		return t
	}
	return t
}

func grokToolInput(raw json.RawMessage) (toolInput []byte, text string) {
	if len(raw) == 0 {
		return nil, ""
	}
	if isJSONObject(raw) {
		return normalizeAgentToolInput(raw), ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if isJSONObject([]byte(s)) {
			return normalizeAgentToolInput([]byte(s)), ""
		}
		return nil, s
	}
	return nil, string(raw)
}

func grokToolResultText(u *grokUpdate) string {
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

func grokPlanText(entries json.RawMessage) string {
	if len(entries) == 0 {
		return ""
	}
	var items []struct {
		Content  string `json:"content"`
		Priority string `json:"priority"`
		Status   string `json:"status"`
	}
	if json.Unmarshal(entries, &items) != nil {
		return string(entries)
	}
	var b strings.Builder
	for _, it := range items {
		if it.Content == "" {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", it.Status, it.Content, it.Priority)
	}
	return strings.TrimRight(b.String(), "\n")
}

func grokTaskCompletedText(snap json.RawMessage) string {
	if len(snap) == 0 {
		return ""
	}
	var s struct {
		TaskID      string `json:"task_id"`
		Command     string `json:"command"`
		ExitCode    *int   `json:"exit_code"`
		Output      string `json:"output"`
		Description string `json:"description"`
	}
	if json.Unmarshal(snap, &s) != nil {
		return "[grok task completed] " + string(snap)
	}
	code := "?"
	if s.ExitCode != nil {
		code = strconv.Itoa(*s.ExitCode)
	}
	out := truncateRunes(s.Output, 500)
	return fmt.Sprintf("[grok task completed] id=%s exit=%s desc=%s cmd=%s\n%s",
		s.TaskID, code, s.Description, s.Command, out)
}

func grokTimestamp(raw json.RawMessage) string {
	if len(raw) == 0 {
		return time.Now().UTC().Format(time.RFC3339)
	}
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
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(i, 0).UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// enrichGrokRaw injects uuid + Claude-shaped message.model/usage so the
// entries table generated columns (model, input_tokens, …) populate.
func enrichGrokRaw(line []byte, uuid, model string, inTok, outTok int) []byte {
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
		msg["model"] = model
	}
	if inTok > 0 || outTok > 0 {
		msg["usage"] = map[string]any{
			"input_tokens":  inTok,
			"output_tokens": outTok,
		}
	}
	if len(msg) > 0 {
		m["message"] = msg
	}
	out, err := json.Marshal(m)
	if err != nil {
		return injectSyntheticUUID(line, uuid)
	}
	return out
}

// repoFromRemote extracts org/repo from a git remote URL.
func repoFromRemote(remote string) string {
	r := strings.TrimSpace(remote)
	r = strings.TrimSuffix(r, ".git")
	// https://github.com/org/repo or ssh://git@github.com/org/repo
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/",
		"ssh://git@github.com/", "git://github.com/",
	} {
		if strings.HasPrefix(r, prefix) {
			r = strings.TrimPrefix(r, prefix)
			parts := strings.Split(r, "/")
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				return parts[0] + "/" + parts[1]
			}
			return ""
		}
	}
	// git@host:org/repo
	if i := strings.Index(r, ":"); i >= 0 && strings.Contains(r[:i], "@") {
		path := r[i+1:]
		parts := strings.Split(path, "/")
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return parts[0] + "/" + parts[1]
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func injectSyntheticUUID(line []byte, uuid string) []byte {
	return injectCodexUUID(line, uuid)
}

// ingestGrokFile ingests a single Grok updates.jsonl incrementally.
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

// fieldAfter pulls the next whitespace-delimited token after key in s.
func fieldAfter(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	rest = strings.TrimSpace(rest)
	if j := strings.IndexAny(rest, " \t\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}
