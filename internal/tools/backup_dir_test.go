// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/store"
)

// 🎯T199: backup_status and backup_now use the configured `backup.dir`.
// Both used to hardcode the directory beside the database, so with a
// custom directory they reported on, and wrote to, a folder the nightly
// worker had stopped using.

// withBackupDir points the handler's store at a custom snapshot
// directory, as the registry does from `backup.dir`.
func withBackupDir(t *testing.T, ch *callHandler, dir string) {
	t.Helper()
	ch.mem.(*store.Store).SetBackupDir(dir)
}

// awayDir returns a snapshot directory under a regular file: the portable
// stand-in for a volume that is not attached.
func awayDir(t *testing.T) string {
	t.Helper()
	volume := filepath.Join(t.TempDir(), "volume")
	if err := os.WriteFile(volume, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(volume, "mnemo", "backups")
}

func TestBackupToolsHonourBackupDir(t *testing.T) {
	ch := newNoteHandler(t)
	custom := t.TempDir()
	withBackupDir(t, ch, custom)
	if got := ch.backupDir(); got != custom {
		t.Fatalf("backupDir() = %s, want the configured %s", got, custom)
	}
	if ch.statusDir() == custom {
		t.Fatal("the status record must stay beside the database, not follow backup.dir")
	}

	// backup_status lists what is in the configured directory.
	name := backup.Filename(backup.TagDaily, time.Now().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(custom, name), []byte("generic"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr, _ := ch.backupStatus()
	if isErr || !strings.Contains(out, name) || !strings.Contains(out, custom) {
		t.Fatalf("backup_status does not report on the configured directory:\n%s", out)
	}

	// backup_now writes there, and records beside the database.
	type call struct {
		dir  string
		args *backup.BackupArgs
	}
	got := make(chan call, 1)
	backupCreate = func(src, dir string, tag backup.Tag, keep backup.Retention, args *backup.BackupArgs) (backup.Result, []string, error) {
		got <- call{dir, args}
		return backup.Result{Path: filepath.Join(dir, "mnemo-manual-generic.db.zst")}, nil, nil
	}
	defer func() { backupCreate = backup.CreateAndRetain }()
	out, isErr, _ = ch.backupNow(map[string]any{"force": true})
	if isErr || !strings.Contains(out, "Backup started in "+custom) {
		t.Fatalf("backup_now = %q isErr=%v", out, isErr)
	}
	select {
	case c := <-got:
		if c.dir != custom {
			t.Errorf("backup_now snapshot directory = %s, want %s", c.dir, custom)
		}
		if c.args == nil || c.args.StatusDir != ch.statusDir() {
			t.Errorf("backup_now args = %+v, want StatusDir %s", c.args, ch.statusDir())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backup_now never started a snapshot")
	}
	for manualRunning.Load() {
		time.Sleep(time.Millisecond)
	}
}

// The destination is away: backup_status says so, and still reports the
// last attempt and the last success.
func TestBackupStatusDestinationAway(t *testing.T) {
	ch := newNoteHandler(t)
	dir := awayDir(t)
	withBackupDir(t, ch, dir)
	statusDir := ch.statusDir()
	if err := backup.RecordAttempt(statusDir, backup.Attempt{
		At: time.Now().Add(-26 * time.Hour), Tag: backup.TagDaily, OK: true}); err != nil {
		t.Fatal(err)
	}
	backup.RecordUnavailable(statusDir, backup.TagDaily, errors.New("backup destination not available: "+dir))

	out, isErr, _ := ch.backupStatus()
	if isErr {
		t.Fatalf("backup_status errored: %s", out)
	}
	for _, want := range []string{"backup destination not available", dir, "Last attempt: SKIPPED", "Last success: "} {
		if !strings.Contains(out, want) {
			t.Errorf("backup_status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "No backups found") || strings.Contains(out, "none recorded") {
		t.Errorf("backup_status reads as if no backup exists:\n%s", out)
	}
}

// backup_now with the destination away: refused synchronously, recorded
// as a skip, and nothing written beside the database.
func TestBackupNowDestinationAway(t *testing.T) {
	ch := newNoteHandler(t)
	dir := awayDir(t)
	withBackupDir(t, ch, dir)
	backupCreate = func(string, string, backup.Tag, backup.Retention, *backup.BackupArgs) (backup.Result, []string, error) {
		t.Error("backup_now started a snapshot with the destination away")
		return backup.Result{}, nil, nil
	}
	defer func() { backupCreate = backup.CreateAndRetain }()

	out, isErr, _ := ch.backupNow(map[string]any{"force": true})
	if !isErr || !strings.Contains(out, "not started") || !strings.Contains(out, "backup destination not available") {
		t.Fatalf("backup_now = %q isErr=%v, want a refusal naming the missing destination", out, isErr)
	}
	a, ok, err := backup.LastAttempt(ch.statusDir())
	if err != nil || !ok || !a.Unavailable || !a.Skipped || a.Tag != backup.TagManual {
		t.Fatalf("recorded attempt = %+v ok=%v err=%v, want an unavailable manual skip", a, ok, err)
	}
	if list, _ := backup.List(ch.statusDir()); len(list) != 0 {
		t.Errorf("a snapshot was written beside the database: %v", list)
	}
}
