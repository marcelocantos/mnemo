// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T201: stage on the database volume while backup.dir is away, and
// replicate there incrementally when it returns.

// stagingLayout is a database volume (statusDir, with staging inside it)
// and an external destination that starts away.
type stagingLayout struct {
	src, dest, statusDir, staging string
	attach                        func()
}

func newStagingLayout(t *testing.T) stagingLayout {
	t.Helper()
	dest, attach := absentDest(t)
	statusDir := t.TempDir()
	return stagingLayout{
		src: seedDB(t, 50), dest: dest, statusDir: statusDir,
		staging: filepath.Join(statusDir, StagingSubdir),
		attach: func() {
			attach()
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
}

func (l stagingLayout) args() *BackupArgs {
	return &BackupArgs{StatusDir: l.statusDir, StagingDir: l.staging}
}

// Destination present: the snapshot goes straight there, and nothing is
// written to the database volume beyond the status file.
func TestStagingDirectWriteWhenDestinationAnswers(t *testing.T) {
	l := newStagingLayout(t)
	l.attach()
	res, _, err := CreateAndRetain(l.src, l.dest, TagManual, Retention{Keep: 1}, l.args())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(res.Path) != l.dest {
		t.Fatalf("snapshot at %s, want it in the destination %s", res.Path, l.dest)
	}
	entries, _ := os.ReadDir(l.statusDir)
	if len(entries) != 1 || entries[0].Name() != StatusFile {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("database volume holds %v, want only %s", names, StatusFile)
	}
	if a, _, _ := LastAttempt(l.statusDir); !a.OK || a.Staged {
		t.Errorf("attempt = %+v, want a direct success", a)
	}
}

// Destination absent: the snapshot is staged, recorded as staged and not
// skipped, and staging keeps one snapshot per tag.
func TestStagingWhenDestinationAbsent(t *testing.T) {
	l := newStagingLayout(t)
	first, _, err := CreateAndRetain(l.src, l.dest, TagDaily, Retention{Keep: 1}, l.args())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(first.Path) != l.staging {
		t.Fatalf("snapshot at %s, want staging %s", first.Path, l.staging)
	}
	a, _, _ := LastAttempt(l.statusDir)
	if !a.OK || !a.Staged || a.Skipped || a.Unavailable ||
		!strings.Contains(a.StagedBecause, "backup destination not available") {
		t.Fatalf("attempt = %+v, want staged (not skipped) naming the absent destination", a)
	}
	if a.LastDailySuccess.IsZero() {
		t.Error("a staged daily does not count for the window; the worker would take it again")
	}
	v, _ := Staging(l.staging)
	if v.Verdict(time.Now()) != StagedWaiting || v.WaitingSince.IsZero() {
		t.Fatalf("verdict = %v waiting=%v, want StagedWaiting", v.Verdict(time.Now()), v.WaitingSince)
	}
	since := v.WaitingSince

	// A newer daily replaces the older one still waiting; a manual one
	// sits beside it. The waiting clock does not restart.
	time.Sleep(1100 * time.Millisecond) // a distinct filename timestamp
	second, _, err := CreateAndRetain(l.src, l.dest, TagDaily, Retention{Keep: 1}, l.args())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CreateAndRetain(l.src, l.dest, TagManual, Retention{Keep: 1}, l.args()); err != nil {
		t.Fatal(err)
	}
	list, _ := List(l.staging)
	byTag := map[Tag][]string{}
	for _, b := range list {
		byTag[b.Tag] = append(byTag[b.Tag], b.Path)
	}
	if len(byTag[TagDaily]) != 1 || byTag[TagDaily][0] != second.Path || len(byTag[TagManual]) != 1 {
		t.Fatalf("staging = %v, want the newest daily (%s) and one manual", byTag, second.Path)
	}
	if v, _ := Staging(l.staging); !v.WaitingSince.Equal(since) {
		t.Errorf("waiting since %v, want it kept at %v", v.WaitingSince, since)
	}
}

// The free-space precheck applies to staging, and a refusal is a skip
// naming the cause.
func TestStagingSpacePrecheck(t *testing.T) {
	l := newStagingLayout(t)
	prev := freeBytesFn
	freeBytesFn = func(string) (int64, error) { return 1 << 20, nil }
	defer func() { freeBytesFn = prev }()
	_, _, err := CreateAndRetain(l.src, l.dest, TagDaily, Retention{Keep: 1}, l.args())
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}
	a, _, _ := LastAttempt(l.statusDir)
	if !a.Skipped || !a.Staged || !strings.Contains(a.Error, "insufficient free space") {
		t.Fatalf("attempt = %+v, want a skip naming the space and the staging", a)
	}
}

// Destination returning mid-run: the replicator copies the staged
// snapshot under its original name, verifies it, and deletes the staged
// copy.
func TestReplicatorCopiesWhenDestinationReturns(t *testing.T) {
	l := newStagingLayout(t)
	res, _, err := CreateAndRetain(l.src, l.dest, TagDaily, Retention{Keep: 1}, l.args())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(res.Path)

	rep := &Replicator{Dest: l.dest, StagingDir: l.staging, Retention: Retention{Keep: 1}, Poll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); rep.Run(ctx) }()

	time.Sleep(50 * time.Millisecond) // a few passes with the destination away
	if v, _ := Staging(l.staging); len(v.Staged) != 1 {
		t.Fatalf("staging holds %d snapshots while the destination is away, want 1", len(v.Staged))
	}
	l.attach()

	final := filepath.Join(l.dest, filepath.Base(res.Path))
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, _ := Staging(l.staging)
		if _, err := os.Stat(final); err == nil && len(v.Staged) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not replicated within 5s; staging=%+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	got, _ := os.ReadFile(final)
	if !bytes.Equal(got, want) {
		t.Fatal("replicated copy differs from the staged snapshot")
	}
	if err := verifyCompressed(final, mustRawSize(t, final)); err != nil {
		t.Fatalf("replicated snapshot does not decompress: %v", err)
	}
	// Retention on the destination sees it with its original timestamp.
	list, _ := List(l.dest)
	if len(list) != 1 || list[0].Name != filepath.Base(res.Path) {
		t.Fatalf("destination = %+v", list)
	}
	v, _ := Staging(l.staging)
	if v.Verdict(time.Now()) != NothingStaged || v.LastReplicated != filepath.Base(res.Path) {
		t.Fatalf("staging view after replication = %+v", v)
	}
	if _, err := os.Stat(progressPath(res.Path)); !os.IsNotExist(err) {
		t.Errorf("progress record left behind: %v", err)
	}
}

