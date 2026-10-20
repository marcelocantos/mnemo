// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func info(name string, at time.Time) Info { return Info{Name: name, Time: at} }

func TestFreshnessStates(t *testing.T) {
	now := time.Date(2026, 10, 19, 21, 0, 0, 0, time.Local)
	fresh := []Info{info("mnemo-daily-a.db.zst", now.Add(-18*time.Hour))}
	stale := []Info{info("mnemo-daily-b.db.zst", now.Add(-31*time.Hour))}
	failed := &Attempt{At: now.Add(-17 * time.Hour), Error: "write: no space left on device"}
	skipped := &Attempt{At: now.Add(-17 * time.Hour), Skipped: true, Error: "insufficient free space for backup: 3.0 GiB free"}
	okAfter := &Attempt{At: now.Add(-18 * time.Hour), OK: true}
	cases := []struct {
		name    string
		list    []Info
		last    *Attempt
		healthy bool
		want    string
	}{
		{"none", nil, nil, false, "no backups exist"},
		{"stale", stale, nil, false, "31h0m0s old (threshold 30h0m0s)"},
		{"failed with no backups", nil, failed, false, "last backup attempt failed"},
		{"failed after the newest backup", fresh, &Attempt{At: now.Add(-1 * time.Hour), Error: "boom"}, false, "failed 1h0m0s ago: boom"},
		{"insufficient space", fresh[:0], skipped, false, "was skipped"},
		{"fresh", fresh, okAfter, true, "18h0m0s old"},
		{"failure superseded by a later good backup", fresh, &Attempt{At: now.Add(-20 * time.Hour), Error: "old"}, true, "18h0m0s old"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			healthy, detail, _ := Freshness(c.list, c.last, now, MaxAge)
			if healthy != c.healthy || !strings.Contains(detail, c.want) {
				t.Fatalf("healthy=%v detail=%q, want healthy=%v containing %q", healthy, detail, c.healthy, c.want)
			}
		})
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestBackupInsufficientSpaceSkipsAndRecords(t *testing.T) {
	src := seedDB(t, 50)
	dir := t.TempDir()
	logs := captureLog(t)
	freeBytesFn = func(string) (int64, error) { return 1 << 20, nil }
	defer func() { freeBytesFn = freeBytes }()

	_, _, err := CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, nil)
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != StatusFile {
			t.Errorf("precheck left %s behind; nothing should be written", e.Name())
		}
	}
	a, ok, err := LastAttempt(dir)
	if err != nil || !ok || a.OK || !a.Skipped || a.Tag != TagDaily {
		t.Fatalf("attempt = %+v ok=%v err=%v, want a skipped daily attempt", a, ok, err)
	}
	if !strings.Contains(logs.String(), "backup: snapshot skipped") || !strings.Contains(logs.String(), "insufficient free space") {
		t.Fatalf("log missing the skip: %s", logs)
	}
	list, _ := List(dir)
	if healthy, detail, _ := Freshness(list, &a, time.Now(), MaxAge); healthy {
		t.Fatalf("health stayed healthy after a skipped attempt: %s", detail)
	}
}

func TestBackupRecordsSuccessAndLogsComplete(t *testing.T) {
	src := seedDB(t, 50)
	dir := t.TempDir()
	logs := captureLog(t)
	res, removed, err := CreateAndRetain(src, dir, TagManual, Retention{Keep: 1}, nil)
	LogResult("backup_now", res, removed, err)
	if err != nil {
		t.Fatal(err)
	}
	a, ok, _ := LastAttempt(dir)
	if !ok || !a.OK || a.Path != res.Path || a.CompressedSize != res.CompressedSize {
		t.Fatalf("attempt = %+v, want OK for %s", a, res.Path)
	}
	line := logs.String()
	if !strings.Contains(line, `msg="backup_now: snapshot complete"`) || !strings.Contains(line, "path="+res.Path) ||
		!strings.Contains(line, "compressed_mb=") {
		t.Fatalf("log line = %s", line)
	}
	u, _ := Usage(dir)
	if u.OtherCount != 0 {
		t.Fatalf("status file counted as a stray: %+v", u)
	}
	if _, running := InProgress(); running {
		t.Fatal("InProgress still set after the snapshot finished")
	}
}

func TestLogResultFailure(t *testing.T) {
	logs := captureLog(t)
	LogResult("backup_now", Result{}, nil, errors.New("disk full"))
	if !strings.Contains(logs.String(), `msg="backup_now: snapshot failed"`) || !strings.Contains(logs.String(), "disk full") {
		t.Fatalf("log = %s", logs)
	}
}

// A failed snapshot is recorded as failed, so /health can say so.
func TestBackupFailureRecorded(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "not-a-db.db")
	if err := os.WriteFile(src, []byte("generic bytes that are not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, nil); err == nil {
		t.Fatal("snapshot of a non-database succeeded")
	}
	a, ok, _ := LastAttempt(dir)
	if !ok || a.OK || a.Skipped || a.Error == "" {
		t.Fatalf("attempt = %+v, want a recorded failure", a)
	}
}

// The precheck sweeps stranded scratch first, including a lone
// .backup-*.db-journal left by a killed run, so a stale temp neither
// survives nor counts against the space check.
func TestBackupSweepsOrphanJournalBeforeSnapshot(t *testing.T) {
	src := seedDB(t, 20)
	dir := t.TempDir()
	old := time.Now().Add(-18 * time.Hour)
	for _, n := range []string{".backup-186272418.db-journal", ".backup-555.db"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, make([]byte, 1024), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old)
	}
	fresh := filepath.Join(dir, ".backup-777.db-journal") // too young to touch
	os.WriteFile(fresh, nil, 0o644)
	if _, _, err := CreateAndRetain(src, dir, TagDaily, Retention{Keep: 1}, nil); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{".backup-186272418.db-journal", ".backup-555.db"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s survived the pre-snapshot sweep", n)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("young scratch file was swept: %v", err)
	}
}

func TestRequiredSpace(t *testing.T) {
	src := seedDB(t, 10)
	st, _ := os.Stat(src)
	need, err := RequiredSpace(src)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(float64(st.Size())*spaceFactor) + spaceMargin; need < want {
		t.Fatalf("need = %d, want >= %d", need, want)
	}
}
