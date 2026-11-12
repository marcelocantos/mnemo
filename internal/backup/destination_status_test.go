// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T199: where the record of an attempt lives when snapshots go to a
// custom directory, and what the worker does with it.

// farWindow returns a Config whose daily window is hours away from now,
// so nothing these tests observe can be the scheduled run: a snapshot
// that appears is a catch-up.
func farWindow(t *testing.T, src, dir, statusDir string) Config {
	t.Helper()
	start := 20 * time.Hour
	if time.Now().Hour() >= 12 {
		start = time.Hour
	}
	return Config{
		SrcPath:      src,
		Dir:          dir,
		StatusDir:    statusDir,
		Keep:         5,
		WindowStart:  start,
		WindowEnd:    start + time.Hour,
		Activity:     &fakeActivity{},
		PollInterval: 5 * time.Millisecond,
		Seed:         7,
	}
}

// awayErr is the error an attempt records for an absent dir.
func awayErr(dir string) error {
	return fmt.Errorf("%w: %s", ErrDestinationUnavailable, dir)
}

// entries lists the names in dir, or nil when it does not exist.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// awaitAttempt polls statusDir until its record satisfies ok.
func awaitAttempt(t *testing.T, statusDir string, ok func(Attempt) bool) Attempt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, have, err := LastAttempt(statusDir)
		if err == nil && have && ok(a) {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching attempt recorded in %s; last = %+v (have=%v err=%v)", statusDir, a, have, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An attempt made while the destination is absent is a recorded skip that
// names the destination, and it writes nothing but that record.
func TestAbsentDestinationRecordsSkipBesideDatabase(t *testing.T) {
	src := seedDB(t, 20)
	dir, _ := absentDest(t)
	statusDir := filepath.Join(t.TempDir(), "backups") // not created: the record must create it
	cfg, end := compressedWindow(t, src, dir, 100*time.Millisecond, 300*time.Millisecond)
	cfg.StatusDir = statusDir
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(200*time.Millisecond))
	defer cancel()
	<-runFor(t, cfg, ctx)

	a, ok, err := LastAttempt(statusDir)
	if err != nil || !ok {
		t.Fatalf("no attempt recorded beside the database: ok=%v err=%v", ok, err)
	}
	if a.OK || !a.Skipped || !a.Unavailable || a.Tag != TagDaily {
		t.Errorf("attempt = %+v, want a skipped, unavailable daily attempt", a)
	}
	if !strings.Contains(a.Error, dir) || !strings.Contains(a.Error, ErrDestinationUnavailable.Error()) {
		t.Errorf("error %q does not name the missing destination %s", a.Error, dir)
	}
	// Nothing lands on the database's volume except the record itself:
	// no snapshot, no VACUUM scratch.
	if got := entries(t, statusDir); len(got) != 1 || got[0] != StatusFile {
		t.Errorf("status directory holds %v, want only %s", got, StatusFile)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("%s was created while the volume is away", dir)
	}
}

// The debt survives a restart: a daemon that comes up after a skipped
// night, with the destination back, takes the owed snapshot without
// waiting for its window.
func TestOwedSnapshotSurvivesRestart(t *testing.T) {
	src := seedDB(t, 20)
	dir, statusDir := t.TempDir(), t.TempDir()
	RecordUnavailable(statusDir, TagDaily, awayErr(dir))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runFor(t, farWindow(t, src, dir, statusDir), ctx)

	if n := awaitDailies(t, dir, 1, 5*time.Second); n != 1 {
		t.Fatalf("%d daily snapshots after restart with a snapshot owed, want 1", n)
	}
	a := awaitAttempt(t, statusDir, func(a Attempt) bool { return a.OK })
	if a.Unavailable || a.Tag != TagDaily {
		t.Errorf("attempt after the catch-up = %+v, want a successful daily one", a)
	}
	// The record stays beside the database; the destination holds only
	// the snapshot.
	if _, err := os.Stat(filepath.Join(dir, StatusFile)); err == nil {
		t.Errorf("%s written into the destination, want it only in the status directory", StatusFile)
	}
	cancel()
	<-done
}

