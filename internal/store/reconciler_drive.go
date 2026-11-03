// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// StreamOverdueMultiple is how many intervals without a completed pass
// before a stream is reported overdue on mnemo_doctor / GET /health (🎯T145).
const StreamOverdueMultiple = 3

// DefaultMaxPassTimeout caps a single Reconcile wall-clock budget even when
// Interval() is very long (e.g. 24h clustering).
const DefaultMaxPassTimeout = 30 * time.Minute

// DefaultMinPassTimeout is the floor for short-interval streams.
const DefaultMinPassTimeout = 15 * time.Second

// streamWithPassTimeout is optional; streams may state an explicit bound.
type streamWithPassTimeout interface {
	PassTimeout() time.Duration
}

// StreamPassTimeout returns the max wall time for one Reconcile call.
// Explicit PassTimeout() wins; otherwise derived from Interval() and capped.
func StreamPassTimeout(sr StreamReconciler) time.Duration {
	if p, ok := sr.(streamWithPassTimeout); ok {
		if t := p.PassTimeout(); t > 0 {
			return t
		}
	}
	iv := sr.Interval()
	if iv <= 0 {
		return time.Minute
	}
	// Short-cadence streams: finish within their interval.
	if iv <= time.Minute {
		if iv < DefaultMinPassTimeout {
			return DefaultMinPassTimeout
		}
		return iv
	}
	// Medium: allow up to the interval, but never above the global cap.
	if iv > DefaultMaxPassTimeout {
		return DefaultMaxPassTimeout
	}
	return iv
}

