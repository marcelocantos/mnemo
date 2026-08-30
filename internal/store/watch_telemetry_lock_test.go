// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

// TestFDProbeDoesNotBlockTelemetryReaders is the regression test for
// 🎯T153.
//
// sampleOpenFDs ran inside watchTel.mu, and on a host where /dev/fd is
// unreadable it falls back to spawning lsof. Every
// WatchTelemetrySnapshot caller — which means every Store.Stats and
// every diagnostics pass — then queued behind that subprocess. A
// goroutine dump caught openFDCountLsofImpl in a syscall holding the
// mutex while an ingest worker blocked on it in Stats, which is how
// shutdown missed its 3s drain grace.
func TestFDProbeDoesNotBlockTelemetryReaders(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	probing := make(chan struct{})
	release := make(chan struct{})
	orig := openFDCountProbe
	openFDCountProbe = func() int {
		close(probing)
		<-release
		return 42
	}
	t.Cleanup(func() { openFDCountProbe = orig })

	go s.noteWatchPoll(1, nil)
	<-probing // the probe is in flight

	// A reader must not wait for it.
	done := make(chan struct{})
	go func() {
		_ = s.WatchTelemetrySnapshot()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("WatchTelemetrySnapshot blocked behind the FD probe — the probe is holding watchTel.mu")
	}
	close(release)
}

// TestFDProbeIsRateLimited: the poll fires every ~5s and the probe is a
// subprocess in the worst case, so it must not run on every tick.
func TestFDProbeIsRateLimited(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	calls := 0
	orig := openFDCountProbe
	openFDCountProbe = func() int { calls++; return 7 }
	t.Cleanup(func() { openFDCountProbe = orig })

	for range 5 {
		s.noteWatchPoll(1, nil)
	}
	if calls != 1 {
		t.Errorf("probe ran %d times across 5 polls, want 1 (fdSampleInterval is %s)", calls, fdSampleInterval)
	}
	if got := s.WatchTelemetrySnapshot().ProcessOpenFDs; got != 7 {
		t.Errorf("ProcessOpenFDs = %d, want the sampled 7", got)
	}
}
