// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package compact distills an ongoing session's transcript into a
// structured summary (targets, decisions, files, open threads, prose
// abstract). The LLM call is behind an interface so the compactor can
// be tested without spawning claudia, and so local/remote backends
// (Sonnet via claudia, Ollama, etc.) are interchangeable.
package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// LLMResult is the summariser's response + accounting metadata.
type LLMResult struct {
	Text         string
	Model        string
	PromptTokens int
	OutputTokens int
	CostUSD      float64
}

// LLMCaller runs a single prompt against a summariser model and returns
// its response. Implementations must be safe for concurrent use.
type LLMCaller interface {
	Call(ctx context.Context, systemPrompt, userPrompt string) (LLMResult, error)
}

// Payload is the structured extraction the compactor expects the LLM
// to emit. Shape is stable across backends; backends that cannot emit
// strict JSON are wrapped in a normalising adapter.
//
// The targets_active / targets_progressed / targets_next fields land
// only when the session's CWD contains a bullseye.yaml the compactor
// could read. They are omitempty so non-bullseye repos produce the
// pre-🎯T1.4 payload shape unchanged.
type Payload struct {
	Targets           []string          `json:"targets"`
	TargetsActive     []string          `json:"targets_active,omitempty"`
	TargetsProgressed map[string]string `json:"targets_progressed,omitempty"`
	TargetsNext       string            `json:"targets_next,omitempty"`
	Decisions         []Decision        `json:"decisions"`
	Files             []string          `json:"files"`
	OpenThreads       []string          `json:"open_threads"`
	Summary           string            `json:"summary"`
}

// Decision records a choice made during the session.
type Decision struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// TargetSnapshot is one row of the target graph passed into the
// compactor's prompt so the summariser can populate the targets_*
// fields. The compactor doesn't know about bullseye's YAML schema —
// the watcher resolves the graph and hands it down as a flat list.
type TargetSnapshot struct {
	ID     string
	Name   string
	Status string
}

// TargetContext is the optional target-graph anchor for a compaction
// span. Empty fields are tolerated — the prompt only mentions a
// section when it has content. A nil *TargetContext means "no graph
// available" (non-bullseye repo, missing file, parse error already
// logged): the compactor falls back to its pre-🎯T1.4 behaviour.
type TargetContext struct {
	RepoRoot    string
	Active      []TargetSnapshot
	Achieved    []TargetSnapshot
	FrontierIDs []string
}

// SystemPrompt is the system message sent to the summariser model.
// Kept as a package constant so backends see the same contract and
// the prompt can be versioned alongside the payload schema.
const SystemPrompt = `You are a session compactor. Given transcript messages, output a JSON object with this exact shape:

{
  "targets": ["T10", ...],
  "targets_active": ["T10", ...],
  "targets_progressed": {"T3": "achieved — tool X implemented"},
  "targets_next": "T9.1",
  "decisions": [{"what": "...", "why": "..."}, ...],
  "files": ["path/to/file.go", ...],
  "open_threads": ["unfinished work", ...],
  "summary": "one or two sentences describing the span"
}

Rules:
- Output JSON only. No markdown fences, no prose commentary, no leading/trailing whitespace.
- Omit fields with no entries by using empty arrays, not nulls. Omit object/string fields entirely (or set to "") when there is nothing to say.
- "targets" are bullseye target IDs (e.g. T10, T9.4) explicitly discussed or worked on in this span. Include legacy unprefixed form for backwards compatibility.
- "targets_active" are IDs from the supplied target graph that the session moved on or treated as in-flight during this span.
- "targets_progressed" is a map from target ID to a one-line progress note (e.g. "achieved — X landed", "blocked by Y", "context refreshed"). Only include entries where progress is observable in the span.
- "targets_next" is the single ID the session is most likely to pick up next, drawn from the supplied frontier or from explicit user direction. Empty string if unclear.
- "decisions" capture choices that future sessions need to remember — include the rationale.
- "files" are paths touched, reviewed, or named as load-bearing.
- "open_threads" are tasks started but not finished, and questions raised but not answered.
- "summary" is a factual prose abstract of the span, not a rating.`