// StreamPassStatus is one stream's last-pass telemetry for doctor.
type StreamPassStatus struct {
	Name           string    `json:"name"`
	Interval       string    `json:"interval"`
	PassTimeout    string    `json:"pass_timeout"`
	LastStarted    time.Time `json:"last_started,omitempty"`
	LastCompleted  time.Time `json:"last_completed,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	Overdue        bool      `json:"overdue"`
	OverdueDetail  string    `json:"overdue_detail,omitempty"`
	InFlightBeyond bool      `json:"in_flight_beyond_timeout,omitempty"`
}

type streamPassRecord struct {
	lastStarted   time.Time
	lastCompleted time.Time
	lastErr       string
	inFlightSince time.Time
	// running is true from launch until the stream's Reconcile call
	// actually returns — including after the driver has stopped waiting
	// on it at its pass deadline. While it is set the stream is not
	// launched again, so a pass that ignores cancellation costs one
	// parked goroutine, not one more every tick.
	running bool
}

type reconcilerTracker struct {
	mu      sync.Mutex
	records map[string]streamPassRecord
	// daemonStart is when tracking began (store open / first drive).
	daemonStart time.Time
}

func (t *reconcilerTracker) ensure() {
	if t.records == nil {
		t.records = make(map[string]streamPassRecord)
	}
	if t.daemonStart.IsZero() {
		t.daemonStart = time.Now()
	}
}

func (t *reconcilerTracker) markStarted(name string, now time.Time) {
	t.tryStart(name, now)
}

// tryStart marks name as started at now unless an earlier pass of it is
// still running, in which case it reports false and changes nothing.
func (t *reconcilerTracker) tryStart(name string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensure()
	r := t.records[name]
	if r.running {
		return false
	}
	r.running = true
	r.lastStarted = now
	r.inFlightSince = now
	t.records[name] = r
	return true
}

func (t *reconcilerTracker) markFinished(name string, now time.Time, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensure()
	r := t.records[name]
	r.running = false
	r.lastCompleted = now
	r.inFlightSince = time.Time{}
	if err != nil {
		r.lastErr = err.Error()
	} else {
		r.lastErr = ""
	}
	t.records[name] = r
}

// markAbandoned records that name's pass outran its deadline without
// returning. It is deliberately not a completion: lastCompleted and
// inFlightSince stay as they are, so health keeps reporting the stream
// as in flight past its pass timeout for as long as it really is.
func (t *reconcilerTracker) markAbandoned(name string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensure()
	r := t.records[name]
	if !r.running {
		return // it returned in the meantime; its own record stands
	}
	r.lastErr = err.Error()
	t.records[name] = r
}

// LaunchStreamReconcilers starts one pass of each given stream and
// returns without waiting for any of them (🎯T145). Each stream is
// scheduled on its own: a stream whose previous pass is still running
// is skipped this tick, and nothing one stream does can delay another
// stream's next launch. This is what the daemon's minute tick calls.
//
// Every pass runs under its own deadline (StreamPassTimeout). A pass
// that honours ctx ends there. One that does not — a read parked in
// the kernel, say — is reported as having exceeded its pass timeout at
// the deadline, and its stream is not relaunched until the call
// finally returns.
func LaunchStreamReconcilers(ctx context.Context, streams []StreamReconciler, now time.Time, track *reconcilerTracker) {
	launchStreamPasses(ctx, streams, now, track, nil)
}

// DriveStreamReconcilers runs one tick of the given streams and waits
// until each launched pass has either returned or reached its own
// deadline. Scheduling is the same as LaunchStreamReconcilers; the wait
// is bounded per stream by that stream's pass timeout, never by a
// shared budget.
func DriveStreamReconcilers(ctx context.Context, streams []StreamReconciler, now time.Time, track *reconcilerTracker) {
	var wg sync.WaitGroup
	launchStreamPasses(ctx, streams, now, track, &wg)
	wg.Wait()
}

func launchStreamPasses(ctx context.Context, streams []StreamReconciler, now time.Time, track *reconcilerTracker, wg *sync.WaitGroup) {
	if len(streams) == 0 {
		return
	}
	if track != nil {
		track.mu.Lock()
		track.ensure()
		track.mu.Unlock()
	}
	for _, sr := range streams {
		if ctx.Err() != nil {
			return
		}
		if track != nil && !track.tryStart(sr.Name(), now) {
			slog.Debug("reconcile tick: previous pass still running, not relaunching",
				"stream", sr.Name())
			continue
		}
		if wg != nil {
			wg.Add(1)
		}
		go func() {
			if wg != nil {
				defer wg.Done()
			}
			runStreamPass(ctx, sr, now, track)
		}()
	}
}

type streamPassResult struct {
	n   int
	err error
}

// runStreamPass runs one already-started pass of sr and returns when the
// pass returns or its deadline passes, whichever is first. Reconcile is
// called on a goroutine of its own so the deadline holds even when the
// stream ignores ctx; that goroutine finishes the tracker record if and
// when the call returns.
func runStreamPass(ctx context.Context, sr StreamReconciler, now time.Time, track *reconcilerTracker) {
	name := sr.Name()
	timeout := StreamPassTimeout(sr)
	passCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	abandoned := make(chan struct{})
	result := make(chan streamPassResult, 1)
	go func() {
		n, err := sr.Reconcile(passCtx, now)
		// If the pass outran its budget, prefer a clear timeout error.
		if passCtx.Err() != nil && ctx.Err() == nil && (err == nil || errorsIsContext(err)) {
			err = fmtPassTimeout(name, timeout)
		}
		if track != nil {
			track.markFinished(name, time.Now(), err)
		}
		select {
		case result <- streamPassResult{n, err}:
		case <-abandoned:
			slog.Info("reconcile: abandoned pass returned", "stream", name, "err", err)
		}
	}()

	select {
	case res := <-result:
		logStreamPass(name, res)
		return
	case <-passCtx.Done():
	}
	// A result ready at the deadline wins over reporting a timeout.
	select {
	case res := <-result:
		logStreamPass(name, res)
		return
	default:
	}
	close(abandoned)
	if ctx.Err() != nil {
		return // shutdown, not a fault of the stream
	}
	err := fmtPassTimeout(name, timeout)
	if track != nil {
		track.markAbandoned(name, err)
	}
	slog.Warn("reconcile failed", "stream", name, "err", err,
		"detail", "pass ignored cancellation; not relaunched until it returns")
}

func logStreamPass(name string, res streamPassResult) {
	if res.err != nil {
		slog.Warn("reconcile failed", "stream", name, "err", res.err)
		return
	}
	if res.n > 0 {
		slog.Info("reconciled", "stream", name, "count", res.n)
	}
}

func errorsIsContext(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context deadline") || strings.Contains(msg, "context canceled")
}

func fmtPassTimeout(name string, d time.Duration) error {
	return fmt.Errorf("stream %s exceeded pass timeout %s", name, d)
}

// StreamHealthReports builds doctor rows for registered streams (🎯T145).
func (s *Store) StreamHealthReports(streams []StreamReconciler, now time.Time) []StreamPassStatus {
	out := make([]StreamPassStatus, 0, len(streams))
	s.reconTracker.mu.Lock()
	defer s.reconTracker.mu.Unlock()
	s.reconTracker.ensure()
	start := s.reconTracker.daemonStart
	for _, sr := range streams {
		iv := sr.Interval()
		pt := StreamPassTimeout(sr)
		rec := s.reconTracker.records[sr.Name()]
		st := StreamPassStatus{
			Name:          sr.Name(),
			Interval:      iv.String(),
			PassTimeout:   pt.String(),
			LastStarted:   rec.lastStarted,
			LastCompleted: rec.lastCompleted,
			LastError:     rec.lastErr,
		}
		// Overdue: no completed pass within multiple × interval after the
		// daemon has been up long enough for that budget.
		budget := time.Duration(StreamOverdueMultiple) * iv
		if budget < time.Minute {
			budget = time.Minute
		}
		uptime := now.Sub(start)
		if uptime >= budget {
			last := rec.lastCompleted
			if last.IsZero() || now.Sub(last) >= budget {
				st.Overdue = true
				if last.IsZero() {
					st.OverdueDetail = "no completed pass since daemon start (>" + budget.String() + ")"
				} else {
					st.OverdueDetail = "last completed " + now.Sub(last).Round(time.Second).String() + " ago (threshold " + budget.String() + ")"
				}
			}
		}
		if !rec.inFlightSince.IsZero() && now.Sub(rec.inFlightSince) > pt {
			st.InFlightBeyond = true
			if !st.Overdue {
				st.Overdue = true
				st.OverdueDetail = "in-flight longer than pass timeout " + pt.String()
			}
		}
		out = append(out, st)
	}
	return out
}
