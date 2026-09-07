// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// WAL maintenance.
//
// SQLite's automatic checkpointing is PASSIVE: it copies frames back
// into the main database and then reuses the -wal from offset zero. The
// file itself is never shrunk, so it sits at its high-water mark
// indefinitely. Two things are needed to keep it bounded, and they fix
// different halves of the problem:
//
//   - journal_size_limit (set in openDB) trims the file whenever a
//     checkpoint resets the WAL. This bounds the size at REST.
//   - A periodic TRUNCATE checkpoint during a quiet moment, below.
//     This is what actually forces the reset when nothing else would.
//
// Neither bounds GROWTH, because growth is not a checkpointing problem.
// A checkpoint can only advance past the oldest active reader, so one
// long read pins the WAL and everything written meanwhile piles up. On
// this corpus the worst offenders are, in order: the VACUUM INTO backup
// (a single read over the whole 21 GB database — 5-11 minutes), the
// image backfill (26s-1m42s per pass over ~1 MB BLOBs), and the
// compaction candidate scan (2-10s, cut to ~2s by 🎯T120). Shortening
// those readers is what keeps the peak down; this worker only reclaims
// the space afterwards.

const (
	// walSizeLimitBytes is the journal_size_limit applied to every
	// connection: the size the -wal is trimmed back to when a checkpoint
	// resets it. Large enough that ordinary write bursts never pay a
	// truncate, small enough that an idle daemon is not sitting on a
	// multi-gigabyte file.
	walSizeLimitBytes = 64 << 20 // 64 MiB

	// walCheckpointThreshold is the size the -wal must exceed before the
	// maintenance worker will spend a checkpoint on it. Below this the
	// file is doing its job as a write buffer and truncating would just
	// contend with writers for no gain.
	walCheckpointThreshold = 128 << 20 // 128 MiB

	// walMaintenanceInterval is how often the worker looks. The check
	// itself is one stat(2), so this can be frequent without cost.
	walMaintenanceInterval = 5 * time.Minute

	// walQuiescence is how long writes must have been idle before a
	// checkpoint is attempted. Deliberately far shorter than the backup
	// worker's window: a truncating checkpoint takes moments, so it only
	// needs a lull, not a genuinely idle machine.
	walQuiescence = 30 * time.Second
)

// StartWALMaintenance runs a periodic truncating checkpoint so the -wal
// does not sit at its high-water mark for the life of the daemon.
//
// It is deliberately opportunistic. TRUNCATE only resets the WAL when no
// other connection holds a read lock, so an attempt during a backup or a
// long backfill returns Busy and changes nothing — that is fine, the
// next tick tries again. Frames copied by a busy attempt are still
// progress.
func (s *Store) StartWALMaintenance(ctx context.Context) {
	// The caller's ctx and the store's bgCtx both stop this: goLoop
	// passes bgCtx, and Close cancels it before draining.
	s.goLoop("wal-maintenance", func(bg context.Context) error {
		ticker := time.NewTicker(walMaintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-bg.Done():
				return nil
			case <-ticker.C:
				s.maybeCheckpointWAL()
			}
		}
	})
}

// maybeCheckpointWAL truncates the WAL when it has grown past the
// threshold and writes have been quiet long enough to have a chance of
// winning the reset.
func (s *Store) maybeCheckpointWAL() {
	size, err := s.walSize()
	if err != nil || size < walCheckpointThreshold {
		return
	}
	if idle := time.Since(s.LastWriteAt()); idle < walQuiescence {
		// TRUNCATE needs a lull; PASSIVE does not (🎯T172). Deferring
		// both meant nothing checkpointed at all during exactly the
		// write bursts that grow the WAL fastest — and left the
		// diagnostic with no progress signal to read. Copy what we can
		// now and try the reset on a later, quieter tick.
		slog.Debug("wal: truncate deferred, writes still active; passive checkpoint instead",
			"wal_mb", size>>20, "idle", idle.Round(time.Second))
		s.checkpointPassive("wal-maintenance (writes active)")
		return
	}

	start := time.Now()
	res, err := s.Checkpoint()
	if err != nil {
		slog.Warn("wal: checkpoint failed", "wal_mb", size>>20, "err", err)
		return
	}
	after, _ := s.walSize()
	if res.Busy != 0 {
		// A reader held the WAL, so it could not be reset. Frames may
		// still have been copied; the next tick retries.
		slog.Info("wal: checkpoint blocked by a reader, will retry",
			"wal_mb", size>>20, "frames", res.Log,
			"checkpointed", res.Checkpointed)
		return
	}
	slog.Info("wal: checkpointed",
		"before_mb", size>>20, "after_mb", after>>20,
		"frames", res.Log, "checkpointed", res.Checkpointed,
		"elapsed", time.Since(start).Round(time.Millisecond))
}