// The destination returns and a snapshot has already been taken: no
// second run.
func TestDestinationReturnsWithSnapshotAlreadyTaken(t *testing.T) {
	// Owed by this process, then paid by a manual backup_now before the
	// worker's next look at the destination.
	t.Run("manual snapshot pays the debt", func(t *testing.T) {
		src := seedDB(t, 20)
		dir, attach := absentDest(t)
		statusDir := t.TempDir()
		RecordUnavailable(statusDir, TagDaily, awayErr(dir))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := runFor(t, farWindow(t, src, dir, statusDir), ctx)
		time.Sleep(50 * time.Millisecond) // the worker is polling for the destination

		if err := RecordAttempt(statusDir, Attempt{At: time.Now(), Tag: TagManual, OK: true}); err != nil {
			t.Fatal(err)
		}
		attach()
		time.Sleep(300 * time.Millisecond)
		if n := dailies(t, dir); n != 0 {
			t.Fatalf("%d daily snapshots after a manual one had already protected the store, want 0", n)
		}
		cancel()
		<-done
	})

	// Nothing owed: last night's snapshot succeeded before the volume
	// was unplugged, and the daemon restarted while it was away.
	t.Run("nothing owed at startup", func(t *testing.T) {
		src := seedDB(t, 20)
		dir, attach := absentDest(t)
		statusDir := t.TempDir()
		at := time.Now().Add(-time.Hour)
		if err := RecordAttempt(statusDir, Attempt{At: at, Tag: TagDaily, OK: true}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := runFor(t, farWindow(t, src, dir, statusDir), ctx)
		time.Sleep(50 * time.Millisecond)
		attach()
		time.Sleep(300 * time.Millisecond)
		if n := dailies(t, dir); n != 0 {
			t.Fatalf("%d daily snapshots with nothing owed, want 0", n)
		}
		cancel()
		<-done
	})
}

// A returned destination that then fails for another reason is not
// retried every poll interval: the debt is cleared and the usual rule
// (rest of the window, then tomorrow) applies.
func TestOtherFailureClearsDebt(t *testing.T) {
	dir, statusDir := t.TempDir(), t.TempDir()
	missingSrc := filepath.Join(t.TempDir(), "no-such.db")
	w, err := NewWorker(farWindow(t, missingSrc, dir, statusDir))
	if err != nil {
		t.Fatal(err)
	}
	w.owedSince = time.Now().Add(-time.Hour)
	if w.attemptBackup(context.Background()) {
		t.Fatal("attemptBackup succeeded without a source database")
	}
	if !w.owedSince.IsZero() {
		t.Errorf("owedSince = %v after a non-destination failure, want it cleared", w.owedSince)
	}
}

// Every snapshot path reports an absent destination the same way: the
// sentinel error, a recorded skip carrying the caller's tag, and nothing
// written.
func TestCreateAndRetainAbsentDestination(t *testing.T) {
	src := seedDB(t, 20)
	dir, _ := absentDest(t)
	statusDir := t.TempDir()
	_, _, err := CreateAndRetain(src, dir, TagManual, Retention{Keep: 1}, &BackupArgs{StatusDir: statusDir})
	if !errors.Is(err, ErrDestinationUnavailable) {
		t.Fatalf("err = %v, want ErrDestinationUnavailable", err)
	}
	a, ok, rerr := LastAttempt(statusDir)
	if rerr != nil || !ok || !a.Unavailable || !a.Skipped || a.Tag != TagManual {
		t.Fatalf("attempt = %+v ok=%v err=%v, want an unavailable manual skip", a, ok, rerr)
	}
	if got := entries(t, statusDir); len(got) != 1 {
		t.Errorf("status directory holds %v, want only the record", got)
	}
}

// With a status directory, a successful snapshot's record goes there and
// not beside the snapshot.
func TestSnapshotRecordsInStatusDir(t *testing.T) {
	src := seedDB(t, 20)
	dir, statusDir := t.TempDir(), t.TempDir()
	res, _, err := CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, &BackupArgs{StatusDir: statusDir})
	if err != nil {
		t.Fatal(err)
	}
	a, ok, _ := LastAttempt(statusDir)
	if !ok || !a.OK || a.Path != res.Path {
		t.Fatalf("status directory record = %+v ok=%v, want the successful snapshot %s", a, ok, res.Path)
	}
	if _, ok, _ := LastAttempt(dir); ok {
		t.Errorf("a record was also written into the destination")
	}
}