// storeBackend is the narrow slice of *store.Store the compactor uses.
// Kept as an interface so tests can inject a fake.
type storeBackend interface {
	ReadSession(sessionID string, role string, offset int, limit int) ([]store.SessionMessage, error)
	PutCompaction(c store.Compaction) (int64, error)
	LatestCompaction(sessionID string) (*store.Compaction, error)
	SessionTokens(sessionID string) (int64, int64, error)
	CompactionTokens(sessionID string) (int64, int64, error)
}

// Compactor produces compactions for sessions on demand.
type Compactor struct {
	store    storeBackend
	caller   LLMCaller
	maxMsgs  int
	maxChars int
	// maxRatio caps the cumulative summariser token cost as a fraction
	// of the tracked session's token cost (🎯T10 AC6). Default 0.10.
	maxRatio float64
}

// Config tunes the compactor. Zero values mean "use defaults".
type Config struct {
	// MaxMessages is the hard cap on messages pulled per compaction call.
	// Default: 500.
	MaxMessages int
	// MaxTranscriptChars bounds the rendered transcript size. Default: 60000.
	MaxTranscriptChars int
	// MaxTokenRatio is the upper bound on (cumulative compaction tokens)
	// divided by (session tokens). When the running ratio already meets
	// or exceeds this bound, further compactions for that session are
	// skipped via ErrBudgetExceeded. Default: 0.10 (10%).
	MaxTokenRatio float64
}

// New wires a Compactor to a Store-like backend and an LLM caller.
func New(s storeBackend, caller LLMCaller, cfg Config) *Compactor {
	c := &Compactor{
		store:    s,
		caller:   caller,
		maxMsgs:  cfg.MaxMessages,
		maxChars: cfg.MaxTranscriptChars,
		maxRatio: cfg.MaxTokenRatio,
	}
	if c.maxMsgs <= 0 {
		c.maxMsgs = 500
	}
	if c.maxChars <= 0 {
		c.maxChars = 60000
	}
	if c.maxRatio <= 0 {
		c.maxRatio = 0.10
	}
	return c
}

// ErrNothingToCompact indicates the session has no messages past the
// most recent compaction. Not a real error — callers poll on this.
var ErrNothingToCompact = errors.New("compact: nothing new to compact")

// ErrBudgetExceeded indicates the cumulative summariser cost for this
// session has reached the configured ratio of the session's own token
// cost. The watcher swallows this like ErrNothingToCompact.
var ErrBudgetExceeded = errors.New("compact: token budget exceeded for session")

// checkBudget returns ErrBudgetExceeded when the cumulative compaction
// token cost already meets or exceeds maxRatio of the session's own
// token cost. Unmeasurable sessions (zero known session tokens) are
// allowed through — the first compaction has to run before there is
// anything to measure against.
func (c *Compactor) checkBudget(sessionID string) error {
	compIn, compOut, err := c.store.CompactionTokens(sessionID)
	if err != nil {
		return fmt.Errorf("compaction tokens: %w", err)
	}
	sessIn, sessOut, err := c.store.SessionTokens(sessionID)
	if err != nil {
		return fmt.Errorf("session tokens: %w", err)
	}
	sessTotal := sessIn + sessOut
	if sessTotal == 0 {
		return nil
	}
	ratio := float64(compIn+compOut) / float64(sessTotal)
	if ratio >= c.maxRatio {
		return ErrBudgetExceeded
	}
	return nil
}