// NoteWALSize records the latest observed -wal size and reports whether
// it grew since the previous observation.
//
// This is what lets the db.wal diagnostic distinguish a fault from a
// high-water mark. A large WAL is normal after a burst — SQLite reuses
// the file rather than shrinking it — so size alone is a lagging
// symptom. Sustained GROWTH is the signal worth alerting on, because it
// means checkpoints cannot advance: either a long reader is pinning the
// WAL or a writer is wedged.
func (s *Store) NoteWALSize(size int64) bool {
	prev := s.lastWALSize.Swap(size)
	return prev > 0 && size > prev
}

// walSize returns the current size of the -wal sidecar in bytes, or 0
// when it does not exist (nothing to reclaim).
func (s *Store) walSize() (int64, error) {
	if s.dbPath == "" {
		return 0, nil
	}
	fi, err := os.Stat(s.dbPath + "-wal")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}

// checkpointPassive copies WAL frames back into the main database
// without waiting for anything (🎯T168). Unlike the TRUNCATE checkpoint
// above it blocks neither readers nor writers and never fails on a busy
// database: it copies as far as the oldest active reader allows and
// returns. That makes it safe to call from inside a long background pass,
// which is the one place the periodic TRUNCATE worker cannot reach — it
// requires a write lull, and a pass over millions of rows never gives it
// one.
//
// Errors are logged rather than returned: a checkpoint that could not run
// is a missed reclamation, not a failure of the work in progress.
func (s *Store) checkpointPassive(reason string) {
	if s.writeDB == nil {
		return
	}
	s.walCkptAttemptUnix.Store(time.Now().Unix())
	var busy, log, checkpointed int
	row := s.writeDB.QueryRow("PRAGMA wal_checkpoint(PASSIVE)")
	if err := row.Scan(&busy, &log, &checkpointed); err != nil {
		slog.Debug("wal: passive checkpoint failed", "reason", reason, "err", err)
		return
	}
	s.noteCheckpointResult(checkpointed)
	slog.Debug("wal: passive checkpoint",
		"reason", reason, "busy", busy, "frames", log, "checkpointed", checkpointed)
}

// noteCheckpointResult records that a checkpoint ran and whether it made
// progress (🎯T172). Progress means frames were copied out of the WAL —
// the thing a pinning reader prevents. Called by both checkpoint paths.
func (s *Store) noteCheckpointResult(checkpointed int) {
	if checkpointed > 0 {
		s.walCkptStuckSinceUnix.Store(0)
		return
	}
	s.walCkptStuckSinceUnix.CompareAndSwap(0, time.Now().Unix())
}

// WALCheckpointProgress reports when a checkpoint was last attempted, and
// since when checkpoints have been copying nothing.
//
// A zero lastAttempt means none has run: absence of evidence, which the
// caller must not read as a fault. A zero stuckSince means the most
// recent checkpoint DID advance, which is positive evidence of health.
func (s *Store) WALCheckpointProgress() (lastAttempt, stuckSince time.Time) {
	if a := s.walCkptAttemptUnix.Load(); a > 0 {
		lastAttempt = time.Unix(a, 0)
	}
	if a := s.walCkptStuckSinceUnix.Load(); a > 0 {
		stuckSince = time.Unix(a, 0)
	}
	return lastAttempt, stuckSince
}

// SetWALCheckpointProgressForTest seeds the checkpoint-progress clocks so
// a diagnostic test can drive the 🎯T172 boundary without waiting out a
// real window. Tests only.
func (s *Store) SetWALCheckpointProgressForTest(attempt, stuckSince time.Time) {
	set := func(dst *atomic.Int64, t time.Time) {
		if t.IsZero() {
			dst.Store(0)
			return
		}
		dst.Store(t.Unix())
	}
	set(&s.walCkptAttemptUnix, attempt)
	set(&s.walCkptStuckSinceUnix, stuckSince)
}