// The first start after upgrading from a release that kept the record
// beside the snapshots: the newer record there is adopted, so last
// night's success is neither forgotten by the worker nor missing from
// /health.
func TestAdoptLegacyStatus(t *testing.T) {
	dir, statusDir := t.TempDir(), t.TempDir()
	stale := time.Now().Add(-50 * time.Hour)
	fresh := time.Now().Add(-2 * time.Hour)
	// Beside the database: what the release before the custom directory
	// left behind. In the destination: last night's success.
	for d, a := range map[string]Attempt{
		statusDir: {At: stale, Tag: TagDaily, OK: true},
		dir:       {At: fresh, Tag: TagDaily, OK: true},
	} {
		if err := RecordAttempt(d, a); err != nil {
			t.Fatal(err)
		}
	}
	w, err := NewWorker(farWindow(t, "x.db", dir, statusDir))
	if err != nil {
		t.Fatal(err)
	}
	w.adoptLegacyStatus()
	w.loadLastSuccess()
	if !w.lastSuccess.Equal(fresh) {
		t.Errorf("lastSuccess = %v, want the destination's newer success %v", w.lastSuccess, fresh)
	}
	a, ok, _ := LastAttempt(statusDir)
	if !ok || !a.At.Equal(fresh) {
		t.Errorf("status directory record = %+v, want the adopted one at %v", a, fresh)
	}
	if _, err := os.Stat(filepath.Join(dir, StatusFile)); err == nil {
		t.Errorf("the old record was left in the destination")
	}

	// An older record in the destination must not displace a newer one
	// beside the database, though its success time still counts.
	dir2, statusDir2 := t.TempDir(), t.TempDir()
	skip := Attempt{At: time.Now().Add(-time.Minute), Tag: TagDaily, Skipped: true, Unavailable: true, Error: "away"}
	if err := RecordAttempt(statusDir2, skip); err != nil {
		t.Fatal(err)
	}
	if err := RecordAttempt(dir2, Attempt{At: fresh, Tag: TagDaily, OK: true}); err != nil {
		t.Fatal(err)
	}
	w2, err := NewWorker(farWindow(t, "x.db", dir2, statusDir2))
	if err != nil {
		t.Fatal(err)
	}
	w2.adoptLegacyStatus()
	w2.loadLastSuccess()
	if w2.owedSince.IsZero() || !w2.lastSuccess.Equal(fresh) {
		t.Errorf("owedSince=%v lastSuccess=%v, want the newer skip kept and the older success remembered",
			w2.owedSince, w2.lastSuccess)
	}
}

// The default layout is unchanged: no status directory means the record
// sits beside the snapshots, and a missing default directory is empty,
// not away.
func TestDefaultLayoutUnchanged(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	if _, _, err := CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LastAttempt(dir); !ok {
		t.Error("no record beside the snapshot in the default layout")
	}
	missing := filepath.Join(t.TempDir(), "backups")
	if err := Away(missing, missing); err != nil {
		t.Errorf("a missing default directory reported as away: %v", err)
	}
	if err := Away(missing, t.TempDir()); !errors.Is(err, ErrDestinationUnavailable) {
		t.Errorf("a missing custom directory: err = %v, want ErrDestinationUnavailable", err)
	}
}

// /health tells a destination that was away apart from a full disk: the
// remedy is to reconnect it, not to free space.
func TestFreshnessUnavailableSkip(t *testing.T) {
	now := time.Now()
	last := &Attempt{At: now.Add(-time.Hour), Tag: TagDaily, Skipped: true, Unavailable: true,
		Error:       ErrDestinationUnavailable.Error() + ": /Volumes/Generic/backups",
		LastSuccess: now.Add(-25 * time.Hour)}
	healthy, detail, remediation := Freshness(nil, last, now, MaxAge)
	if healthy {
		t.Fatal("a skipped night reported healthy")
	}
	if !strings.Contains(detail, "not available") || !strings.Contains(detail, "last successful backup") {
		t.Errorf("detail = %q, want the missing destination and the last success", detail)
	}
	if !strings.Contains(remediation, "reconnect") || strings.Contains(remediation, "free space") {
		t.Errorf("remediation = %q, want reconnecting the volume, not freeing space", remediation)
	}
}
