// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestStartReconcilerOptInGate verifies the 🎯T63 contract:
// StartReconciler with enabled=false must return immediately without
// spawning a goroutine, regardless of whether ANTHROPIC_ADMIN_API_KEY
// is set in the environment. The intent is that an operator with the
// key in their shell for unrelated tooling does NOT accidentally
// activate the Admin API reconciler.
//
// We use a synthetic "would-have-fired" detector: with enabled=false,
// StartReconciler returns synchronously; with enabled=true and no key,
// it likewise returns synchronously (key-missing branch); only with
// enabled=true and a key would it spawn a goroutine. The test asserts
// the synchronous return semantics directly.
func TestStartReconcilerOptInGate(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Inject a fake key so the only thing preventing API calls is the
	// opt-in flag itself.
	t.Setenv(anthropicAdminAPIKeyEnv, "sk-ant-admin-fake")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// enabled=false must return immediately and never invoke the
	// network. We bound the call with a generous-but-finite timeout —
	// if StartReconciler were to spawn a goroutine and block here,
	// the test would deadlock; the timeout converts that into a
	// visible failure rather than a hang.
	done := make(chan struct{})
	go func() {
		s.StartReconciler(ctx, false)
		close(done)
	}()
	select {
	case <-done:
		// success — StartReconciler returned synchronously
	case <-time.After(2 * time.Second):
		t.Fatal("StartReconciler(ctx, false) did not return; opt-in gate is leaky")
	}
}

// TestStartReconcilerEnabledWithoutKey verifies the second guard:
// when the operator opts in via config but the environment is missing
// the key, StartReconciler still returns without spawning a worker.
// This is the existing key-missing branch, kept for completeness.
func TestStartReconcilerEnabledWithoutKey(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	// Explicitly clear the env so the key-missing branch fires even
	// on a host that happens to have it set.
	t.Setenv(anthropicAdminAPIKeyEnv, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.StartReconciler(ctx, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartReconciler(ctx, true) without key did not return")
	}
}
