// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
)

// backup_now returns as soon as the snapshot starts, refuses a second
// concurrent run, and logs the outcome when the snapshot finishes.
func TestBackupNowReturnsImmediatelyAndLogs(t *testing.T) {
	ch := newNoteHandler(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	release := make(chan struct{})
	done := make(chan struct{})
	backupCreate = func(src, dir string, tag backup.Tag, keep backup.Retention, args *backup.BackupArgs) (backup.Result, []string, error) {
		<-release
		return backup.Result{Path: dir + "/mnemo-manual-generic.db.zst", RawSize: 3 << 20, CompressedSize: 1 << 20, Elapsed: time.Second}, nil, nil
	}
	defer func() { backupCreate = backup.CreateAndRetain }()

	start := time.Now()
	out, isErr, err := ch.backupNow(map[string]any{"force": true})
	if err != nil || isErr || !strings.Contains(out, "Backup started") {
		t.Fatalf("backupNow = %q isErr=%v err=%v", out, isErr, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("backupNow blocked for %s", d)
	}
	out, _, _ = ch.backupNow(map[string]any{"force": true})
	if !strings.Contains(out, "already") {
		t.Fatalf("second concurrent backupNow = %q, want an already-running reply", out)
	}
	go func() {
		close(release)
		for manualRunning.Load() {
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background snapshot did not finish")
	}
	if !strings.Contains(buf.String(), `msg="backup_now: snapshot complete"`) || !strings.Contains(buf.String(), "mnemo-manual-generic.db.zst") {
		t.Fatalf("log = %s", buf.String())
	}
}

// backup_status reports the recorded outcome a backup_now caller polls for.
func TestBackupStatusShowsLastAttempt(t *testing.T) {
	ch := newNoteHandler(t)
	dir := ch.backupDir()
	if err := ensureDir(dir); err != nil {
		t.Fatal(err)
	}
	backup.RecordAttempt(dir, backup.Attempt{At: time.Now().Add(-time.Minute), Skipped: true,
		Error: "insufficient free space for backup: generic"})
	out, _, _ := ch.backupStatus()
	if !strings.Contains(out, "Last attempt: SKIPPED") {
		t.Fatalf("status = %q", out)
	}
}
