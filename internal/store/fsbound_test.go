// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestBoundedFSAbandonsAndDedupes: a read that never returns costs the
// caller its deadline, and later calls for the same key fail fast
// instead of parking another goroutine on it until it finally returns.
func TestBoundedFSAbandonsAndDedupes(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	read := func() string {
		calls.Add(1)
		<-release
		return "value"
	}
	key := "test:" + t.Name()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := boundedFS(ctx, key, read); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked read: err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("blocked read held the caller for %v", d)
	}

	if _, err := boundedFS(context.Background(), key, read); !errors.Is(err, errFSStillBlocked) {
		t.Fatalf("second call while blocked: err = %v, want errFSStillBlocked", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("read started %d times while the first was still blocked, want 1", got)
	}

	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := boundedFS(context.Background(), key, func() string { return "fresh" })
		if err == nil {
			if v != "fresh" {
				t.Fatalf("got %q, want fresh", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key never released after the blocked read returned: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
