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
