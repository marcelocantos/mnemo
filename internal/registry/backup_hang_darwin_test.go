// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package registry

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/backup"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/tools"
)

// 🎯T200: a `backup.dir` whose open() never returns.
//
// On 2026-11-13 open() on /Volumes/Storage/mnemo/backups sat in the
// kernel indefinitely, most likely while macOS waited on a
// removable-volume permission decision for a newly signed binary. The
// stand-in here is the same call blocking for real: a small dylib,
// injected with DYLD_INSERT_LIBRARIES, interposes libc's open() and parks
// any open of a path containing a marker until a release file appears.
// Go on darwin calls open() through libSystem, so every open of the
// directory — os.ReadDir included — blocks exactly as it did then. An
// in-process seam cannot do this for a build that predates the seam.
//
// The child half uses only API that existed in v0.117.0, so this file
// can be dropped into that release unchanged. There it fails at the
// first deadline: startBackupWorker swept the directory synchronously
// and never returned.

const hangOpenC = `
#include <fcntl.h>
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/stat.h>
static int hang_open(const char *path, int flags, ...) {
	int mode = 0;
	if (flags & O_CREAT) { va_list ap; va_start(ap, flags); mode = va_arg(ap, int); va_end(ap); }
	const char *marker = getenv("HANG_OPEN_MARKER");
	if (marker && *marker && path && strstr(path, marker)) {
		const char *release = getenv("HANG_OPEN_RELEASE");
		struct stat st;
		while (!(release && stat(release, &st) == 0)) usleep(10000);
	}
	return open(path, flags, mode);
}
__attribute__((used)) static struct { const void *n; const void *o; } interposers[]
	__attribute__((section("__DATA,__interpose"))) = { { (const void *)hang_open, (const void *)open } };
`

const hangChildEnv = "MNEMO_HANG_OPEN_CHILD"

func TestStartupSurvivesHangingBackupDir(t *testing.T) {
	if os.Getenv(hangChildEnv) != "" {
		hangingBackupDirChild(t)
		return
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler to build the open() interposer")
	}
	work := t.TempDir()
	src := filepath.Join(work, "hang.c")
	lib := filepath.Join(work, "hang.dylib")
	if err := os.WriteFile(src, []byte(hangOpenC), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(cc, "-dynamiclib", "-o", lib, src).CombinedOutput(); err != nil {
		t.Skipf("cannot build the open() interposer: %v\n%s", err, out)
	}
	marker := "mnemo-hang-open-" + filepath.Base(work)
	dir := filepath.Join(work, marker, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run", "^TestStartupSurvivesHangingBackupDir$", "-test.v")
	cmd.Env = append(os.Environ(),
		hangChildEnv+"="+dir,
		"DYLD_INSERT_LIBRARIES="+lib,
		"HANG_OPEN_MARKER="+marker,
		"HANG_OPEN_RELEASE="+filepath.Join(work, "release"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child with a hanging backup.dir failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "interposer active") {
		t.Fatalf("the open() interposer did not take effect in the child:\n%s", out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "_test.go:") {
			t.Log("child:", strings.TrimSpace(line))
		}
	}
}

// hangingBackupDirChild runs with open() of the backup directory parked
// in the kernel.
func hangingBackupDirChild(t *testing.T) {
	dir := os.Getenv(hangChildEnv)

	// Prove the stand-in is live: an open of the directory must block.
	probe := make(chan struct{})
	go func() { defer close(probe); os.ReadDir(dir) }()
	select {
	case <-probe:
		t.Fatal("open() of the backup directory returned; the interposer is not active")
	case <-time.After(300 * time.Millisecond):
		t.Log("interposer active: open() of the backup directory blocks")
	}

	root := t.TempDir()
	dbDir := filepath.Join(root, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(filepath.Join(dbDir, "mnemo.db"), root)
	if err != nil {
		t.Fatal(err)
	}
	s.SetBackupDir(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRegistry(ctx, store.Config{}, "")
	e := &userEntry{store: s, homeDir: root}

	deadline := func(what string, d time.Duration, fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); fn() }()
		select {
		case <-done:
		case <-time.After(d):
			t.Fatalf("%s did not return within %s against a backup.dir that does not answer", what, d)
		}
	}

	// Store bring-up.
	deadline("startBackupWorker (store bring-up)", 3*time.Second, func() {
		r.startBackupWorker("default", e, slog.Default())
	})

	h := tools.NewHandler(func(string) (store.Backend, error) { return s, nil })
	call := func(args map[string]any) string {
		out, _, err := h.Call(ctx, tools.CallContext{}, "mnemo_ops", args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// backup_status answers, and says why nothing is listed.
	deadline("backup_status", 12*time.Second, func() {
		out := call(map[string]any{"op": "backup_status"})
		if !strings.Contains(out, "not available") {
			t.Errorf("backup_status = %q, want it to name the unavailable destination", out)
		}
	})

	// backup_now answers, and the skip is recorded beside the database.
	statusDir := backup.DefaultDir(s.DBPath())
	deadline("backup_now", 12*time.Second, func() {
		call(map[string]any{"op": "backup_now", "force": true})
	})
	recorded := time.Now().Add(15 * time.Second)
	for {
		a, ok, _ := backup.LastAttempt(statusDir)
		if ok && a.Skipped && strings.Contains(a.Error, "not available") {
			t.Logf("skip recorded: %s", a.Error)
			break
		}
		if time.Now().After(recorded) {
			t.Fatalf("no skip recorded for backup_now; last attempt = %+v ok=%v", a, ok)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// backup.recent answers and names the cause.
	deadline("backup.recent", 12*time.Second, func() {
		res := backupRecent(s.BackupDir(), statusDir, time.Now())
		if !strings.Contains(res.Detail, "not available") {
			t.Errorf("backup.recent = %v %q, want it to name the unavailable destination", res.Severity, res.Detail)
		}
		t.Logf("backup.recent: %v %s", res.Severity, res.Detail)
	})
}
