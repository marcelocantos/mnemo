// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 🎯T200: a backup destination whose calls never return.
//
// hangDest makes every bounded call on dir block until release is
// called, standing in for a volume macOS holds while it waits on a
// permission decision. The bounded waits are shortened so the tests run
// in well under a second each.
func hangDest(t *testing.T, dir string) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	key := filepath.Clean(dir)
	prevHook, prevAnswer, prevData := hangHook, answerTimeout, dataTimeout
	hangHook = func(d, op string) {
		if d == key {
			<-gate
		}
	}
	answerTimeout, dataTimeout = 150*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() {
		release()
		// Let the stranded calls drain before the next test reuses the
		// pending table.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, stuck := NotAnswering(dir); !stuck {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		hangHook, answerTimeout, dataTimeout = prevHook, prevAnswer, prevData
	})
	return release
}

// within fails the test if fn has not returned after d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: the destination blocked it", what, d)
	}
}

// A call that does not return is abandoned after the bound, reported as
// ErrDestinationUnavailable with a reason, and further calls on the same
// directory are refused at once instead of stranding a thread each. Once
// the stuck call returns, the directory is usable again.
func TestAnswerBoundsAStuckCall(t *testing.T) {
	dir := t.TempDir()
	release := hangDest(t, dir)

	start := time.Now()
	_, err := readDir(dir)
	if !errors.Is(err, ErrDestinationUnavailable) {
		t.Fatalf("readDir on a hanging destination: err = %v, want ErrDestinationUnavailable", err)
	}
	if !strings.Contains(err.Error(), "has not answered") || !strings.Contains(err.Error(), "permission") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("readDir took %s, want about the bound", el)
	}
	if _, _, stuck := NotAnswering(dir); !stuck {
		t.Error("the directory is not marked as not answering")
	}

	// Refused immediately: no second thread parked in the kernel.
	start = time.Now()
	if err := probe(dir); !errors.Is(err, ErrDestinationUnavailable) {
		t.Fatalf("probe while stuck: %v", err)
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Errorf("a call on a stuck directory waited %s; it should be refused at once", el)
	}

	release()
	deadline := time.Now().Add(time.Second)
	for {
		if _, _, stuck := NotAnswering(dir); !stuck {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the directory stayed marked after its call returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
	hangHook = nil
	if err := probe(dir); err != nil {
		t.Fatalf("probe after the call returned: %v", err)
	}
}

// Every read path a caller can reach on a custom backup.dir returns
// within the bound: the startup orphan sweep, worker housekeeping,
// backup_status (Away, List, Usage) and the health checks.
func TestHangingDestinationNeverStalls(t *testing.T) {
	dir := t.TempDir()
	statusDir := t.TempDir()
	hangDest(t, dir)

	within(t, 2*time.Second, "SweepOrphans", func() {
		if _, err := SweepOrphans(dir, DefaultOrphanAge); !errors.Is(err, ErrDestinationUnavailable) {
			t.Errorf("SweepOrphans: %v", err)
		}
	})
	within(t, 2*time.Second, "Away", func() {
		if err := Away(dir, statusDir); !errors.Is(err, ErrDestinationUnavailable) {
			t.Errorf("Away: %v", err)
		}
	})
	within(t, 2*time.Second, "List", func() {
		if _, err := List(dir); !errors.Is(err, ErrDestinationUnavailable) {
			t.Errorf("List: %v", err)
		}
	})
	within(t, 2*time.Second, "Usage", func() {
		if _, err := Usage(dir); !errors.Is(err, ErrDestinationUnavailable) {
			t.Errorf("Usage: %v", err)
		}
	})
	within(t, 2*time.Second, "EnsureDir", func() {
		if err := EnsureDir(dir); !errors.Is(err, ErrDestinationUnavailable) {
			t.Errorf("EnsureDir: %v", err)
		}
	})
	w, err := NewWorker(Config{SrcPath: "x", Dir: dir, StatusDir: statusDir, Keep: 1,
		WindowStart: time.Hour, WindowEnd: 2 * time.Hour, Activity: &fakeActivity{}})
	if err != nil {
		t.Fatal(err)
	}
	within(t, 2*time.Second, "worker housekeeping", func() {
		w.adoptLegacyStatus()
		w.housekeep()
	})
}

// A snapshot with no staging directory and a hanging destination is a
// recorded skip, returned within the bound, and nothing is written.
func TestHangingDestinationSnapshotSkipped(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	statusDir := t.TempDir()
	hangDest(t, dir)

	within(t, 3*time.Second, "CreateAndRetain", func() {
		_, _, err := CreateAndRetain(src, dir, TagManual, Retention{Keep: 1}, &BackupArgs{StatusDir: statusDir})
		if !errors.Is(err, ErrDestinationUnavailable) {
			t.Errorf("CreateAndRetain: %v", err)
		}
	})
	a, ok, err := LastAttempt(statusDir)
	if err != nil || !ok || !a.Skipped || !a.Unavailable || !strings.Contains(a.Error, "has not answered") {
		t.Fatalf("recorded attempt = %+v ok=%v err=%v, want an unavailable skip naming the cause", a, ok, err)
	}
	if entries, _ := os.ReadDir(statusDir); len(entries) != 1 {
		t.Errorf("status dir holds %d entries, want only %s", len(entries), StatusFile)
	}
}

// 🎯T201 with 🎯T200: with a staging directory, a hanging destination
// costs a few seconds and the snapshot is staged rather than lost.
func TestHangingDestinationSnapshotStaged(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	statusDir := t.TempDir()
	staging := filepath.Join(statusDir, StagingSubdir)
	hangDest(t, dir)

	var res Result
	within(t, 3*time.Second, "CreateAndRetain", func() {
		var err error
		res, _, err = CreateAndRetain(src, dir, TagManual, Retention{Keep: 1},
			&BackupArgs{StatusDir: statusDir, StagingDir: staging})
		if err != nil {
			t.Errorf("CreateAndRetain: %v", err)
		}
	})
	if filepath.Dir(res.Path) != staging {
		t.Fatalf("snapshot written to %s, want staging %s", res.Path, staging)
	}
	a, _, _ := LastAttempt(statusDir)
	if !a.OK || !a.Staged || a.Skipped || !strings.Contains(a.StagedBecause, "has not answered") {
		t.Fatalf("attempt = %+v, want a staged success naming the hang", a)
	}
}
