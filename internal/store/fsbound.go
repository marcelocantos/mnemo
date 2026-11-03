// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// Filesystem reads that cannot be cancelled.
//
// A read of a file the kernel cannot serve right now — a checkout that
// lives on a cloud-backed volume through a symlink, a stalled network
// mount, a file provider waiting on a consent prompt nobody will see —
// blocks inside open(2). Go has no way to interrupt that syscall, so a
// context check before or after the call does nothing: the goroutine
// simply never returns. One such read in the mirror pass parked the
// pass for a day, and because the pass never returned it never even
// logged its 45-second timeout.
//
// boundedFS runs the read on its own goroutine and stops waiting when
// ctx ends. The read itself cannot be killed, so the goroutine is left
// parked, but it is recorded under its key: while it is still blocked,
// later calls with the same key fail fast with errFSStillBlocked
// instead of parking another goroutine (and another OS thread) on the
// same path every pass. When the read finally returns, the key is
// released and the next call tries again.

// errFSStillBlocked reports that an earlier read under the same key is
// still parked in the kernel, so this call did not start another.
var errFSStillBlocked = errors.New("filesystem read still blocked from an earlier pass")

var boundedFSInFlight sync.Map // key → struct{}

// boundedFS calls fn on its own goroutine and returns its result, or
// ctx's error once ctx ends first. See the comment above for why the
// goroutine is abandoned rather than cancelled.
func boundedFS[T any](ctx context.Context, key string, fn func() T) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if _, busy := boundedFSInFlight.LoadOrStore(key, struct{}{}); busy {
		return zero, errFSStillBlocked
	}
	done := make(chan T, 1)
	go func() {
		defer boundedFSInFlight.Delete(key)
		done <- fn()
	}()
	select {
	case v := <-done:
		return v, nil
	case <-ctx.Done():
		select {
		case v := <-done:
			return v, nil
		default:
		}
		slog.Warn("filesystem read abandoned: blocked past the pass deadline",
			"key", key, "err", ctx.Err())
		return zero, ctx.Err()
	}
}