func mustRawSize(t *testing.T, path string) int64 {
	t.Helper()
	n, err := decompressTo(path, discard{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// stageRandom writes a staged snapshot of n random bytes. The replicator
// copies bytes; it does not care what they are.
func stageRandom(t *testing.T, staging string, tag Tag, at time.Time, n int) (string, []byte) {
	t.Helper()
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(staging, Filename(tag, at))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, b
}

// Detach during replication: nothing appears under the final name, and
// the next pass — standing in for a reattach, a daemon restart or a
// reboot — resumes from the last verified offset rather than the start.
func TestReplicatorResumesAfterDetach(t *testing.T) {
	prevChunk := replicaChunk
	replicaChunk = 4096
	defer func() { replicaChunk = prevChunk; replicaFault = nil }()

	l := newStagingLayout(t)
	l.attach()
	staged, want := stageRandom(t, l.staging, TagDaily, time.Now().Add(-time.Hour), 25*4096+123)
	name := filepath.Base(staged)
	final := filepath.Join(l.dest, name)

	replicaFault = func(_ string, off int64) error {
		if off >= 10*4096 {
			return errors.New("device not configured") // the drive was pulled
		}
		return nil
	}
	rep := &Replicator{Dest: l.dest, StagingDir: l.staging, Retention: Retention{Keep: 1}}
	if _, err := rep.Pass(context.Background()); err == nil {
		t.Fatal("pass succeeded through a detach")
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("a file exists under the final name after a failed copy: %v", err)
	}
	if fi, err := os.Stat(final + ReplicaPartExt); err != nil || fi.Size() != 10*4096 {
		t.Fatalf("partial copy = %v %v, want 10 chunks under the temporary name", fi, err)
	}
	if v, _ := Staging(l.staging); len(v.Staged) != 1 || v.Staged[0].Copied != 10*4096 {
		t.Fatalf("staging view = %+v, want progress at 10 chunks", v)
	}

	// Reattached, and a fresh replicator (a restarted daemon).
	replicaFault = nil
	rep = &Replicator{Dest: l.dest, StagingDir: l.staging, Retention: Retention{Keep: 1}}
	report, err := rep.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ResumedAt != 10*4096 || report.Copied != int64(len(want))-10*4096 {
		t.Fatalf("resumed at %d and copied %d, want to resume at %d and copy only the rest",
			report.ResumedAt, report.Copied, 10*4096)
	}
	got, _ := os.ReadFile(final)
	if !bytes.Equal(got, want) {
		t.Fatal("resumed copy differs from the staged snapshot")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staged copy not deleted after replication")
	}
	if _, err := os.Stat(final + ReplicaPartExt); !os.IsNotExist(err) {
		t.Error("temporary copy left behind")
	}
}

// A partial copy that does not match its record (overwritten, or a
// write that never reached the disk) is not trusted: the copy restarts,
// and the result is still exact.
func TestReplicatorRestartsOnMismatchedPartial(t *testing.T) {
	prevChunk := replicaChunk
	replicaChunk = 4096
	defer func() { replicaChunk = prevChunk; replicaFault = nil }()

	l := newStagingLayout(t)
	l.attach()
	staged, want := stageRandom(t, l.staging, TagManual, time.Now().Add(-time.Hour), 8*4096)
	final := filepath.Join(l.dest, filepath.Base(staged))
	replicaFault = func(_ string, off int64) error {
		if off >= 4*4096 {
			return errors.New("input/output error")
		}
		return nil
	}
	rep := &Replicator{Dest: l.dest, StagingDir: l.staging, Retention: Retention{Keep: 1}}
	_, _ = rep.Pass(context.Background())
	// Corrupt the last recorded chunk on the destination.
	f, err := os.OpenFile(final+ReplicaPartExt, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("garbage"), 3*4096+5); err != nil {
		t.Fatal(err)
	}
	f.Close()

	replicaFault = nil
	report, err := rep.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.ResumedAt != 0 {
		t.Errorf("resumed at %d from a partial that does not match its record, want 0", report.ResumedAt)
	}
	got, _ := os.ReadFile(final)
	if !bytes.Equal(got, want) {
		t.Fatal("copy differs from the staged snapshot")
	}
}

// A staged snapshot that retention on the destination would discard on
// arrival — the destination already has newer ones of its pool — is
// deleted without copying it.
func TestReplicatorDropsSupersededStagedSnapshot(t *testing.T) {
	l := newStagingLayout(t)
	l.attach()
	staged, _ := stageRandom(t, l.staging, TagDaily, time.Now().Add(-48*time.Hour), 4096)
	newer := filepath.Join(l.dest, Filename(TagDaily, time.Now().Add(-time.Hour)))
	if err := os.WriteFile(newer, []byte("newer"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &Replicator{Dest: l.dest, StagingDir: l.staging, Retention: Retention{Keep: 1}}
	report, err := rep.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Superseded) != 1 || report.Copied != 0 {
		t.Fatalf("report = %+v, want the staged snapshot dropped uncopied", report)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("superseded staged snapshot not deleted")
	}
	if _, err := os.Stat(newer); err != nil {
		t.Error("the newer snapshot on the destination was touched")
	}
}

// A replicated snapshot counts for retention on the destination exactly
// as a directly written one, by its original timestamp.
func TestReplicatedSnapshotCountsForRetention(t *testing.T) {
	l := newStagingLayout(t)
	l.attach()
	older := filepath.Join(l.dest, Filename(TagDaily, time.Now().Add(-48*time.Hour)))
	if err := os.WriteFile(older, []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, _ := stageRandom(t, l.staging, TagDaily, time.Now().Add(-time.Hour), 4096)
	rep := &Replicator{Dest: l.dest, StagingDir: l.staging, Retention: Retention{Keep: 1}}
	if _, err := rep.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, _ := List(l.dest)
	if len(list) != 1 || list[0].Name != filepath.Base(staged) {
		t.Fatalf("destination = %+v, want only the replicated (newer) snapshot", list)
	}
}

// The three states, and the threshold.
func TestStagingVerdictThreshold(t *testing.T) {
	staging := t.TempDir()
	now := time.Now()
	v, _ := Staging(staging)
	if v.Verdict(now) != NothingStaged {
		t.Fatalf("empty staging: %v", v.Verdict(now))
	}
	stageRandom(t, staging, TagDaily, now.Add(-time.Hour), 16)
	if err := writeStagingState(staging, stagingState{WaitingSince: now.Add(-6 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := Staging(staging); v.Verdict(now) != StagedWaiting {
		t.Fatalf("staged 6 days: %v, want StagedWaiting", v.Verdict(now))
	}
	if err := writeStagingState(staging, stagingState{WaitingSince: now.Add(-8 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	v, _ = Staging(staging)
	if v.Verdict(now) != StagedTooLong {
		t.Fatalf("staged 8 days: %v, want StagedTooLong", v.Verdict(now))
	}
	if s := v.Summary(now); !strings.Contains(s, "staged on the database volume") {
		t.Errorf("summary = %q", s)
	}
}

// The nightly worker with staging: a window with the destination away
// produces a staged snapshot, owes nothing, and does not run again.
func TestWorkerStagesWhenDestinationAway(t *testing.T) {
	l := newStagingLayout(t)
	cfg, end := compressedWindow(t, l.src, l.dest, 50*time.Millisecond, 300*time.Millisecond)
	cfg.StatusDir, cfg.StagingDir = l.statusDir, l.staging
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(300*time.Millisecond))
	defer cancel()
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	<-done
	if n := dailies(t, l.staging); n != 1 {
		t.Fatalf("staging holds %d dailies after the window, want 1", n)
	}
	if !w.owedSince.IsZero() {
		t.Errorf("a staged snapshot left a debt (owed since %v)", w.owedSince)
	}
	if a, _, _ := LastAttempt(l.statusDir); !a.OK || !a.Staged || a.Tag != TagDaily {
		t.Errorf("attempt = %+v, want a staged daily", a)
	}
}
