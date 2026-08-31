// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeActivity is an ActivityProvider backed by an atomic Int64
// (unix-nano) for tests. matches the production shape (store.Store
// uses the same pattern).
type fakeActivity struct{ ns atomic.Int64 }

func (f *fakeActivity) Set(t time.Time) { f.ns.Store(t.UnixNano()) }
func (f *fakeActivity) LastWriteAt() time.Time {
	v := f.ns.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v)
}

func newWorker(t *testing.T, dir string, src string, act ActivityProvider) *Worker {
	t.Helper()
	w, err := NewWorker(Config{
		SrcPath:      src,
		Dir:          dir,
		Keep:         3,
		WindowStart:  3 * time.Hour,
		WindowEnd:    4 * time.Hour,
		Quiescence:   100 * time.Millisecond,
		Activity:     act,
		PollInterval: 10 * time.Millisecond,
		Seed:         42,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNewWorkerValidatesConfig(t *testing.T) {
	act := &fakeActivity{}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing SrcPath", Config{Dir: "/tmp/x", Keep: 1, WindowEnd: time.Hour, Activity: act}},
		{"missing Dir", Config{SrcPath: "x.db", Keep: 1, WindowEnd: time.Hour, Activity: act}},
		{"missing Activity", Config{SrcPath: "x.db", Dir: "/tmp/x", Keep: 1, WindowEnd: time.Hour}},
		{"zero Keep", Config{SrcPath: "x.db", Dir: "/tmp/x", WindowEnd: time.Hour, Activity: act}},
		{"window inverted", Config{SrcPath: "x.db", Dir: "/tmp/x", Keep: 1, WindowStart: 2 * time.Hour, WindowEnd: time.Hour, Activity: act}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewWorker(c.cfg); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestScheduleNextStaysInsideWindow(t *testing.T) {
	w := newWorker(t, t.TempDir(), "x.db", &fakeActivity{})
	// "now" is yesterday — next attempt is today's 03:00–04:00 window.
	loc := time.Local
	now := time.Date(2026, 5, 18, 0, 0, 0, 0, loc)
	next := w.scheduleNext(now)
	wantStart := time.Date(2026, 5, 18, 3, 0, 0, 0, loc)
	wantEnd := time.Date(2026, 5, 18, 4, 0, 0, 0, loc)
	if next.Before(wantStart) || !next.Before(wantEnd) {
		t.Errorf("scheduleNext = %v, want in [%v, %v)", next, wantStart, wantEnd)
	}
}

func TestScheduleNextAdvancesToTomorrowAfterWindow(t *testing.T) {
	w := newWorker(t, t.TempDir(), "x.db", &fakeActivity{})
	loc := time.Local
	now := time.Date(2026, 5, 18, 10, 0, 0, 0, loc) // after today's window
	next := w.scheduleNext(now)
	tomorrowStart := time.Date(2026, 5, 19, 3, 0, 0, 0, loc)
	tomorrowEnd := time.Date(2026, 5, 19, 4, 0, 0, 0, loc)
	if next.Before(tomorrowStart) || !next.Before(tomorrowEnd) {
		t.Errorf("scheduleNext = %v, want in [%v, %v)", next, tomorrowStart, tomorrowEnd)
	}
}

func TestQuiescenceCheck(t *testing.T) {
	act := &fakeActivity{}
	w := newWorker(t, t.TempDir(), "x.db", act)

	// No activity ever recorded → quiescent.
	if !w.isQuiescent() {
		t.Error("expected quiescent when no activity recorded")
	}

	// Activity just now → NOT quiescent.
	act.Set(time.Now())
	if w.isQuiescent() {
		t.Error("expected not-quiescent immediately after activity")
	}

	// Activity old enough → quiescent.
	act.Set(time.Now().Add(-time.Hour))
	if !w.isQuiescent() {
		t.Error("expected quiescent when last write was 1h ago")
	}
}

func TestWorkerEndToEnd(t *testing.T) {
	// Seed a real (tiny) DB, run the worker once via direct attempt,
	// verify a backup landed and is openable.
	src := seedDB(t, 10)
	dir := t.TempDir()
	act := &fakeActivity{}
	w := newWorker(t, dir, src, act)

	// Activity zero → quiescent. attemptBackup will produce a snapshot.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.attemptBackup(ctx)

	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(list))
	}
	got := list[0]
	if got.Tag != TagDaily {
		t.Errorf("Tag = %q, want %q", got.Tag, TagDaily)
	}
	if got.Size == 0 {
		t.Error("Size = 0")
	}
	if rows := countRows(t, got.Path); rows != 10 {
		t.Errorf("restored rows = %d, want 10", rows)
	}
}

func TestWorkerWaitsForQuiescenceThenFires(t *testing.T) {
	src := seedDB(t, 3)
	dir := t.TempDir()
	act := &fakeActivity{}
	act.Set(time.Now()) // start non-quiescent
	w := newWorker(t, dir, src, act)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Spawn attemptBackup; clear activity after a short delay so
	// the quiescence check passes.
	go func() {
		time.Sleep(50 * time.Millisecond)
		act.Set(time.Time{}) // back to zero = quiescent
	}()
	w.attemptBackup(ctx)

	list, _ := List(dir)
	if len(list) != 1 {
		t.Fatalf("expected 1 backup after quiescence, got %d", len(list))
	}
}

func TestGCKeepsOnlyMostRecent(t *testing.T) {
	dir := t.TempDir()
	// Create 5 fake backups with increasing timestamps.
	times := make([]time.Time, 5)
	for i := range times {
		times[i] = time.Now().Add(time.Duration(i) * time.Hour)
		path := filepath.Join(dir, Filename(TagDaily, times[i]))
		if err := os.WriteFile(path, []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := GCOldest(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %d backups, want 2", len(removed))
	}

	list, _ := List(dir)
	if len(list) != 3 {
		t.Errorf("retained %d backups, want 3", len(list))
	}
	// The three retained should be the newest three (indices 4, 3, 2).
	want := []time.Time{times[4], times[3], times[2]}
	for i, w := range want {
		if !list[i].Time.Equal(w.UTC().Truncate(time.Second)) {
			t.Errorf("list[%d].Time = %v, want %v", i, list[i].Time, w)
		}
	}
}

func TestGCNoOpWhenUnderKeep(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, Filename(TagDaily, time.Now().Add(time.Duration(i)*time.Hour)))
		os.WriteFile(path, []byte("fake"), 0o644)
	}
	removed, err := GCOldest(dir, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %d, want 0", len(removed))
	}
}

func TestGCMixedTagsSharedPool(t *testing.T) {
	dir := t.TempDir()
	// Pre-migration backup (oldest), then 4 dailies.
	pm := filepath.Join(dir, Filename(TagPreMigration, time.Now()))
	os.WriteFile(pm, []byte("fake"), 0o644)
	for i := 1; i <= 4; i++ {
		path := filepath.Join(dir, Filename(TagDaily, time.Now().Add(time.Duration(i)*time.Hour)))
		os.WriteFile(path, []byte("fake"), 0o644)
	}

	// Keep 3 — the pre-migration (oldest) and one daily should go.
	removed, err := GCOldest(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %d, want 2", len(removed))
	}

	list, _ := List(dir)
	if len(list) != 3 {
		t.Errorf("retained %d, want 3", len(list))
	}
	// The pre-migration backup is the oldest by construction, so it
	// should have been removed.
	for _, b := range list {
		if b.Tag == TagPreMigration {
			t.Error("pre-migration backup survived GC; expected oldest-removed")
		}
	}
}

func TestListSkipsNonBackupFiles(t *testing.T) {
	dir := t.TempDir()
	// Real backup.
	os.WriteFile(filepath.Join(dir, Filename(TagDaily, time.Now())), []byte("ok"), 0o644)
	// Scratch / temp files the package's own code produces.
	os.WriteFile(filepath.Join(dir, ".backup-12345.db"), []byte("scratch"), 0o644)
	os.WriteFile(filepath.Join(dir, "mnemo-daily-20260518T031742Z.db.gz.tmp"), []byte("partial"), 0o644)
	// Random user file.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644)

	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("got %d entries, want 1; names = %v", len(list), namesOf(list))
	}
}

func TestParseFilenameRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"mnemo-",
		"mnemo-daily.db.gz",
		"mnemo-daily-not-a-date.db.gz",
		"snapshot.db.gz",
		"mnemo-daily-20260518T031742Z.db", // missing .gz
	}
	for _, c := range cases {
		if _, _, ok := parseFilename(c); ok {
			t.Errorf("parseFilename(%q) returned ok=true; want false", c)
		}
	}
}

