// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"sync"
	"time"

	"github.com/marcelocantos/mnemo/internal/store/fswatch"
)

// DefaultWatchFDWarn is the soft warning threshold for process open FDs
// while the transcript watcher is running (🎯T142). Target steady-state
// is well under 5k; warn at this floor so drift is visible before panic.
const DefaultWatchFDWarn = 3000

// DefaultWatchFDFail is the fail threshold for process open FDs attributable
// to an unhealthy watch posture. Crossing this means the old kqueue-style
// failure mode (or something equally severe) may be returning.
const DefaultWatchFDFail = 8000

// WatchTelemetry is live telemetry for Store.Watch / vault tree watching
// (🎯T142). Surfaced on mnemo_status diagnostics, mnemo_doctor / GET /health,
// and logs at watch start.
type WatchTelemetry struct {
	// Running is true while Store.Watch's event loop is active.
	Running bool `json:"running"`
	// Backend is "fsevents", "fsnotify", or "none".
	Backend string `json:"backend,omitempty"`
	// Roots is the number of tree roots subscribed.
	Roots int `json:"roots"`
	// DirWatches is dir-watch count (fsnotify) or root-stream count (FSEvents).
	DirWatches int `json:"dir_watches"`
	// CapHit is true when MaxDirWatches stopped further expansion.
	CapHit bool `json:"cap_hit"`
	// ProcessOpenFDs is the last sampled process open-FD count (lsof//dev/fd).
	ProcessOpenFDs int `json:"process_open_fds,omitempty"`
	// EventsReceived counts filtered path events delivered to ingest.
	EventsReceived int64 `json:"events_received"`
	// PollTicks counts safety-poll cycles.
	PollTicks int64 `json:"poll_ticks"`
	// PollCandidates counts paths the poller reported as needing re-ingest.
	PollCandidates int64 `json:"poll_candidates"`
	// PollStated is the cumulative Stat call count from the active PollTracker
	// (best-effort; updated each tick).
	PollStated int64 `json:"poll_stated"`
	// StartedAt is when the current Watch loop began.
	StartedAt time.Time `json:"started_at,omitempty"`
	// LastEventAt is the wall time of the most recent filtered event.
	LastEventAt time.Time `json:"last_event_at,omitempty"`
	// LastPollAt is the wall time of the most recent safety poll tick.
	LastPollAt time.Time `json:"last_poll_at,omitempty"`
	// SampledAt is when ProcessOpenFDs was last measured.
	SampledAt time.Time `json:"sampled_at,omitempty"`
}

// watchTelemetryState holds the mutable snapshot guarded by its own mutex
// so Watch can update without contending on s.mu (offsets).
type watchTelemetryState struct {
	mu   sync.Mutex
	snap WatchTelemetry
}

func (s *Store) initWatchTelemetry() {
	// no-op placeholder if zero value is fine; field is on Store
}

// WatchTelemetrySnapshot returns a copy of the live watch telemetry.
func (s *Store) WatchTelemetrySnapshot() WatchTelemetry {
	s.watchTel.mu.Lock()
	defer s.watchTel.mu.Unlock()
	return s.watchTel.snap
}

// noteWatchStarted records backend identity at Watch start.
func (s *Store) noteWatchStarted(backend string, roots, dirWatches int, capHit bool) {
	fds, sampled := s.sampleOpenFDs() // before the lock (🎯T153)

	s.watchTel.mu.Lock()
	defer s.watchTel.mu.Unlock()
	s.watchTel.snap = WatchTelemetry{
		Running:    true,
		Backend:    backend,
		Roots:      roots,
		DirWatches: dirWatches,
		CapHit:     capHit,
		StartedAt:  time.Now().UTC(),
	}
	if sampled {
		s.watchTel.snap.ProcessOpenFDs = fds
		s.watchTel.snap.SampledAt = time.Now().UTC()
	}
}

// noteWatchStopped clears the running flag (daemon drain).
func (s *Store) noteWatchStopped() {
	// Deliberately does not probe: this runs on the shutdown path, where
	// the value is never read again and a subprocess would only add
	// latency to a drain that is already racing a grace period (🎯T153).
	s.watchTel.mu.Lock()
	defer s.watchTel.mu.Unlock()
	s.watchTel.snap.Running = false
}

