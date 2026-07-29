// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"errors"
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
	// calls; 🎯T132.4's sweep chose 12.
	DripSize int
	// Model is recorded in the derivation stamp (🎯T134). It does not
	// select the model — the Summariser was built with it — but a span
	// that cannot say which model drew it cannot be redrawn selectively.
	Model string

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

// finishBudget bounds the closing call. It runs on a detached context
// after cancellation, so it needs its own deadline or a wedged summariser
// would keep a "stopped" session alive indefinitely.
const finishBudget = 2 * time.Minute

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
	if errors.Is(err, ErrSpendCeiling) {
		// Terminal: stop watching this session rather than re-offering
		// the drip. Retrying past a ceiling is how a bounded overrun
		// becomes an unbounded one (🎯T139).
		slog.Warn("stream segmentation stopped: session spend ceiling reached",
			"session", r.SessionID, "err", err)
		return 0, nil
	}
	if err != nil {
		// A failed drip is not a lost drip: nothing was sealed, so the
		// same messages are re-offered next Step. Returning the count
		// consumed would advance a cursor that never moved.
		return 0, fmt.Errorf("summariser: %w", err)
	}

	sealed := r.auto.Apply(ParseEvents(reply))
	// Backstop: whatever the model declined to seal and has now held far
	// too long (🎯T132.4). Persisted alongside the model's own seals so a
	// session that never seals still yields spans rather than silence.
	sealed = append(sealed, r.auto.ForceSealStale()...)
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

// PromptVersion changes whenever SystemPrompt changes in a way that could
// move boundaries. It is part of the derivation key, so a prompt revision
// makes the spans it produced findable and redrawable (🎯T134) instead of
// silently mixed in with the ones before it.
// PromptVersion 2 rebalanced the sealing instructions: v1 named only the
// cost of sealing early and never said that an unsealed span is discarded,
// so the model held spans open and sessions produced one span where
// hindsight drew five (🎯T132.4).
const PromptVersion = 2

// derivation is the configuration fingerprint stamped on every span this
// runner writes: method/model/drip/lookahead/prompt-version.
func (r *Runner) derivation() string {
	model := r.Model
	if model == "" {
		model = "default"
	}
	k := r.Cfg.SealLookahead
	if k <= 0 {
		k = DefaultSealLookahead
	}
	return fmt.Sprintf("stream/%s/d%d/k%d/p%d", model, r.DripSize, k, PromptVersion)
}

func (r *Runner) persist(sealed []Sealed) error {
	if len(sealed) == 0 {
		return nil
	}
	deriv := r.derivation()
	spans := make([]store.StreamSpan, 0, len(sealed))
	for _, s := range sealed {
		spans = append(spans, store.StreamSpan{
			SessionID:  r.SessionID,
			FromMsgID:  s.From,
			ToMsgID:    s.To,
			Label:      s.Label,
			Summary:    s.Summary,
			Derivation: deriv,
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

// Finish closes out a session whose transcript has ended.
//
// It gives the summariser one last chance to seal its open spans properly
// — that call is what produces real summaries rather than bare labels —
// and then force-seals whatever remains. Without it every span still open
// when a session stopped was discarded, and a session's last stretch is
// often its most active.
//
// Safe to call more than once; with nothing open it does nothing and
// costs no model call.
func (r *Runner) Finish(ctx context.Context) error {
	if r.auto == nil || len(r.auto.OpenSpans()) == 0 {
		return nil
	}

	// One closing prompt. A failure here is not fatal: the force-seal
	// below still salvages the spans, just with thinner summaries.
	if reply, err := r.Summ.Ask(ctx, renderClosing(r.auto)); err == nil {
		if sealed := r.auto.Apply(ParseEvents(reply)); len(sealed) > 0 {
			if err := r.persist(sealed); err != nil {
				return err
			}
		}
	} else {
		slog.Warn("closing drip failed; force-sealing with labels only",
			"session", r.SessionID, "err", err)
	}

	if err := r.persist(r.auto.SealAllOpen(r.auto.LastTailID())); err != nil {
		return err
	}

	// The session is over and its stream spans are final, so structural
	// coverage they overlap can stop winning retrieval (🎯T132.4).
	if rt, ok := r.Store.(structuralRetirer); ok {
		if n, err := rt.RetireStructuralSpansCovered(r.SessionID); err != nil {
			slog.Warn("structural retirement failed", "session", r.SessionID, "err", err)
		} else if n > 0 {
			slog.Info("retired structural spans covered by stream spans",
				"session", r.SessionID, "count", n)
		}
	}
	return nil
}

// structuralRetirer is optional on SpanStore: the replay store used by the
// sweep has no structural spans to retire, and should not be made to
// pretend otherwise.
type structuralRetirer interface {
	RetireStructuralSpansCovered(sessionID string) (int, error)
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
			// The watcher cancels when a session leaves the live set,
			// so this is the normal end of a conversation and the point
			// at which open spans must be salvaged. A detached context
			// is used deliberately: ctx is already cancelled, and the
			// closing call is the whole reason we are here.
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishBudget)
			defer cancel()
			if err := r.Finish(fctx); err != nil {
				slog.Warn("finishing session spans failed", "session", r.SessionID, "err", err)
			}
			return nil
		case <-time.After(wait):
		}
	}
}
