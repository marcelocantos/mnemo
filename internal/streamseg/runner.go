// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/marcelocantos/mnemo/internal/segment"
	"github.com/marcelocantos/mnemo/internal/store"
)

// Summariser is the model-facing half, kept behind an interface so the
// runner is testable without spawning a Claude process. The real
// implementation is claudiaSummariser; tests supply a scripted one.
type Summariser interface {
	// Ask sends one drip and returns the reply.
	Ask(ctx context.Context, drip string) (string, error)
	// Restart discards the conversation and begins a fresh one. The
	// runner re-seeds from the automaton's state, so nothing is lost —
	// this is how the context budget is reclaimed.
	Restart(ctx context.Context) error
	// Close releases the underlying agent.
	Close()
}

// SpanStore is the persistence the runner needs. Narrow on purpose: the
// runner should not be able to reach the rest of the store.
type SpanStore interface {
	SubstantiveMessagesSince(sessionID string, afterMsgID, limit int) ([]store.StreamMessage, error)
	PutStreamSpans(spans []store.StreamSpan) error
	StreamSealedThrough(sessionID string) (int, error)
	StreamSpanIDAt(sessionID string, from, to int) (string, error)
	MarkSuperseded(spanID, bySpanID string) error
}

// Runner follows one live session, feeding drips to a summariser and
// persisting the spans it seals.
type Runner struct {
	SessionID string
	Store     SpanStore
	Summ      Summariser
	Cfg       Config
	// DripSize is how many substantive messages are gathered before the
	// summariser is asked. Small drips mean fresher spans and more
	// calls; 🎯T132.4's sweep decides the operating point.
	DripSize int

	auto *Automaton
}

// DefaultDripSize is the measured operating point (🎯T132.4), not a
// guess. The sweep found drip 12 beats drip 24 on boundary quality
// (meanPk 0.267 vs 0.332) at roughly double the cost, because cost tracks
// CALL COUNT rather than payload — the ~45k fixed per-call overhead
// dominates an 840-byte drip by ~50x.
//
// Drip 12 is chosen accepting that, because a large drip is batch
// segmentation with extra steps: the whole value of the tier is spans
// landing while the conversation is still going.
const DefaultDripSize = 12

// Start prepares the runner, recovering from whatever is already durable.
//
// Recovery is just "resume from the last sealed span". Because span ids
// are derived from their extent, replaying a drip that was already
// processed converges on the same rows rather than duplicating them, so
// crash recovery needs no journal of its own.
func (r *Runner) Start() error {
	if r.DripSize <= 0 {
		r.DripSize = DefaultDripSize
	}
	through, err := r.Store.StreamSealedThrough(r.SessionID)
	if err != nil {
		return err
	}
	r.auto = New(r.SessionID, r.Cfg, &State{
		SessionID:     r.SessionID,
		SealedThrough: through,
	})
	return nil
}

// Step processes at most one drip. It returns the number of messages
// consumed, so a caller can back off when a session goes quiet rather
// than spinning on an empty transcript.
func (r *Runner) Step(ctx context.Context) (int, error) {
	if r.auto == nil {
		if err := r.Start(); err != nil {
			return 0, err
		}
	}
	after := r.auto.State().SealedThrough
	if tail := r.auto.State().Tail; len(tail) > 0 {
		after = tail[len(tail)-1].ID
	}

	raw, err := r.Store.SubstantiveMessagesSince(r.SessionID, after, r.DripSize)
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, nil
	}
	msgs := make([]segment.Message, 0, len(raw))
	for _, m := range raw {
		msgs = append(msgs, segment.Message{
			ID: m.ID, Role: m.Role, Text: m.Text, Timestamp: m.Timestamp,
		})
	}

	fresh := r.auto.Ingest(msgs)
	if len(fresh) == 0 {
		return 0, nil
	}

	reply, err := r.Summ.Ask(ctx, renderDrip(r.auto, fresh))
	if err != nil {
		// A failed drip is not a lost drip: nothing was sealed, so the
		// same messages are re-offered next Step. Returning the count
		// consumed would advance a cursor that never moved.
		return 0, fmt.Errorf("summariser: %w", err)
	}

	sealed := r.auto.Apply(ParseEvents(reply))
	if err := r.persist(sealed); err != nil {
		return len(fresh), err
	}
	if err := r.applySupersedes(); err != nil {
		// Losing a supersession edge degrades ranking; it does not
		// corrupt the spans. Log and continue rather than stalling the
		// whole session over it.
		slog.Warn("stream supersede not applied", "session", r.SessionID, "err", err)
	}

	if r.auto.NeedsRestart() {
		if err := r.Summ.Restart(ctx); err != nil {
			return len(fresh), fmt.Errorf("summariser restart: %w", err)
		}
		r.auto.NoteRestarted()
		slog.Info("stream summariser restarted on context budget",
			"session", r.SessionID, "sealed_through", r.auto.State().SealedThrough)
	}
	return len(fresh), nil
}

func (r *Runner) persist(sealed []Sealed) error {
	if len(sealed) == 0 {
		return nil
	}
	spans := make([]store.StreamSpan, 0, len(sealed))
	for _, s := range sealed {
		spans = append(spans, store.StreamSpan{
			SessionID: r.SessionID,
			FromMsgID: s.From,
			ToMsgID:   s.To,
			Label:     s.Label,
			Summary:   s.Summary,
		})
	}
	return r.Store.PutStreamSpans(spans)
}

// applySupersedes resolves queued supersede events against stored spans.
// Both ends must resolve: an edge pointing at a span that was never
// persisted would be a dangling reference that ranking would then have to
// defend against.
func (r *Runner) applySupersedes() error {
	for _, ev := range r.auto.Supersedes() {
		fromA, toA, okA := r.auto.SealedExtent(ev.Ref)
		fromB, toB, okB := r.auto.SealedExtent(ev.By)
		if !okA || !okB {
			continue
		}
		oldID, err := r.Store.StreamSpanIDAt(r.SessionID, fromA, toA)
		if err != nil {
			return err
		}
		newID, err := r.Store.StreamSpanIDAt(r.SessionID, fromB, toB)
		if err != nil {
			return err
		}
		if oldID == "" || newID == "" || oldID == newID {
			continue
		}
		if err := r.Store.MarkSuperseded(oldID, newID); err != nil {
			return err
		}
	}
	return nil
}

// Run drives Step until the context is cancelled, backing off when the
// session is quiet. It returns nil on cancellation — a watched session
// going quiet is the normal ending, not a failure.
func (r *Runner) Run(ctx context.Context, idle time.Duration) error {
	if idle <= 0 {
		idle = 5 * time.Second
	}
	for {
		n, err := r.Step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("stream segmentation step failed", "session", r.SessionID, "err", err)
		}
		wait := time.Duration(0)
		if n == 0 {
			wait = idle
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
