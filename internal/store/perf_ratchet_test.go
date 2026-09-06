// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// The perf ratchet (🎯T165) pins the deterministic half of the
// benchmarks — payload size and statements issued — so it can run in
// every `go test ./...` on any machine, where ns/op cannot. Timing is
// locked separately by `make bench-gate` against docs/perf/baseline.txt.
//
// Every number here is locked in BOTH directions. A rise is a
// regression; a fall means something got cheaper and the lock must be
// moved deliberately, in the same commit as the change that earned it,
// so the baseline never drifts by accident. Re-lock by copying the
// values the failure message prints.
//
// selects/op counts SELECT authorisations on the read pool (see
// ReadSelectCount); it is the N+1 detector. Background workers can
// issue reads of their own, so each measurement is the minimum of a few
// runs — noise only ever adds.
const (
	ratchetSearchMessagesSelects  = 129
	ratchetSearchUnifiedSelects   = 352
	ratchetRecentActivitySelects  = 1
	ratchetUsageRepoSelects       = 5
	ratchetRecentActivityBytes30d = 36732
	ratchetUsageRepoBytes         = 17644
	ratchetUsageSessionBytes      = 101000
)

// ratchetRuns is how many times a measurement is repeated before its
// minimum is taken.
const ratchetRuns = 3

// minSelects runs f ratchetRuns times and returns the smallest number of
// read-pool SELECTs one run issued.
func minSelects(t *testing.T, f func()) int64 {
	t.Helper()
	best := int64(-1)
	for i := 0; i < ratchetRuns; i++ {
		before := store.ReadSelectCount()
		f()
		n := store.ReadSelectCount() - before
		if best < 0 || n < best {
			best = n
		}
	}
	return best
}

func TestPerfRatchet(t *testing.T) {
	s := newPerfStore(t, perfRatchetShape)

	type lock struct {
		name string
		want int64
		got  int64
	}
	var locks []lock
	check := func(name string, want, got int64) {
		locks = append(locks, lock{name, want, got})
	}

	check("search_messages.selects", ratchetSearchMessagesSelects, minSelects(t, func() {
		if _, err := s.Search(perfQuery, perfSearchLimit, "interactive", "",
			perfContext, perfContext, true); err != nil {
			t.Fatal(err)
		}
	}))

	opts := store.UnifiedOpts{
		Limit: perfSearchLimit, SessionType: "interactive",
		ContextBefore: perfContext, ContextAfter: perfContext, SubstantiveOnly: true,
	}
	check("search_unified.selects", ratchetSearchUnifiedSelects, minSelects(t, func() {
		if _, err := s.UnifiedSearchOpts(perfQuery, opts, time.Now()); err != nil {
			t.Fatal(err)
		}
	}))

	var activityBytes int
	check("recent_activity_30d.selects", ratchetRecentActivitySelects, minSelects(t, func() {
		res, err := s.RecentActivity(perfWindowDays, "")
		if err != nil {
			t.Fatal(err)
		}
		activityBytes = perfPayloadBytes(t, res)
	}))
	check("recent_activity_30d.payload_bytes", ratchetRecentActivityBytes30d, int64(activityBytes))

	var repoBytes int
	check("usage_repo.selects", ratchetUsageRepoSelects, minSelects(t, func() {
		res, err := s.Usage(store.UsageParams{Days: perfWindowDays, GroupBy: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		repoBytes = perfPayloadBytes(t, res)
	}))
	check("usage_repo.payload_bytes", ratchetUsageRepoBytes, int64(repoBytes))

	res, err := s.Usage(store.UsageParams{Days: perfWindowDays, GroupBy: "session"})
	if err != nil {
		t.Fatal(err)
	}
	check("usage_session.payload_bytes", ratchetUsageSessionBytes, int64(perfPayloadBytes(t, res)))

	for _, l := range locks {
		if l.got != l.want {
			t.Errorf("%s: locked %d, measured %d — a rise is a regression, a fall must be re-locked in the same commit",
				l.name, l.want, l.got)
		}
	}
}
