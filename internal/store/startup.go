// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

// Startup capability graph (🎯T154).
//
// Startup brings facilities online at different times, and consumers need
// them at different times. Before this, availability was signalled four
// different ways — a `done` channel for the schema, another channel for
// the codec, an atomic bool for "codec ready", a second atomic latch for
// "entries materialised" — and consumers declared their needs in three:
// call AwaitSchemaUpgrade, test CompressionReady at each write, or
// nothing at all. Nothing enumerated the set, so adding a facility meant
// finding its consumers by hand. Every miss was found in production:
//
//   - the docs writer used content_z during the pre-migration window and
//     failed 1,337 inserts, one logged error each, until the next boot;
//   - the segmenter read text_z in the same window and warned per session;
//   - boot-time writes raced anything measuring the database, which
//     surfaced as a WAL-size assertion failing only on Windows.
//
// The model here is a small DAG. A *phase* requires capabilities and
// provides capabilities; the runner starts it once its requirements
// resolve. A capability is pending, available, or unavailable-with-reason
// — the third state is what makes degraded mode declarable instead of
// discovered one SQL error at a time.
//
// Three properties follow, and each is checkable rather than hoped for:
//
//	P1  A consumer states its requirement once, via Requires, and is
//	    skipped with a single logged reason when it cannot run. It does
//	    not discover the gap through a raw SQL error per statement.
//	P2  Every background startup writer belongs to a phase, so
//	    AwaitStartup is a complete answer to "is the store quiescent?".
//	    startup_ratchet_test.go fails the build on a raw `go` statement
//	    in the startup path.
//	P3  Phase state is reported (doctor's startup.phases), so a phase
//	    that never resolves is visible in production, not only as a
//	    flaky test.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Capability names a facility that startup brings online. Consumers name
// what they need; phases name what they provide.
type Capability string

const (
	// CapSchemaCurrent: the live schema matches schema.sql, so every
	// column, view and trigger this binary knows about exists. Unavailable
	// when a deferred migration failed or was rejected (an older binary
	// against a newer DB), in which case the store keeps serving on
	// whatever schema is there.
	CapSchemaCurrent Capability = "schema.current"

	// CapCodecReady: the compression codec is loaded and new rows are
	// written packed (🎯T151). Requires CapSchemaCurrent — the *_z
	// columns arrive with the migration.
	CapCodecReady Capability = "codec.ready"

	// CapEntriesMaterialised: every pre-🎯T152 entries row has its *_m
	// columns populated, so entries_v is correct for all rows and
	// entries.raw may be compressed.
	CapEntriesMaterialised Capability = "entries.materialised"
)

// allCapabilities is the declared set, in dependency order. A capability
// missing from here is not reported and cannot be awaited.
var allCapabilities = []Capability{
	CapSchemaCurrent,
	CapCodecReady,
	CapEntriesMaterialised,
}

// capState is one capability's resolution.
type capState struct {
	done      chan struct{} // closed on resolve, whichever way
	available bool
	reason    string // why unavailable; empty when available
	at        time.Time
}

// startupGraph tracks capability resolution and phase completion.
type startupGraph struct {
	mu   sync.Mutex
	caps map[Capability]*capState
	// phases is a WaitGroup over finite background work — phases and
	// goOnce calls — so AwaitStartup covers work that provides nothing
	// (a backfill, say) as well as work that resolves a capability.
	phases sync.WaitGroup
	// loops covers long-lived workers, which AwaitStartup must not wait
	// for; Close cancels them and drains this group instead.
	loops sync.WaitGroup
	// skipped records one line per (activity, capability) so a consumer
	// that runs on a ticker logs its skip once, not once per tick.
	skipped map[string]bool
}

func newStartupGraph() *startupGraph {
	g := &startupGraph{
		caps:    make(map[Capability]*capState, len(allCapabilities)),
		skipped: map[string]bool{},
	}
	for _, c := range allCapabilities {
		g.caps[c] = &capState{done: make(chan struct{})}
	}
	return g
}

// resolve marks a capability available or unavailable. First call wins:
// a phase that fails after a partial success cannot un-provide, and a
// double resolve is a programming error rather than a race to lose.
func (g *startupGraph) resolve(c Capability, available bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.caps[c]
	if st == nil {
		slog.Error("startup: resolve of undeclared capability", "capability", c)
		return
	}
	select {
	case <-st.done:
		return // already resolved
	default:
	}
	st.available, st.reason, st.at = available, reason, time.Now()
	close(st.done)
	if available {
		slog.Info("startup: capability available", "capability", c)
	} else {
		slog.Warn("startup: capability unavailable", "capability", c, "reason", reason)
	}
}

