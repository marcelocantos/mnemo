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
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensure()
	r := t.records[name]
	r.lastStarted = now
	r.inFlightSince = now
	t.records[name] = r
}

func (t *reconcilerTracker) markFinished(name string, now time.Time, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ensure()
	r := t.records[name]
	r.lastCompleted = now
	r.inFlightSince = time.Time{}
	if err != nil {
		r.lastErr = err.Error()
	} else {
		r.lastErr = ""
	}
	t.records[name] = r
}

// DriveStreamReconcilers runs one tick of the given streams with per-pass
// deadlines. Each stream runs in its own goroutine so a blocked pass cannot
// starve the others; registration order is not load-bearing (🎯T145).
func DriveStreamReconcilers(ctx context.Context, streams []StreamReconciler, now time.Time, track *reconcilerTracker) {
	if len(streams) == 0 {
		return
	}
	if track != nil {
		track.mu.Lock()
		track.ensure()
		track.mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, sr := range streams {
		sr := sr
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			timeout := StreamPassTimeout(sr)
			passCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if track != nil {
				track.markStarted(sr.Name(), now)
			}
			n, err := sr.Reconcile(passCtx, now)
			// If the pass outran its budget, prefer a clear timeout error.
			if passCtx.Err() != nil && (err == nil || errorsIsContext(err)) {
				err = fmtPassTimeout(sr.Name(), timeout)
			}
			if track != nil {
				track.markFinished(sr.Name(), time.Now(), err)
			}
			if err != nil {
				slog.Warn("reconcile failed", "stream", sr.Name(), "err", err)
				return
			}
			if n > 0 {
				slog.Info("reconciled", "stream", sr.Name(), "count", n)
			}
		}()
	}

	// Bound how long we wait for all wrappers. Streams that ignore cancel
	// are abandoned after maxTimeout; their goroutines may still run until
	// they honour ctx (cluster/calibration already check ctx).
	maxTimeout := DefaultMaxPassTimeout
	for _, sr := range streams {
		if t := StreamPassTimeout(sr); t > maxTimeout {
			maxTimeout = t
		}
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(maxTimeout + time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		slog.Warn("reconcile tick: abandoned still-running streams after max pass budget",
			"budget", maxTimeout)
	case <-ctx.Done():
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
			Name:        sr.Name(),
			Interval:    iv.String(),
			PassTimeout: pt.String(),
			LastStarted: rec.lastStarted,
			LastCompleted: rec.lastCompleted,
			LastError:   rec.lastErr,
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
