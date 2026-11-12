// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 🎯T199: a backup destination on a volume that is not attached.
//
// The tests in this file use only the API that existed before the fix,
// so they run — and fail, on behaviour — against the code they guard
// against: a worker that exited at startup, and a missed night that was
// never made up.

// absentDest returns a backup directory that cannot be created, and a
// function that makes it creatable. The directory sits under a regular
// file, the portable stand-in for a mount point with no volume behind
// it: creating anything below it is refused, as it is under /Volumes on
// macOS or on a missing drive letter on Windows.
func absentDest(t *testing.T) (dir string, attach func()) {
	t.Helper()
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.WriteFile(volume, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	attach = func() {
		t.Helper()
		if err := os.Remove(volume); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(volume, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(volume, "mnemo", "backups"), attach
}

// awaitDailies polls until dir holds want daily snapshots or the wait
// runs out, and reports the count it last saw.
func awaitDailies(t *testing.T, dir string, want int, wait time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		n := dailies(t, dir)
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A daemon started while the destination is absent keeps its worker, and
// the worker uses the destination once it is there. Run used to return
// at startup when it could not create the directory, so nothing was
// backed up again until the daemon was restarted.
func TestRunSurvivesDestinationAbsentAtStartup(t *testing.T) {
	src := seedDB(t, 20)
	dir, attach := absentDest(t)
	cfg, end := compressedWindow(t, src, dir, 400*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(500*time.Millisecond))
	defer cancel()
	done := runFor(t, cfg, ctx)

	// The volume comes back before the window opens.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("worker exited because the destination was absent at startup")
	default:
	}
	attach()

	<-done
	if n := dailies(t, dir); n != 1 {
		t.Fatalf("window produced %d daily snapshots after the destination returned, want 1", n)
	}
}

// The destination is absent for the whole window and returns afterwards.
// The snapshot is owed: it is taken when the destination comes back, not
// at tomorrow's window.
func TestRunTakesOwedSnapshotWhenDestinationReturns(t *testing.T) {
	src := seedDB(t, 20)
	dir, attach := absentDest(t)
	cfg, end := compressedWindow(t, src, dir, 100*time.Millisecond, 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runFor(t, cfg, ctx)

	// Let the window pass with the volume away. The next window is
	// tomorrow, so any snapshot from here on is the catch-up.
	time.Sleep(time.Until(end) + 100*time.Millisecond)
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("%s exists while the volume is away", dir)
	}
	attach()

	if n := awaitDailies(t, dir, 1, 5*time.Second); n != 1 {
		t.Fatalf("%d daily snapshots after the destination returned, want the owed one", n)
	}
	// Exactly one: the debt is paid once.
	time.Sleep(200 * time.Millisecond)
	if n := dailies(t, dir); n != 1 {
		t.Fatalf("%d daily snapshots, want 1: the owed snapshot was taken more than once", n)
	}
	cancel()
	<-done
}