// Compact distills the next window of a session's transcript into a
// Compaction row tagged with the given connection_id. Picks up after
// the latest existing compaction's entry_id_to (0 if none). Returns
// ErrNothingToCompact if no new substantive messages have accumulated,
// or ErrBudgetExceeded if the cumulative summariser cost has reached
// MaxTokenRatio of the session's own token cost (🎯T10 AC6).
//
// connectionID is the mcpbridge ConnContext ID of the live proxy
// driving this session. It is recorded on the compaction so that
// mnemo_restore can resolve session → connection → prior compactions
// across /clear boundaries without needing a chain heuristic.
//
// targets, when non-nil, anchors the summariser's output in the
// repo's bullseye target graph (🎯T1.4). The watcher resolves it from
// the session's CWD; nil falls back to the pre-graph compaction shape.
func (c *Compactor) Compact(ctx context.Context, connectionID, sessionID string, targets *TargetContext) (*store.Compaction, error) {
	if err := c.checkBudget(sessionID); err != nil {
		return nil, err
	}

	latest, err := c.store.LatestCompaction(sessionID)
	if err != nil {
		return nil, fmt.Errorf("latest compaction: %w", err)
	}
	var fromID int64
	if latest != nil {
		fromID = latest.EntryIDTo
	}

	msgs, err := c.store.ReadSession(sessionID, "", 0, c.maxMsgs)
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	msgs = filterNew(msgs, fromID)
	if len(msgs) == 0 {
		return nil, ErrNothingToCompact
	}

	transcript := renderTranscript(msgs, c.maxChars)
	userPrompt := buildUserPrompt(targets, transcript)

	res, err := c.caller.Call(ctx, SystemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("llm call: %w", err)
	}

	payload, payloadJSON, err := parsePayload(res.Text)
	if err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}

	comp := store.Compaction{
		SessionID:    sessionID,
		ConnectionID: connectionID,
		Model:        res.Model,
		PromptTokens: res.PromptTokens,
		OutputTokens: res.OutputTokens,
		CostUSD:      res.CostUSD,
		EntryIDFrom:  fromID,
		EntryIDTo:    int64(msgs[len(msgs)-1].ID),
		PayloadJSON:  payloadJSON,
		Summary:      payload.Summary,
	}
	id, err := c.store.PutCompaction(comp)
	if err != nil {
		return nil, fmt.Errorf("put compaction: %w", err)
	}
	comp.ID = id
	return &comp, nil
}

// filterNew keeps messages with ID > fromID and drops noise markers.
func filterNew(msgs []store.SessionMessage, fromID int64) []store.SessionMessage {
	out := msgs[:0]
	for _, m := range msgs {
		if int64(m.ID) <= fromID {
			continue
		}
		if m.IsNoise {
			continue
		}
		out = append(out, m)
	}
	return out
}

// buildUserPrompt assembles the compaction user message: an optional
// target-graph preface (when the session is inside a bullseye repo),
// then the rendered transcript span. The graph section is verbatim
// data — no instructions — because rules live in SystemPrompt and
// duplicating them here would let the two drift.
func buildUserPrompt(tc *TargetContext, transcript string) string {
	var b strings.Builder
	if tc != nil && (len(tc.Active) > 0 || len(tc.Achieved) > 0) {
		b.WriteString("Bullseye target graph for this repo (")
		b.WriteString(tc.RepoRoot)
		b.WriteString("):\n")
		if len(tc.Active) > 0 {
			b.WriteString("Active:\n")
			for _, t := range tc.Active {
				fmt.Fprintf(&b, "  - %s [%s] %s\n", t.ID, t.Status, t.Name)
			}
		}
		if len(tc.Achieved) > 0 {
			b.WriteString("Achieved:\n")
			for _, t := range tc.Achieved {
				fmt.Fprintf(&b, "  - %s %s\n", t.ID, t.Name)
			}
		}
		if len(tc.FrontierIDs) > 0 {
			fmt.Fprintf(&b, "Frontier (unblocked active): %s\n",
				strings.Join(tc.FrontierIDs, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Compact the following transcript span:\n\n")
	b.WriteString(transcript)
	return b.String()
}

// renderTranscript formats messages for the LLM prompt, tail-truncating
// once the byte budget is exceeded. Kept simple: tests can verify the
// exact format, and the LLM doesn't need surrounding ceremony.
func renderTranscript(msgs []store.SessionMessage, maxChars int) string {
	var b strings.Builder
	for _, m := range msgs {
		line := fmt.Sprintf("[%s] %s\n", m.Role, m.Text)
		if b.Len()+len(line) > maxChars {
			b.WriteString("... (truncated)\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

// parsePayload extracts the structured Payload from the LLM's raw text,
// tolerating a ```json fenced wrapper if the model insists on one. The
// raw JSON (fences stripped) is returned alongside the parsed value so
// callers can store the model's exact output for inspection.
func parsePayload(raw string) (Payload, string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var p Payload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return Payload{}, "", fmt.Errorf("unmarshal: %w (raw=%q)", err, raw)
	}
	return p, s, nil
}