// Have reports whether a capability is available right now. It does not
// block, and a pending capability reads as false — the hot write path
// uses this to choose the legacy statement shape.
func (s *Store) Have(c Capability) bool {
	if s.startup == nil {
		return false
	}
	st := s.startup.caps[c]
	if st == nil {
		return false
	}
	select {
	case <-st.done:
		s.startup.mu.Lock()
		defer s.startup.mu.Unlock()
		return st.available
	default:
		return false
	}
}

// Await blocks until the capability resolves and reports whether it is
// available. A capability whose phase never runs (because a requirement
// went unavailable) resolves as unavailable, so this cannot hang on a
// dependency failure.
func (s *Store) Await(c Capability) bool {
	if s.startup == nil {
		return false
	}
	st := s.startup.caps[c]
	if st == nil {
		return false
	}
	<-st.done
	s.startup.mu.Lock()
	defer s.startup.mu.Unlock()
	return st.available
}

// AwaitStartup blocks until every finite background task has finished. This is the
// single answer to "is the store quiescent?" — tests that measure the
// database, its WAL, or row counts must call it, and so must anything
// that reports on-disk size.
func (s *Store) AwaitStartup() {
	if s.startup != nil {
		s.startup.phases.Wait()
	}
}

// Requires is the consumer-side declaration. It reports whether the
// activity may proceed; when it may not, the reason is logged once per
// (activity, capability) rather than once per attempt. Use it at the top
// of any worker that touches a capability's columns:
//
//	if !s.Requires(CapSchemaCurrent, "docs ingest") { return }
//
// It does not block: a consumer on a ticker should skip this pass and
// come back, not pin a goroutine for the length of a migration.
func (s *Store) Requires(c Capability, activity string) bool {
	if s.Have(c) {
		return true
	}
	g := s.startup
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.caps[c]
	reason := "still starting"
	if st != nil {
		select {
		case <-st.done:
			reason = st.reason
		default:
		}
	}
	key := activity + "\x00" + string(c)
	if !g.skipped[key] {
		g.skipped[key] = true
		slog.Warn("startup: activity skipped",
			"activity", activity, "requires", c, "reason", reason)
	}
	return false
}

// --- Background work supervision -------------------------------------
//
// Ordering is only half the problem. An inventory of this package found
// ten background writers with no context and no completion signal:
// backfillImages, the per-session image extractor, the OCR / describer /
// embedder fan-outs, and the segment backfill among them. Nothing could
// cancel them and nothing could wait for them, which is why
// "is the store quiescent?" had no answer and why a WAL-size assertion
// could fail on one platform and pass on another.
//
// Every background goroutine in this package now goes through goOnce or
// goLoop. The distinction is what AwaitStartup means:
//
//	goOnce  finite work — a backfill, a migration, a phase. AwaitStartup
//	        waits for it, Close cancels and drains it.
//	goLoop  a ticker or event loop that runs for the store's life.
//	        AwaitStartup does NOT wait (it would never return); Close
//	        cancels and drains it.
//
// Both take s.bgCtx, so Close cancels everything by cancelling once.

// goOnce runs finite background work under supervision. AwaitStartup
// blocks until every goOnce has returned.
func (s *Store) goOnce(name string, fn func(ctx context.Context) error) {
	// A zero-value Store (a few unit tests build one to exercise a single
	// method) has no graph. Run unsupervised rather than panicking: the
	// supervision guarantee is about stores that New returned, and those
	// always have one.
	if s.startup == nil {
		go s.runSupervised(name, fn)
		return
	}
	s.startup.phases.Add(1)
	go func() {
		defer s.startup.phases.Done()
		s.runSupervised(name, fn)
	}()
}

// goLoop runs a long-lived worker under supervision. AwaitStartup does
// not wait for it; Close cancels it and drains via awaitLoops.
func (s *Store) goLoop(name string, fn func(ctx context.Context) error) {
	if s.startup == nil {
		go s.runSupervised(name, fn)
		return
	}
	s.startup.loops.Add(1)
	go func() {
		defer s.startup.loops.Done()
		s.runSupervised(name, fn)
	}()
}