func TestListNonexistentDirIsEmpty(t *testing.T) {
	list, err := List("/nonexistent/path")
	if err != nil {
		t.Errorf("List on missing dir: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List on missing dir returned %d entries", len(list))
	}
}

func namesOf(list []Info) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.Name
	}
	return out
}

func TestParseFilenameSplitsLastDash(t *testing.T) {
	// The tag itself may contain dashes (e.g. "pre-migration"). Verify
	// the parser splits on the LAST dash before the .db.gz suffix.
	got, ts, ok := parseFilename("mnemo-pre-migration-20260518T031742Z.db.gz")
	if !ok {
		t.Fatal("ok=false")
	}
	if got != TagPreMigration {
		t.Errorf("tag = %q, want %q", got, TagPreMigration)
	}
	want, _ := time.Parse("20060102T150405Z", "20260518T031742Z")
	if !ts.Equal(want) {
		t.Errorf("time = %v, want %v", ts, want)
	}
}

// TestGCKeepsOneAndOnlyTheNewest pins the retention the owner chose
// (🎯T158): one snapshot in steady state, a second existing only while
// its replacement is being written.
//
// At the previous default of 7 the directory held ~81 GB against an
// 18.9 GB database. The snapshot exists so the file can be restored if
// it is lost, and a restore takes the newest copy; keeping six older
// ones only covers damage noticed after the next snapshot has run.
func TestGCKeepsOneAndOnlyTheNewest(t *testing.T) {
	dir := t.TempDir()
	// Oldest to newest, across tags, since all tags share one pool.
	names := []string{
		"mnemo-daily-20260101T000000Z.db.gz",
		"mnemo-pre-migration-20260102T000000Z.db.gz",
		"mnemo-manual-20260103T000000Z.db.gz",
		"mnemo-daily-20260104T000000Z.db.gz",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := GCOldest(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Errorf("removed %d, want 3", len(removed))
	}
	left, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Fatalf("kept %d backups, want exactly 1", len(left))
	}
	if got := filepath.Base(left[0].Path); got != names[len(names)-1] {
		t.Errorf("kept %s, want the newest (%s)", got, names[len(names)-1])
	}
}

