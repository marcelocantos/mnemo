// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the two defects of the 20 Oct 2026 nightly run: the window
// produced two snapshots (03:29 and 04:01, the second after a daemon
// restart at 03:00 had already been followed by one success), and the
// keep=1 retention of the second deleted the manual snapshot taken the
// evening before.

func plant(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("generic"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func remaining(t *testing.T, dir string) []string {
	t.Helper()
	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	return namesOf(list)
}

func sameNames(got, want []string) bool { return strings.Join(got, ",") == strings.Join(want, ",") }

// --- retention -------------------------------------------------------

// The reported case: one manual snapshot older than the dailies, keep=1.
// The shared pool deleted it; the manual pool keeps it.
func TestRetentionKeepOneSparesManual(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir,
		"mnemo-pre-migration-20261017T010000Z.db.zst",
		"mnemo-daily-20261018T163000Z.db.zst",
		"mnemo-manual-20261018T232704Z.db.zst",
		"mnemo-daily-20261019T162900Z.db.zst",
	)
	removed, err := GCOldest(dir, Retention{Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %d, want 2 (the older daily and the pre-migration)", len(removed))
	}
	want := []string{"mnemo-daily-20261019T162900Z.db.zst", "mnemo-manual-20261018T232704Z.db.zst"}
	if got := remaining(t, dir); !sameNames(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// The manual pool is bounded, by its own count, independently of keep.
func TestRetentionManualPoolIsBoundedSeparately(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir,
		"mnemo-manual-20261001T000000Z.db.zst",
		"mnemo-daily-20261002T000000Z.db.zst",
		"mnemo-manual-20261003T000000Z.db.gz",
		"mnemo-daily-20261004T000000Z.db.zst",
		"mnemo-manual-20261005T000000Z.db.zst",
		"mnemo-daily-20261006T000000Z.db.zst",
		"mnemo-manual-20261007T000000Z.db.zst",
	)
	if _, err := GCOldest(dir, Retention{Keep: 2, KeepManual: 3}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mnemo-manual-20261007T000000Z.db.zst",
		"mnemo-daily-20261006T000000Z.db.zst",
		"mnemo-manual-20261005T000000Z.db.zst",
		"mnemo-daily-20261004T000000Z.db.zst",
		"mnemo-manual-20261003T000000Z.db.gz",
	}
	if got := remaining(t, dir); !sameNames(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// An unset keep_manual means DefaultKeepManual, never "keep none".
func TestRetentionManualDefault(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir,
		"mnemo-manual-20261001T000000Z.db.zst",
		"mnemo-manual-20261002T000000Z.db.zst",
		"mnemo-manual-20261003T000000Z.db.zst",
	)
	for _, km := range []int{0, -1} {
		if _, err := GCOldest(dir, Retention{Keep: 1, KeepManual: km}); err != nil {
			t.Fatal(err)
		}
		if got := remaining(t, dir); len(got) != DefaultKeepManual {
			t.Errorf("keep_manual=%d kept %v, want the newest %d", km, got, DefaultKeepManual)
		}
	}
}

// Manual snapshots do not occupy daily slots either: with keep=1 and
// manual snapshots newer than every daily, the newest daily survives.
func TestRetentionManualDoesNotEvictDaily(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir,
		"mnemo-daily-20261001T000000Z.db.zst",
		"mnemo-manual-20261002T000000Z.db.zst",
		"mnemo-manual-20261003T000000Z.db.zst",
	)
	if _, err := GCOldest(dir, Retention{Keep: 1}); err != nil {
		t.Fatal(err)
	}
	if got := remaining(t, dir); len(got) != 3 {
		t.Errorf("kept %v, want all three", got)
	}
}

// The cycle housekeeping (startup and every window) uses the same pools.
func TestHousekeepSparesManual(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir,
		"mnemo-manual-20261018T232704Z.db.zst",
		"mnemo-daily-20261019T162900Z.db.zst",
		"mnemo-daily-20261019T170100Z.db.zst",
	)
	w := &Worker{dir: dir, retention: Retention{Keep: 1}.normalized()}
	w.housekeep()
	want := []string{"mnemo-daily-20261019T170100Z.db.zst", "mnemo-manual-20261018T232704Z.db.zst"}
	if got := remaining(t, dir); !sameNames(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}
}

// End to end through a real snapshot: a nightly run at keep=1 leaves the
// manual snapshot alone and removes only the older daily.
func TestDailySnapshotSparesManual(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	man, _, err := CreateAndRetain(src, dir, TagManual, Retention{Keep: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plant(t, dir, "mnemo-daily-20260101T000000Z.db.zst")
	daily, removed, err := CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || filepath.Base(removed[0]) != "mnemo-daily-20260101T000000Z.db.zst" {
		t.Errorf("removed %v, want only the old daily", removed)
	}
	for _, p := range []string{man.Path, daily.Path} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s missing after the daily run: %v", filepath.Base(p), err)
		}
	}
}

// The free-space precheck still sees manual snapshots: they are no
// longer reclaimed by the nightly run, so space they hold must make a
// run refuse up front (never delete them to make room), and the refusal
// must name them as what is holding the space.
func TestSpacePrecheckAccountsForManualSnapshots(t *testing.T) {
	src := seedDB(t, 50)
	dir := t.TempDir()
	m1, _, err := CreateAndRetain(src, dir, TagManual, Retention{Keep: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m2, _, err := CreateAndRetain(src, dir, TagManual, Retention{Keep: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The volume is full of what is there: no room for another snapshot.
	freeBytesFn = func(string) (int64, error) { return 1 << 20, nil }
	defer func() { freeBytesFn = freeBytes }()

	_, _, err = CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, nil)
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	if !strings.Contains(err.Error(), "2 manual") || !strings.Contains(err.Error(), "keep_manual") {
		t.Errorf("refusal does not account for the manual snapshots: %v", err)
	}
	for _, p := range []string{m1.Path, m2.Path} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("manual snapshot %s deleted to make room: %v", filepath.Base(p), err)
		}
	}
	if u, _ := Usage(dir); u.ManualCount != 2 || u.ManualBytes == 0 || u.ManualBytes != u.RetainedBytes {
		t.Errorf("usage = %+v, want both snapshots counted as manual", u)
	}
}

// --- one snapshot per window -----------------------------------------

func at(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.Local) }

func inWindow(t *testing.T, got time.Time, day int) {
	t.Helper()
	if got.Before(at(day, 3, 0)) || !got.Before(at(day, 4, 0)) {
		t.Errorf("scheduled %v, want inside 03:00-04:00 on day %d", got, day)
	}
}

// The bug: Run re-entered scheduleNext inside the window after a
// success and got the remainder of the same window.
func TestScheduleNextAfterSuccessInsideWindowIsTomorrow(t *testing.T) {
	w := newWorker(t, t.TempDir(), "x.db", &fakeActivity{})
	w.lastSuccess = at(20, 3, 29)
	for _, now := range []time.Time{at(20, 3, 29).Add(90 * time.Second), at(20, 3, 59), at(20, 4, 1)} {
		inWindow(t, w.scheduleNext(now), 21)
	}
}

// A success that ran past window_end while waiting for quiescence still
// spends the window.
func TestScheduleNextLateSuccessSpendsWindow(t *testing.T) {
	w := newWorker(t, t.TempDir(), "x.db", &fakeActivity{})
	w.lastSuccess = at(20, 4, 30)
	inWindow(t, w.scheduleNext(at(20, 4, 45)), 21)
}

// Yesterday's success says nothing about today's window.
func TestScheduleNextYesterdaysSuccessDoesNotSkipToday(t *testing.T) {
	w := newWorker(t, t.TempDir(), "x.db", &fakeActivity{})
	w.lastSuccess = at(19, 3, 29)
	inWindow(t, w.scheduleNext(at(20, 1, 0)), 20)
	next := w.scheduleNext(at(20, 3, 15))
	if next.Before(at(20, 3, 15)) || !next.Before(at(20, 4, 0)) {
		t.Errorf("scheduled %v, want the rest of today's window", next)
	}
}

// Restart inside the window after a success: the new process reads the
// success from StatusFile and does not run again tonight.
func TestRestartInsideWindowAfterSuccessDoesNotRerun(t *testing.T) {
	dir := t.TempDir()
	if err := RecordAttempt(dir, Attempt{At: at(20, 3, 29), Tag: TagDaily, OK: true,
		Path: filepath.Join(dir, "mnemo-daily-20261019T162900Z.db.zst")}); err != nil {
		t.Fatal(err)
	}
	w := newWorker(t, dir, "x.db", &fakeActivity{})
	w.loadLastSuccess()
	if !w.lastSuccess.Equal(at(20, 3, 29)) {
		t.Fatalf("loaded lastSuccess %v, want 03:29", w.lastSuccess)
	}
	inWindow(t, w.scheduleNext(at(20, 3, 45)), 21)
}

// A status file written before last_daily_success existed still counts
// when it records a daily success, so the fix works from the first boot
// after upgrading.
func TestRestartReadsLegacyStatusFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"at":"` + at(20, 3, 29).Format(time.RFC3339Nano) + `","tag":"daily","ok":true}`
	if err := os.WriteFile(filepath.Join(dir, StatusFile), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	w := newWorker(t, dir, "x.db", &fakeActivity{})
	w.loadLastSuccess()
	inWindow(t, w.scheduleNext(at(20, 3, 45)), 21)
}

// The record of tonight's daily success survives later attempts that
// overwrite the status file: a manual snapshot and a failed run.
func TestRecordAttemptCarriesLastDailySuccess(t *testing.T) {
	dir := t.TempDir()
	steps := []Attempt{
		{At: at(20, 3, 29), Tag: TagDaily, OK: true},
		{At: at(20, 3, 40), Tag: TagManual, OK: true},
		{At: at(20, 3, 50), Tag: TagManual, Error: "boom"},
	}
	for _, a := range steps {
		if err := RecordAttempt(dir, a); err != nil {
			t.Fatal(err)
		}
		got, _, _ := LastAttempt(dir)
		if !got.LastDailySuccess.Equal(at(20, 3, 29)) {
			t.Fatalf("after %+v: last_daily_success = %v, want 03:29", a, got.LastDailySuccess)
		}
	}
	w := newWorker(t, dir, "x.db", &fakeActivity{})
	w.loadLastSuccess()
	inWindow(t, w.scheduleNext(at(20, 3, 55)), 21)
}

// A manual snapshot inside the window does not stand in for the daily:
// they are different pools.
func TestManualSuccessDoesNotSpendWindow(t *testing.T) {
	dir := t.TempDir()
	if err := RecordAttempt(dir, Attempt{At: at(20, 3, 10), Tag: TagManual, OK: true}); err != nil {
		t.Fatal(err)
	}
	w := newWorker(t, dir, "x.db", &fakeActivity{})
	w.loadLastSuccess()
	next := w.scheduleNext(at(20, 3, 15))
	if next.Before(at(20, 3, 15)) || !next.Before(at(20, 4, 0)) {
		t.Errorf("scheduled %v, want the rest of today's window", next)
	}
}

// A failure inside the window keeps the existing retry: the rest of the
// window is still eligible, before and after a restart.
func TestFailureInsideWindowStillRetries(t *testing.T) {
	dir := t.TempDir()
	for _, a := range []Attempt{
		{At: at(19, 3, 20), Tag: TagDaily, OK: true},
		{At: at(20, 3, 29), Tag: TagDaily, Error: "vacuum into: disk I/O error"},
	} {
		if err := RecordAttempt(dir, a); err != nil {
			t.Fatal(err)
		}
	}
	w := newWorker(t, dir, "x.db", &fakeActivity{})
	w.loadLastSuccess()
	next := w.scheduleNext(at(20, 3, 31))
	if next.Before(at(20, 3, 31)) || !next.Before(at(20, 4, 0)) {
		t.Errorf("scheduled %v after a failure, want a retry in the rest of today's window", next)
	}
}

// --- the loop itself, on a compressed window ---------------------------

// compressedWindow returns a Config whose daily window opens shortly
// after now and lasts span, for driving Run on the real clock.
func compressedWindow(t *testing.T, src, dir string, lead, span time.Duration) (Config, time.Time) {
	t.Helper()
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := now.Sub(midnight) + lead
	if start+span >= 24*time.Hour {
		t.Skip("compressed window would cross midnight")
	}
	return Config{
		SrcPath:      src,
		Dir:          dir,
		Keep:         5, // roomy, so a second snapshot would be visible
		WindowStart:  start,
		WindowEnd:    start + span,
		Activity:     &fakeActivity{},
		PollInterval: 5 * time.Millisecond,
		Seed:         7,
	}, midnight.Add(start + span)
}

func runFor(t *testing.T, cfg Config, ctx context.Context) <-chan struct{} {
	t.Helper()
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	return done
}

func dailies(t *testing.T, dir string) int {
	n := 0
	list, _ := List(dir)
	for _, b := range list {
		if b.Tag == TagDaily {
			n++
		}
	}
	return n
}

// One snapshot per window, through Run, on the real clock. Before the
// fix the success rescheduled into the rest of the same window and a
// second snapshot followed.
func TestRunTakesOneSnapshotPerWindow(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	cfg, end := compressedWindow(t, src, dir, 100*time.Millisecond, 2*time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(500*time.Millisecond))
	defer cancel()
	<-runFor(t, cfg, ctx)
	if n := dailies(t, dir); n != 1 {
		t.Fatalf("window produced %d daily snapshots, want 1: %v", n, remaining(t, dir))
	}
}

// Restart inside the window after a success: the second process must
// not take another.
func TestRunRestartInsideWindowDoesNotRerun(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	cfg, end := compressedWindow(t, src, dir, 100*time.Millisecond, 3*time.Second)

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := runFor(t, cfg, ctx1)
	for dailies(t, dir) == 0 && time.Now().Before(end) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel1()
	<-done1
	if n := dailies(t, dir); n != 1 {
		t.Fatalf("first process produced %d snapshots, want 1", n)
	}
	if !time.Now().Before(end) {
		t.Skip("first snapshot landed at the very end of the window; restart case not exercised")
	}

	ctx2, cancel2 := context.WithDeadline(context.Background(), end.Add(500*time.Millisecond))
	defer cancel2()
	cfg.Seed = 8
	<-runFor(t, cfg, ctx2)
	if n := dailies(t, dir); n != 1 {
		t.Fatalf("restart inside the window produced %d snapshots in total, want 1: %v",
			n, remaining(t, dir))
	}
}

// A failure inside the window is retried inside the same window.
func TestRunRetriesFailureInsideWindow(t *testing.T) {
	seeded := seedDB(t, 20)
	src := filepath.Join(t.TempDir(), "src.db")
	dir := t.TempDir()
	cfg, end := compressedWindow(t, src, dir, 100*time.Millisecond, 3*time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(500*time.Millisecond))
	defer cancel()
	done := runFor(t, cfg, ctx)

	// The source is missing, so the first attempt fails. Restore it as
	// soon as the failure is on record.
	sawFailure := false
	for time.Now().Before(end) {
		if a, ok, _ := LastAttempt(dir); ok && !a.OK {
			sawFailure = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !sawFailure {
		t.Fatal("no failed attempt recorded in the window")
	}
	if err := os.Rename(seeded, src); err != nil {
		t.Fatal(err)
	}
	<-done
	if n := dailies(t, dir); n != 1 {
		t.Fatalf("after a failure the window produced %d snapshots, want 1 (the retry)", n)
	}
}
