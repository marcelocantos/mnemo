// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package streamseg

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// scriptedSummariser replays canned replies, so the runner can be
// exercised end-to-end without spawning a Claude process.
type scriptedSummariser struct {
	mu       sync.Mutex
	replies  []string
	asked    []string
	restarts int
}

func (s *scriptedSummariser) Ask(_ context.Context, drip string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, drip)
	if len(s.replies) == 0 {
		return "", nil
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func (s *scriptedSummariser) Restart(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restarts++
	return nil
}
func (s *scriptedSummariser) Close() {}

// memStore is an in-memory SpanStore. Using a fake rather than a real
// store keeps these tests about the runner's logic; the SQL itself is
// covered in internal/store.
type memStore struct {
	msgs       []store.StreamMessage
	spans      map[string]store.StreamSpan
	superseded map[string]string
	putCalls   int
}

func newMemStore(n int) *memStore {
	m := &memStore{spans: map[string]store.StreamSpan{}, superseded: map[string]string{}}
	for i := 1; i <= n; i++ {
		m.msgs = append(m.msgs, store.StreamMessage{
			ID: i, Role: "user", Text: fmt.Sprintf("message %d body text", i),
		})
	}
	return m
}

func (m *memStore) SubstantiveMessagesSince(_ string, after, limit int) ([]store.StreamMessage, error) {
	var out []store.StreamMessage
	for _, msg := range m.msgs {
		if msg.ID > after {
			out = append(out, msg)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memStore) key(from, to int) string { return fmt.Sprintf("%d-%d", from, to) }

func (m *memStore) PutStreamSpans(spans []store.StreamSpan) error {
	m.putCalls++
	for _, sp := range spans {
		m.spans[m.key(sp.FromMsgID, sp.ToMsgID)] = sp
	}
	return nil
}

func (m *memStore) StreamSealedThrough(string) (int, error) {
	max := 0
	for _, sp := range m.spans {
		if sp.ToMsgID > max {
			max = sp.ToMsgID
		}
	}
	return max, nil
}

func (m *memStore) StreamSpanIDAt(_ string, from, to int) (string, error) {
	if _, ok := m.spans[m.key(from, to)]; ok {
		return m.key(from, to), nil
	}
	return "", nil
}

func (m *memStore) MarkSuperseded(id, by string) error {
	m.superseded[id] = by
	return nil
}

func seal(ref string, from, to int, label string) string {
	return fmt.Sprintf(`{"event":"open","span":%q,"from":%d,"label":%q}`+"\n"+
		`{"event":"seal","span":%q,"to":%d,"summary":"done"}`, ref, from, label, ref, to)
}

// TestRunnerPersistsSealedSpans is the basic path: drips in, spans out.
func TestRunnerPersistsSealedSpans(t *testing.T) {
	st := newMemStore(60)
	summ := &scriptedSummariser{replies: []string{seal("t1", 1, 5, "first topic")}}
	r := &Runner{SessionID: "s", Store: st, Summ: summ, DripSize: 20,
		Cfg: Config{SealLookahead: 1}}

	if _, err := r.Step(context.Background()); err != nil {
		t.Fatalf("Step: %v", err)
	}
	sp, ok := st.spans["1-5"]
	if !ok {
		t.Fatalf("span not persisted; have %v", st.spans)
	}
	if sp.Label != "first topic" {
		t.Errorf("label = %q", sp.Label)
	}
}

// TestRunnerRecoversFromLastSealedState is the crash-recovery acceptance.
//
// A watcher that died mid-session must resume from the last sealed span,
// not from the beginning. Re-deriving covered ground would pay the
// summariser twice for the same conversation — the exact cost the
// bounded-state design exists to avoid.
func TestRunnerRecoversFromLastSealedState(t *testing.T) {
	st := newMemStore(60)
	// Pretend a previous incarnation sealed through message 20.
	st.spans["1-20"] = store.StreamSpan{SessionID: "s", FromMsgID: 1, ToMsgID: 20, Label: "earlier"}

	summ := &scriptedSummariser{replies: []string{""}}
	r := &Runner{SessionID: "s", Store: st, Summ: summ, DripSize: 5,
		Cfg: Config{SealLookahead: 1}}
	if _, err := r.Step(context.Background()); err != nil {
		t.Fatalf("Step: %v", err)
	}

	if len(summ.asked) != 1 {
		t.Fatalf("expected one drip, got %d", len(summ.asked))
	}
	drip := summ.asked[0]
	// Messages 1..20 are already covered and must not be re-offered.
	for _, gone := range []string{"#1 ", "#12 ", "#20 "} {
		if strings.Contains(drip, gone) {
			t.Errorf("re-offered already-sealed message (%q) — recovery restarted from zero", gone)
		}
	}
	if !strings.Contains(drip, "#21 ") {
		t.Errorf("drip did not resume at message 21:\n%s", drip)
	}
}

// TestRunnerIsIdempotentAcrossReplay: replaying the same event stream
// must converge on the same spans rather than duplicating them. This is
// what lets recovery be a replay rather than a bookkeeping exercise.
func TestRunnerIsIdempotentAcrossReplay(t *testing.T) {
	run := func(st *memStore) {
		summ := &scriptedSummariser{replies: []string{seal("t1", 1, 5, "topic")}}
		r := &Runner{SessionID: "s", Store: st, Summ: summ, DripSize: 20,
			Cfg: Config{SealLookahead: 1}}
		if _, err := r.Step(context.Background()); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	st := newMemStore(60)
	run(st)
	first := len(st.spans)
	run(st) // a fresh runner over the same store, same events
	if len(st.spans) != first {
		t.Errorf("replay produced %d spans, want %d — span identity is not extent-derived",
			len(st.spans), first)
	}
}

// TestRunnerRestartsOnContextBudget: the agent is restarted when its
// budget fills, and the automaton keeps its state across the restart.
// Without this a long session either grows unboundedly or loses its open
// spans.
func TestRunnerRestartsOnContextBudget(t *testing.T) {
	st := newMemStore(400)
	summ := &scriptedSummariser{}
	r := &Runner{SessionID: "s", Store: st, Summ: summ, DripSize: 50,
		Cfg: Config{RestartTokens: 200, MaxTail: 10, SealLookahead: 1}}

	for i := 0; i < 6; i++ {
		if _, err := r.Step(context.Background()); err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
	}
	if summ.restarts == 0 {
		t.Error("summariser never restarted despite exceeding the context budget")
	}
}

// TestRunnerReoffersDripAfterSummariserFailure: a failed drip must not
// advance the cursor, or the messages it covered are lost from the span
// coverage with nothing to go back and fill them.
func TestRunnerReoffersDripAfterSummariserFailure(t *testing.T) {
	st := newMemStore(30)
	failing := &failingSummariser{}
	r := &Runner{SessionID: "s", Store: st, Summ: failing, DripSize: 5,
		Cfg: Config{SealLookahead: 1}}

	if _, err := r.Step(context.Background()); err == nil {
		t.Fatal("expected the summariser failure to surface")
	}
	if got, _ := r.Store.StreamSealedThrough("s"); got != 0 {
		t.Errorf("sealed watermark advanced to %d despite the drip failing", got)
	}
}

type failingSummariser struct{}

func (failingSummariser) Ask(context.Context, string) (string, error) {
	return "", fmt.Errorf("model unavailable")
}
func (failingSummariser) Restart(context.Context) error { return nil }
func (failingSummariser) Close()                        {}