// noteWatchEvent increments the event counter after a filtered path is handled.
func (s *Store) noteWatchEvent() {
	s.watchTel.mu.Lock()
	defer s.watchTel.mu.Unlock()
	s.watchTel.snap.EventsReceived++
	s.watchTel.snap.LastEventAt = time.Now().UTC()
}

// noteWatchPoll records one safety-poll tick.
//
// The FD probe runs BEFORE the lock (🎯T153). It was inside it, and on a
// machine where /dev/fd is unreadable it falls back to lsof — so every
// WatchTelemetrySnapshot caller, which means every Stats and every
// diagnostics pass, queued behind a subprocess. That is what made
// shutdown miss its 3s drain grace: a goroutine dump caught
// openFDCountLsofImpl in a syscall holding watchTel.mu while the ingest
// worker blocked on it in Store.Stats.
func (s *Store) noteWatchPoll(candidates int, poller *fswatch.PollTracker) {
	fds, sampled := s.sampleOpenFDs()

	s.watchTel.mu.Lock()
	defer s.watchTel.mu.Unlock()
	s.watchTel.snap.PollTicks++
	s.watchTel.snap.PollCandidates += int64(candidates)
	s.watchTel.snap.LastPollAt = time.Now().UTC()
	if poller != nil {
		s.watchTel.snap.PollStated = int64(poller.StatCallCount())
	}
	if sampled {
		s.watchTel.snap.ProcessOpenFDs = fds
		s.watchTel.snap.SampledAt = time.Now().UTC()
	}
}

// openFDCountProbe is the FD probe, indirected so a test can substitute a
// slow one and assert that readers are not blocked behind it.
var openFDCountProbe = fswatch.OpenFDCount

// fdSampleInterval is the floor between FD probes. The poll fires every
// ~5s; the probe is a directory read at best and a bounded subprocess at
// worst, and the number it produces moves slowly, so sampling every poll
// bought nothing and cost a process spawn.
const fdSampleInterval = 60 * time.Second

// sampleOpenFDs probes the process FD count without holding watchTel.mu,
// rate-limited to fdSampleInterval. Reports the count and whether this
// call actually sampled.
func (s *Store) sampleOpenFDs() (int, bool) {
	now := time.Now()
	last := s.lastFDSample.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < fdSampleInterval {
		return 0, false
	}
	if !s.lastFDSample.CompareAndSwap(last, now.UnixNano()) {
		return 0, false // another poll is sampling
	}
	n := openFDCountProbe()
	if n < 0 {
		return 0, false
	}
	return n, true
}

// EvaluateWatchHealth maps telemetry to ok/warn/fail detail for doctor.
// Returns severity name, detail, remediation (remediation empty when ok).
func EvaluateWatchHealth(t WatchTelemetry) (severity, detail, remediation string) {
	if !t.Running {
		return "warn", "transcript tree watcher not running",
			"ensure the daemon finished startup workers; check logs for watcher failed"
	}
	if t.CapHit {
		return "warn",
			"directory watch cap hit (MaxDirWatches); safety poll covers remaining paths",
			"reduce watched tree size or raise fswatch.MaxDirWatches only with FD monitoring"
	}
	fds := t.ProcessOpenFDs
	if fds >= DefaultWatchFDFail {
		return "fail",
			"process open FDs are " + itoa(fds) + " (fail threshold " + itoa(DefaultWatchFDFail) + "); risk of vnode exhaustion",
			"restart mnemo; confirm backend is fsevents on Darwin (not kqueue Walk+Add); check lsof -p $(pgrep mnemo)"
	}
	if fds >= DefaultWatchFDWarn {
		return "warn",
			"process open FDs are " + itoa(fds) + " (warn threshold " + itoa(DefaultWatchFDWarn) + ")",
			"inspect lsof; target steady-state well under 5k; see docs/design/fd-bounded-watching.md"
	}
	// Healthy
	detail = "backend=" + t.Backend +
		" roots=" + itoa(t.Roots) +
		" dir_watches=" + itoa(t.DirWatches) +
		" open_fds=" + itoa(fds) +
		" events=" + itoa64(t.EventsReceived) +
		" poll_ticks=" + itoa64(t.PollTicks)
	if t.Backend == "fsevents" || t.Backend == "fsnotify" || t.Backend == "none" {
		return "ok", detail, ""
	}
	return "ok", detail, ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func itoa64(n int64) string {
	return itoa(int(n))
}