// runSupervised is the shared body: skip if already cancelled, run, and
// log a failure once with the worker's name. A panic in background work
// takes down the daemon by default; recovering here would hide a bug, so
// it deliberately does not.
func (s *Store) runSupervised(name string, fn func(ctx context.Context) error) {
	if s.bgCtx == nil {
		// Zero-value Store (a few tests build one directly for a pure
		// function); run inline against a background context.
		if err := fn(context.Background()); err != nil {
			slog.Warn("background work failed", "worker", name, "err", err)
		}
		return
	}
	if s.bgCtx.Err() != nil {
		return
	}
	if err := fn(s.bgCtx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("background work failed", "worker", name, "err", err)
	}
}

// awaitLoops drains long-lived workers after cancellation, bounded so a
// worker wedged in a syscall cannot block Close forever. Reports whether
// everything stopped in time.
func (s *Store) awaitLoops(grace time.Duration) bool {
	if s.startup == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		s.startup.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(grace):
		return false
	}
}

// phase describes one unit of startup work.
type phase struct {
	// name identifies the phase in logs and diagnostics.
	name string
	// requires must all be available before run is called. If any
	// resolves unavailable, the phase is skipped and everything it
	// provides resolves unavailable with that reason.
	requires []Capability
	// provides resolve available when run returns nil, unavailable with
	// the error otherwise.
	provides []Capability
	// run does the work. It must observe ctx.
	run func(ctx context.Context) error
}

// start launches a phase. It returns immediately; the work waits for the
// phase's requirements in its own goroutine, so declaring a dependency
// never blocks the caller. Every phase is registered with the graph's
// WaitGroup before returning, so a caller that starts phases and then
// calls AwaitStartup cannot miss one.
//
// This is the only sanctioned way to start background startup work —
// see P2 in the file comment and startup_ratchet_test.go.
func (s *Store) startPhase(ctx context.Context, p phase) {
	s.startup.phases.Add(1)
	go func() {
		defer s.startup.phases.Done()
		for _, req := range p.requires {
			if !s.Await(req) {
				reason := fmt.Sprintf("requirement %s unavailable", req)
				slog.Warn("startup: phase skipped", "phase", p.name, "reason", reason)
				for _, prov := range p.provides {
					s.startup.resolve(prov, false, reason)
				}
				return
			}
		}
		if err := ctx.Err(); err != nil {
			for _, prov := range p.provides {
				s.startup.resolve(prov, false, "cancelled during startup")
			}
			return
		}
		started := time.Now()
		err := p.run(ctx)
		switch {
		case err != nil && errors.Is(err, context.Canceled):
			for _, prov := range p.provides {
				s.startup.resolve(prov, false, "cancelled during startup")
			}
		case err != nil:
			slog.Warn("startup: phase failed", "phase", p.name, "err", err,
				"elapsed", time.Since(started).Round(time.Millisecond))
			for _, prov := range p.provides {
				s.startup.resolve(prov, false, err.Error())
			}
		default:
			slog.Info("startup: phase complete", "phase", p.name,
				"elapsed", time.Since(started).Round(time.Millisecond))
			for _, prov := range p.provides {
				s.startup.resolve(prov, true, "")
			}
		}
	}()
}

// CapabilityStatus is one capability's state, for diagnostics.
type CapabilityStatus struct {
	Name      string `json:"name"`
	State     string `json:"state"` // "available" | "unavailable" | "pending"
	Reason    string `json:"reason,omitempty"`
	ResolvedS string `json:"resolved_at,omitempty"`
}

// StartupReport returns every declared capability's state, for the
// doctor's startup.phases check and GET /health. Pending capabilities
// are the interesting ones: a phase that never resolves shows here
// rather than only as a hang somewhere downstream.
func (s *Store) StartupReport() []CapabilityStatus {
	g := s.startup
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]CapabilityStatus, 0, len(g.caps))
	for c, st := range g.caps {
		cs := CapabilityStatus{Name: string(c), State: "pending"}
		select {
		case <-st.done:
			if st.available {
				cs.State = "available"
			} else {
				cs.State = "unavailable"
				cs.Reason = st.reason
			}
			cs.ResolvedS = st.at.UTC().Format(time.RFC3339)
		default:
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
