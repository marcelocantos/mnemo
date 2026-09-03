// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Auto-backfill cadence (🎯T162). The work itself is batched and
// resumable; this is how often we look for a newly-acquired backlog
// after a family has gone quiet. Short enough that a restored backup
// converges without a restart, long enough that a finished store is
// not a full-table probe every few seconds.
const compressBackfillPoll = 2 * time.Minute

// Yield between committed batches so a multi-gigabyte run does not pin
// the writer. MCP-triggered compress_gc leaves this at zero.
const compressAutoYield = 10 * time.Millisecond

// StartCompressBackfill enqueues the historical-row packer as a
// long-lived background loop (🎯T162). Idempotent. The trigger is a
// detected backlog of compressible plain rows, not a boot or a release
// boundary. Completion does not depend on anyone knowing compress_gc.
func (s *Store) StartCompressBackfill() {
	s.backfill.mu.Lock()
	if s.backfill.started {
		s.backfill.mu.Unlock()
		return
	}
	s.backfill.started = true
	s.backfill.mu.Unlock()
	s.goLoop("compress-backfill", s.compressBackfillLoop)
}

// CompressWorkerStatus is the last decision the auto-backfill worker
// published. Empty phase means the worker has not started.
func (s *Store) CompressWorkerStatus() CompressWorkerSnapshot {
	return s.backfill.snapshot()
}

func (s *Store) compressBackfillLoop(ctx context.Context) error {
	s.compressBackfillCycle(ctx)
	ticker := time.NewTicker(compressBackfillPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.compressBackfillCycle(ctx)
		}
	}
}

func (s *Store) compressBackfillCycle(ctx context.Context) {
	if !s.autoBackfillEnabled() {
		s.backfill.setPhase(CompressPhaseDisabled, "compression.auto_backfill=false")
		return
	}
	if !s.CompressionReady() {
		s.backfill.setPhase(CompressPhaseThrottled, "compression schema not ready")
		return
	}

	anyRunning := false
	for _, family := range allFamilies {
		if ctx.Err() != nil {
			return
		}
		fs, err := familyOf(family)
		if err != nil {
			continue
		}
		if family == FamilyEntriesRaw {
			ok, err := s.EntriesMaterialised()
			if err != nil || !ok {
				s.backfill.setPhase(CompressPhaseThrottled, "entries.fields materialisation still running")
				return
			}
		}
		outstanding, err := s.familyOutstanding(fs)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("compress backfill outstanding probe failed", "family", family, "err", err)
			}
			continue
		}
		if outstanding == 0 {
			continue
		}
		if err := s.reopenIfMarkedDone(family); err != nil {
			slog.Warn("compress backfill reopen failed", "family", family, "err", err)
			continue
		}
		s.backfill.setPhase(CompressPhaseRunning, family)
		s.backfill.setYield(compressAutoYield)
		res, err := s.CompressBackfill(ctx, family)
		s.backfill.setYield(0)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrBackfillRunning) {
				anyRunning = true
				continue
			}
			slog.Warn("compress auto-backfill stopped", "family", family, "rows", res.Rows, "err", err)
			continue
		}
		slog.Info("compress auto-backfill pass",
			"family", family, "rows", res.Rows, "compressed", res.Compressed,
			"saved", res.Saved, "done", res.Done)
	}

	left, err := s.totalOutstanding()
	if err != nil && ctx.Err() == nil {
		slog.Warn("compress backfill outstanding sum failed", "err", err)
	}
	switch {
	case anyRunning || left > 0:
		s.backfill.setPhase(CompressPhaseRunning, "packing historical rows")
	default:
		s.backfill.setPhase(CompressPhaseComplete, "no compressible plain rows")
	}
}

func (s *Store) totalOutstanding() (int64, error) {
	var sum int64
	for _, family := range allFamilies {
		fs, err := familyOf(family)
		if err != nil {
			continue
		}
		n, err := s.familyOutstanding(fs)
		if err != nil {
			return 0, err
		}
		sum += n
	}
	return sum, nil
}

func (s *Store) reopenIfMarkedDone(family string) error {
	var done int
	err := s.readDB.QueryRow(`SELECT done FROM compression_gc WHERE family = ?`, family).Scan(&done)
	if err != nil {
		// No cursor yet: CompressBackfill will create one.
		return nil
	}
	if done != 1 {
		return nil
	}
	return s.reopenBackfill(family)
}

func (s *Store) autoBackfillEnabled() bool {
	s.backfill.mu.Lock()
	forced := s.backfill.forceDisabled
	s.backfill.mu.Unlock()
	if forced {
		return false
	}
	cfg, err := LoadConfig()
	if err != nil {
		return true
	}
	return cfg.Compression.AutoBackfillEnabled()
}

// disableAutoBackfillForTest stops the worker from packing. Tests only.
func (s *Store) disableAutoBackfillForTest() {
	s.backfill.mu.Lock()
	s.backfill.forceDisabled = true
	s.backfill.mu.Unlock()
}
