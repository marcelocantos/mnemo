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
type Payload struct {
	Targets     []string   `json:"targets"`
	Decisions   []Decision `json:"decisions"`
	Files       []string   `json:"files"`
	OpenThreads []string   `json:"open_threads"`
	Summary     string     `json:"summary"`
}

// Decision records a choice made during the session.
type Decision struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// SystemPrompt is the system message sent to the summariser model.
// Kept as a package constant so backends see the same contract and
// the prompt can be versioned alongside the payload schema.
const SystemPrompt = `You are a session compactor. Given transcript messages, output a JSON object with this exact shape:

{
  "targets": ["T10", ...],
  "decisions": [{"what": "...", "why": "..."}, ...],
  "files": ["path/to/file.go", ...],
  "open_threads": ["unfinished work", ...],
  "summary": "one or two sentences describing the span"
}

Rules:
- Output JSON only. No markdown fences, no prose commentary, no leading/trailing whitespace.
- Omit fields with no entries by using empty arrays, not nulls.
- "targets" are bullseye target IDs (e.g. T10, T9.4) explicitly discussed or worked on.
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
}

// Compactor produces compactions for sessions on demand.
type Compactor struct {
	store    storeBackend
	caller   LLMCaller
	maxMsgs  int
	maxChars int
}

// Config tunes the compactor. Zero values mean "use defaults".
type Config struct {
	// MaxMessages is the hard cap on messages pulled per compaction call.
	// Default: 500.
	MaxMessages int
	// MaxTranscriptChars bounds the rendered transcript size. Default: 60000.
	MaxTranscriptChars int
}

// New wires a Compactor to a Store-like backend and an LLM caller.
func New(s storeBackend, caller LLMCaller, cfg Config) *Compactor {
	c := &Compactor{store: s, caller: caller, maxMsgs: cfg.MaxMessages, maxChars: cfg.MaxTranscriptChars}
	if c.maxMsgs <= 0 {
		c.maxMsgs = 500
	}
	if c.maxChars <= 0 {
		c.maxChars = 60000
	}
	return c
}

// ErrNothingToCompact indicates the session has no messages past the
// most recent compaction. Not a real error — callers poll on this.
var ErrNothingToCompact = errors.New("compact: nothing new to compact")

// Compact distills the next window of a session's transcript into a
// Compaction row. Picks up after the latest existing compaction's
// entry_id_to (0 if none). Returns ErrNothingToCompact if no new
// substantive messages have accumulated.
func (c *Compactor) Compact(ctx context.Context, sessionID string) (*store.Compaction, error) {
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
	userPrompt := "Compact the following transcript span:\n\n" + transcript

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
