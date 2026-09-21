// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"sync"
	"time"
)

// Snapshot holds the most recent result of every check, whichever tier's
// run produced it.
//
// It exists so that reading health costs nothing. GET /health used to call
// Registry.Run on every request — all checks, Full tier included, live —
// while the scheduler was already running the same checks on its own
// cadence and discarding the result. A health endpoint that does work
// inherits the latency of its slowest check and multiplies the load by
// however often anyone looks; on 2026-09-21 that was a dozen contended
// aggregates per request and ~12s per GET.
//
// A plain "latest report" would not do, because the tiers run on
// different cadences: a Fast run skips Full checks, so serving the last
// report as-is would drop the Full results for up to an hour after every
// Fast tick. The menu-bar shim had exactly that defect, since it was
// handed each partial report as it came. Snapshot keeps the newest result
// per check and assembles a complete report on read; each result carries
// its own CheckedAt so its age is visible rather than implied by the
// report's timestamp.
type Snapshot struct {
	mu      sync.RWMutex
	order   []string
	results map[string]Result
	latest  time.Time
}

// NewSnapshot returns an empty snapshot.
func NewSnapshot() *Snapshot {
	return &Snapshot{results: map[string]Result{}}
}

// Observe folds a report in. A full run is authoritative about which
// checks exist — it replaces membership, so a dynamic check that has gone
// away (a plugin removed, say) leaves at the next full run rather than
// lingering forever. A Fast run updates only the checks it ran.
func (s *Snapshot) Observe(rep Report, full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if full {
		s.order = s.order[:0]
		s.results = map[string]Result{}
	}
	for _, r := range rep.Results {
		if _, seen := s.results[r.Name]; !seen {
			s.order = append(s.order, r.Name)
		}
		s.results[r.Name] = r
	}
	if rep.GeneratedAt.After(s.latest) {
		s.latest = rep.GeneratedAt
	}
}

// Report assembles the snapshot into a report in registration order. ok
// is false until something has been observed, so a caller can fall back
// to a live run during the brief window after startup.
func (s *Snapshot) Report() (rep Report, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.order) == 0 {
		return Report{}, false
	}
	rep.GeneratedAt = s.latest
	for _, name := range s.order {
		r := s.results[name]
		switch r.Severity {
		case "fail":
			rep.Fail++
		case "warn":
			rep.Warn++
		default:
			rep.OK++
		}
		rep.Results = append(rep.Results, r)
	}
	return rep, true
}
