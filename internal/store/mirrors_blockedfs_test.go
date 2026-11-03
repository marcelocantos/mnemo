// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package store

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestCIReposHonoursCancellationOnBlockedCheckout: a checkout whose
// .git/config blocks in open(2) — here a FIFO with no writer, the same
// kernel-level wait a cloud-backed or stalled volume produces — must
// cost the mirror pass only its deadline, and must be skipped, not
// re-read, by later passes while that read is still parked. Other
// checkouts keep resolving.
func TestCIReposHonoursCancellationOnBlockedCheckout(t *testing.T) {
	ghRoot := filepath.Join(t.TempDir(), "work", "github.com")
	mkRepo(t, filepath.Join(ghRoot, "exampleorg", "healthy"),
		"git@github.com:exampleorg/healthy.git")

	stuckRoot := filepath.Join(ghRoot, "exampleorg", "stuck")
	if err := os.MkdirAll(filepath.Join(stuckRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(stuckRoot, ".git", "config")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	// Unpark the reader at the end: opening the write side lets the
	// blocked open(2) complete, and closing it gives the read EOF.
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})

	s := newTestStore(t, t.TempDir())
	s.SetWorkspaceRoots([]string{filepath.Dir(ghRoot)})
	for _, tc := range []struct{ sess, repo string }{
		{"sess-stuck", "exampleorg/stuck"},
		{"sess-healthy", "exampleorg/healthy"},
	} {
		if _, err := s.writeDB.Exec(
			"INSERT INTO session_meta (session_id, repo) VALUES (?, ?)", tc.sess, tc.repo,
		); err != nil {
			t.Fatal(err)
		}
	}

	call := func(timeout time.Duration) ([]string, error, time.Duration) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		type res struct {
			repos []string
			err   error
		}
		ch := make(chan res, 1)
		start := time.Now()
		go func() {
			r, err := s.ciRepos(ctx)
			ch <- res{r, err}
		}()
		select {
		case r := <-ch:
			return r.repos, r.err, time.Since(start)
		case <-time.After(timeout + 2*time.Second):
			t.Fatalf("ciRepos ignored its %v deadline on a blocked checkout", timeout)
			return nil, nil, 0
		}
	}

	// First pass: the blocked read eats this pass's deadline, no more.
	if _, err, d := call(300 * time.Millisecond); err == nil {
		t.Errorf("first pass returned no error after %v; want its deadline", d)
	} else if d > 1500*time.Millisecond {
		t.Errorf("first pass took %v; want about its 300ms deadline", d)
	}

	// Next pass: the still-parked checkout is skipped and the rest resolve.
	repos, err, d := call(5 * time.Second)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if d > time.Second {
		t.Errorf("second pass took %v; the blocked checkout should be skipped", d)
	}
	got := map[string]bool{}
	for _, r := range repos {
		got[r] = true
	}
	if !got["exampleorg/healthy"] {
		t.Errorf("healthy checkout not resolved alongside a blocked one; got %v", repos)
	}
	if got["exampleorg/stuck"] {
		t.Errorf("blocked checkout reported with a path-derived identity; got %v", repos)
	}
}
