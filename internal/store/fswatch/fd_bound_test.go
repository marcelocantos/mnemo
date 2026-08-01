// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package fswatch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestFDBoundOracle drives the shipped New/Start path on a synthetic corpus
// large enough that the old kqueue Walk+Add-every-dir mode would open O(files)
// FDs. Pass: open-FD delta stays far below file count (T142 / T142.6).
func TestFDBoundOracle(t *testing.T) {
	if OpenFDCount() < 0 {
		t.Skip("/dev/fd not available")
	}
	root := t.TempDir()
	// 80 dirs × 40 files = 3200 files — enough to expose per-file FD growth.
	const dirs, filesPer = 80, 40
	fileCount, err := SyntheticCorpus(root, dirs, filesPer)
	if err != nil {
		t.Fatal(err)
	}
	if fileCount < 1000 {
		t.Fatalf("corpus too small: %d", fileCount)
	}

	// Warm /dev/fd listing path.
	_ = OpenFDCount()
	before := OpenFDCount()

	w, err := New(Config{
		Roots:         []string{root},
		Mode:          ModeTranscript,
		MaxDirWatches: MaxDirWatches,
		Latency:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Allow backend startup.
	time.Sleep(200 * time.Millisecond)
	after := OpenFDCount()
	if after < 0 || before < 0 {
		t.Fatal("OpenFDCount failed")
	}
	delta := after - before
	t.Logf("GOOS=%s backend=%s files=%d dirs_or_streams=%d fd_before=%d fd_after=%d delta=%d cap_hit=%v",
		runtime.GOOS, w.Backend(), fileCount, w.DirWatchCount(), before, after, delta, w.CapHit())

	// Old kqueue behaviour: delta ≈ fileCount + dirCount (thousands).
	// Bound: delta must be far below half the file count and under 5k.
	if delta >= fileCount/2 {
		t.Fatalf("FD delta %d grows with corpus (files=%d); kqueue-style regression", delta, fileCount)
	}
	if delta >= 5000 {
		t.Fatalf("FD delta %d exceeds 5k watch budget for test corpus", delta)
	}
	// Darwin must use FSEvents.
	if runtime.GOOS == "darwin" && w.Backend() != "fsevents" {
		t.Fatalf("darwin backend=%s want fsevents", w.Backend())
	}
}

// TestCapFailSoft ensures fsnotify backends stop expanding at MaxDirWatches.
// On Darwin (FSEvents) CapHit stays false; we still assert New succeeds.
func TestCapFailSoft(t *testing.T) {
	root := t.TempDir()
	// More dirs than a tiny cap.
	for i := 0; i < 30; i++ {
		if err := os.MkdirAll(filepath.Join(root, "d", itoa(i), "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const capN = 5
	w, err := New(Config{
		Roots:         []string{root},
		MaxDirWatches: capN,
		Latency:       20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if runtime.GOOS == "darwin" {
		// FSEvents does not install per-dir watches; cap does not apply.
		if w.Backend() != "fsevents" {
			t.Fatalf("backend %s", w.Backend())
		}
		return
	}
	if !w.CapHit() {
		t.Fatal("expected CapHit when dirs exceed MaxDirWatches")
	}
	if w.DirWatchCount() > capN {
		t.Fatalf("dir watches %d > cap %d", w.DirWatchCount(), capN)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
