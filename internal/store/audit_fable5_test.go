// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Regression tests for the Fable-5 deep-audit findings (🎯T109):
//   - 🎯T104 oversized JSONL line silently dropped (and its tail)
//   - 🎯T105 unbounded image-sidecar goroutine spawn
//   - 🎯T107 mid-stream ingest commit persists an overshot offset
// (🎯T103/🎯T106 — the read-only query surface — are covered by
// TestQuery and TestQueryRejectsAttach in store_test.go.)

package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestIngestOversizedLineNotDropped verifies that a JSONL line larger
// than the old bufio.Scanner token cap (1<<20) is ingested rather than
// silently skipped, and that content after it is not lost either
// (🎯T104). Before the fix, bufio.Scanner returned ErrTooLong on the
// oversized line, the scan loop exited as if at EOF, and the offset was
// advanced past it.
func TestIngestOversizedLineNotDropped(t *testing.T) {
	projectDir := t.TempDir()

	oversized := "giantmarker " + strings.Repeat("x", 1<<20) // > 1 MiB line
	writeJSONL(t, projectDir, "proj", "sess-oversize", []map[string]any{
		msg("user", "alpha marker line", "2026-04-01T10:00:00Z"),
		msg("assistant", oversized, "2026-04-01T10:00:05Z"),
		msg("user", "omega marker after the giant line", "2026-04-01T10:00:10Z"),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}

	countLike := func(pattern string) int64 {
		rows, err := s.Query("SELECT COUNT(*) AS c FROM messages WHERE text LIKE '%" + pattern + "%'")
		if err != nil {
			t.Fatal(err)
		}
		c, _ := rows[0]["c"].(int64)
		return c
	}

	if countLike("giantmarker") == 0 {
		t.Error("oversized line was dropped (🎯T104)")
	}
	if countLike("omega marker") == 0 {
		t.Error("content after the oversized line was dropped (🎯T104)")
	}
}

// TestRunSidecarBoundsSpawn verifies that runSidecar acquires a semaphore
// slot before spawning, so the caller back-pressures once the pool is
// full instead of launching an unbounded number of goroutines that each
// pin image bytes (🎯T105). Before the fix the semaphore was acquired
// inside the already-spawned goroutine.
func TestRunSidecarBoundsSpawn(t *testing.T) {
	const slots = 2
	s := &Store{imageSem: make(chan struct{}, slots)}

	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	fn := func() {
		entered <- struct{}{}
		<-release
	}

	producerDone := make(chan struct{})
	go func() {
		for i := 0; i < slots+3; i++ {
			s.runSidecar(fn)
		}
		close(producerDone)
	}()

	// Exactly `slots` sidecars start; the next runSidecar call must block
	// the producer acquiring a slot.
	for i := 0; i < slots; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected %d sidecars to start", slots)
		}
	}
	select {
	case <-producerDone:
		t.Fatal("producer finished without back-pressure: spawn is unbounded (🎯T105)")
	case <-time.After(100 * time.Millisecond):
		// expected: producer parked acquiring the next slot
	}

	close(release)
	select {
	case <-producerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not drain after release")
	}
}

// TestIngestFileMidStreamOffsetOnBoundary verifies that the offset
// persisted at a mid-stream commit is a true line boundary, so a crash
// between that commit and completion cannot skip buffered-but-unwritten
// lines (🎯T107). Before the fix the offset came from f.Seek(0,1), which
// reads ahead of the last processed line.
func TestIngestFileMidStreamOffsetOnBoundary(t *testing.T) {
	savedLC, savedCI := ingestLineCheckInterval, ingestCommitInterval
	ingestLineCheckInterval, ingestCommitInterval = 1, 0 // commit after every line
	var offsets []int64
	testMidStreamCommitOffset = func(off int64) { offsets = append(offsets, off) }
	t.Cleanup(func() {
		ingestLineCheckInterval, ingestCommitInterval = savedLC, savedCI
		testMidStreamCommitOffset = nil
	})

	projectDir := t.TempDir()
	// Lines wide enough that bufio reads ahead of the last processed line,
	// so the pre-fix f.Seek offset would land mid-line.
	entries := make([]map[string]any, 0, 20)
	for i := 0; i < 20; i++ {
		entries = append(entries,
			msg("user", fmt.Sprintf("line-%02d ", i)+strings.Repeat("x", 600),
				fmt.Sprintf("2026-04-01T10:%02d:00Z", i)))
	}
	path := writeJSONL(t, projectDir, "proj", "sess-mid", entries)

	s := newTestStore(t, projectDir)
	if err := s.ingestFile(path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) == 0 {
		t.Fatal("no mid-stream commit fired; cannot validate offset")
	}
	for _, off := range offsets {
		if off <= 0 || off > int64(len(data)) {
			t.Errorf("mid-stream offset %d out of range [1,%d]", off, len(data))
			continue
		}
		if off < int64(len(data)) && data[off-1] != '\n' {
			t.Errorf("mid-stream offset %d is not a line boundary (preceding byte %q) (🎯T107)",
				off, data[off-1])
		}
	}
}
