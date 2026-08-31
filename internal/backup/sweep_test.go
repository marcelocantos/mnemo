// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// write creates a file with a given age.
func write(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestSweepRemovesStrandedScratch covers acceptance criterion 2 of
// 🎯T158 with the exact shapes found on disk: dot-prefixed VACUUM temps
// and their SQLite sidecars, plus half-written compressor output.
func TestSweepRemovesStrandedScratch(t *testing.T) {
	dir := t.TempDir()
	old := 24 * time.Hour
	write(t, dir, ".backup-1416910440.db", old)
	write(t, dir, ".backup-1416910440.db-journal", old)
	write(t, dir, ".backup-999.db-wal", old)
	write(t, dir, ".backup-999.db-shm", old)
	write(t, dir, "mnemo-daily-20260101T000000Z.db.zst.tmp", old)
	// Must survive: real backups, and anything the sweep does not
	// recognise. Deleting an unrecognised file would be a worse bug than
	// the leak, so the sweep leaves it for a human.
	write(t, dir, "mnemo-daily-20260102T000000Z.db.zst", old)
	write(t, dir, "mnemo-daily-20260103T000000Z.db.gz", old)
	write(t, dir, "notes.txt", old)
	write(t, dir, "important.db", old)

	swept, err := SweepOrphans(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 5 {
		t.Errorf("swept %d files, want 5: %+v", len(swept), swept)
	}
	got := names(t, dir)
	want := []string{
		"important.db",
		"mnemo-daily-20260102T000000Z.db.zst",
		"mnemo-daily-20260103T000000Z.db.gz",
		"notes.txt",
	}
	if len(got) != len(want) {
		t.Fatalf("survivors = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("survivors = %v, want %v", got, want)
			break
		}
	}
	for _, s := range swept {
		if s.Reason == "" {
			t.Errorf("swept %s with no stated reason", s.Path)
		}
	}
}

// TestSweepSpareYoungScratch is the guard against the sweep deleting a
// backup that is currently being taken. Age is the cross-process
// safety net, so it has to actually hold.
func TestSweepSparesYoungScratch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".backup-inflight.db", 2*time.Minute)
	swept, err := SweepOrphans(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept a 2-minute-old scratch file: %+v", swept)
	}
	if len(names(t, dir)) != 1 {
		t.Error("young scratch file was removed")
	}
}

// TestSweepSparesLiveTempRegardlessOfAge covers the in-process guard.
// A VACUUM of a very large database can outlive any age threshold we
// would be willing to set, so a path this process is actively writing
// is never a candidate.
func TestSweepSparesLiveTempRegardlessOfAge(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, ".backup-live.db", 72*time.Hour)
	markTempLive(p)
	defer markTempDone(p)

	swept, err := SweepOrphans(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept a live temp: %+v", swept)
	}

	// Once the backup finishes, the same file is fair game.
	markTempDone(p)
	swept, err = SweepOrphans(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 {
		t.Fatalf("swept %d after the temp was released, want 1", len(swept))
	}
}

func TestSweepMissingDirIsNotAnError(t *testing.T) {
	swept, err := SweepOrphans(filepath.Join(t.TempDir(), "nope"), time.Hour)
	if err != nil {
		t.Errorf("missing dir: %v", err)
	}
	if len(swept) != 0 {
		t.Errorf("swept %d files from a missing dir", len(swept))
	}
}

// TestUsageSeparatesBackupsFromEverythingElse is acceptance criterion 4:
// the number retention manages and the number the disk holds are
// different questions, and the gap is the whole finding.
func TestUsageSeparatesBackupsFromEverythingElse(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, size int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("mnemo-daily-20260102T000000Z.db.zst", 1000)
	mk("mnemo-daily-20260103T000000Z.db.gz", 2000)
	mk(".backup-orphan.db", 5000) // the 187 GB, in miniature
	mk("stray.txt", 100)

	u, err := Usage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.RetainedCount != 2 || u.RetainedBytes != 3000 {
		t.Errorf("retained = %d files / %d bytes, want 2 / 3000",
			u.RetainedCount, u.RetainedBytes)
	}
	if u.OtherCount != 2 || u.OtherBytes != 5100 {
		t.Errorf("other = %d files / %d bytes, want 2 / 5100",
			u.OtherCount, u.OtherBytes)
	}
	if u.TotalBytes != 8100 {
		t.Errorf("total = %d bytes, want 8100", u.TotalBytes)
	}
	// A directory reporting only its retained size would say 3000 here
	// while occupying 8100. That understatement is the bug.
	if u.TotalBytes == u.RetainedBytes {
		t.Error("Usage collapsed the very distinction it exists to report")
	}
}

func TestUsageMissingDirIsZero(t *testing.T) {
	u, err := Usage(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Errorf("missing dir: %v", err)
	}
	if u.TotalBytes != 0 || u.RetainedCount != 0 {
		t.Errorf("missing dir reported %+v", u)
	}
}