// TestRetentionSpansBothFormats is the guard for the failure that put ~187
// GB of unmanaged files on disk (🎯T158): retention only manages what
// parseFilename recognises, so a format change that teaches it the new
// suffix and drops the old one leaves every pre-existing snapshot
// invisible — never listed, never collected, growing forever. The switch
// to zstd (🎯T159) is exactly such a change, so both extensions are
// pinned here.
func TestRetentionSpansBothFormats(t *testing.T) {
	dir := t.TempDir()
	// Interleaved deliberately: the old gzip snapshot is NEWER than one of
	// the zstd ones, so a correct implementation must order across formats
	// rather than preferring the new suffix.
	names := []string{
		"mnemo-daily-20260101T000000Z.db.zst",
		"mnemo-daily-20260102T000000Z.db.gz",
		"mnemo-daily-20260103T000000Z.db.zst",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(names) {
		t.Fatalf("List saw %d of %d backups (%v) — an unlisted snapshot is "+
			"one retention will never delete", len(list), len(names), namesOf(list))
	}

	removed, err := GCOldest(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed %d, want 2", len(removed))
	}
	left, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || filepath.Base(left[0].Path) != names[2] {
		t.Errorf("kept %v, want just the newest (%s)", namesOf(left), names[2])
	}
}

// TestParseFilenameRejectsForeignExtensions keeps the recognised set
// closed. Recognising a file means retention may DELETE it, so a loose
// suffix match is a data-loss bug, not a cosmetic one.
func TestParseFilenameRejectsForeignExtensions(t *testing.T) {
	for _, c := range []string{
		"mnemo-daily-20260518T031742Z.db.zst.tmp",
		"mnemo-daily-20260518T031742Z.db.bz2",
		"mnemo-daily-20260518T031742Z.zst",
		"mnemo-daily-20260518T031742Z.db",
	} {
		if _, _, ok := parseFilename(c); ok {
			t.Errorf("parseFilename(%q) = ok; want rejected", c)
		}
	}
}

// TestCreateAndRetainPrunes covers the shared entry point every
// snapshot-creating path now uses.
func TestCreateAndRetainPrunes(t *testing.T) {
	src := seedDB(t, 25)
	dir := t.TempDir()
	// Pre-existing snapshots, older than anything we are about to write.
	for _, n := range []string{
		"mnemo-daily-20260101T000000Z.db.zst",
		"mnemo-daily-20260102T000000Z.db.gz",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, removed, err := CreateAndRetain(src, dir, TagManual, 1, nil)
	if err != nil {
		t.Fatalf("CreateAndRetain: %v", err)
	}
	if len(removed) != 2 {
		t.Errorf("retention removed %d, want 2", len(removed))
	}
	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("kept %d snapshots at keep=1: %v", len(list), namesOf(list))
	}
	// The snapshot just taken must be the survivor — retention must never
	// discard the backup its caller just paid two minutes for.
	if list[0].Path != res.Path {
		t.Errorf("kept %s, want the snapshot just written (%s)", list[0].Path, res.Path)
	}
}

// TestCreateAndRetainKeepFloor stops a zero or negative keep from being
// read as "retain nothing" and deleting the snapshot just written.
func TestCreateAndRetainKeepFloor(t *testing.T) {
	src := seedDB(t, 5)
	for _, keep := range []int{0, -1} {
		dir := t.TempDir()
		res, _, err := CreateAndRetain(src, dir, TagDaily, keep, nil)
		if err != nil {
			t.Fatalf("keep=%d: %v", keep, err)
		}
		if _, err := os.Stat(res.Path); err != nil {
			t.Errorf("keep=%d deleted the snapshot it just wrote: %v", keep, err)
		}
	}
}

// TestHousekeepRunsWithoutABackup is acceptance criterion 4: retention
// must not be a side effect of a successful daily backup. A run of
// failed backups, a disabled schedule, or a day of migrations must still
// prune — that coupling is the reason a directory could grow while
// retention looked healthy.
func TestHousekeepRunsWithoutABackup(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"mnemo-daily-20260101T000000Z.db.zst",
		"mnemo-daily-20260102T000000Z.db.zst",
		"mnemo-daily-20260103T000000Z.db.zst",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A stranded VACUUM temp from a killed run, old enough to sweep.
	orphan := filepath.Join(dir, ".backup-stranded.db")
	if err := os.WriteFile(orphan, []byte("huge"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(orphan, when, when); err != nil {
		t.Fatal(err)
	}

	w := &Worker{dir: dir, keep: 1}
	w.housekeep() // no backup taken, deliberately

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("housekeep left a stranded VACUUM temp behind")
	}
	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("housekeep left %d snapshots at keep=1: %v", len(list), namesOf(list))
	}
}

// TestBackupsInTheSameSecondDoNotCollide guards a silent overwrite.
// Filename has one-second granularity, so two snapshots started in the
// same second named the same file and the second replaced the first via
// the closing rename — the caller was told both were written. Found by
// calling op=backup_now twice in quick succession.
func TestBackupsInTheSameSecondDoNotCollide(t *testing.T) {
	src := seedDB(t, 10)
	dir := t.TempDir()

	first, _, err := CreateAndRetain(src, dir, TagManual, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := CreateAndRetain(src, dir, TagManual, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path {
		t.Fatalf("both snapshots claimed the same path %s — the second overwrote the first",
			first.Path)
	}
	for _, p := range []string{first.Path, second.Path} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("reported snapshot missing from disk: %s", p)
		}
	}
	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("directory holds %d snapshots, want 2: %v", len(list), namesOf(list))
	}
}
