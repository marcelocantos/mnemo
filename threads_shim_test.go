// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

// TestShimSupervisorNoAppIsInert verifies the safety properties of the
// menu-bar shim supervisor without launching anything: when no Mnemo.app is
// resolved (app == ""), run() returns immediately rather than hanging or
// shelling out, and SetEnabled never blocks even when toggled repeatedly
// against a full wake channel. This covers the non-darwin / app-not-installed
// path and the hot-reload poke being fire-and-forget.
func TestShimSupervisorNoAppIsInert(t *testing.T) {
	s := &shimSupervisor{wake: make(chan struct{}, 1)} // app == "" → inert

	// SetEnabled must be non-blocking even when the wake channel is already
	// full (the second/third calls hit the default case).
	s.SetEnabled(true)
	s.SetEnabled(false)
	s.SetEnabled(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return for an app-less supervisor")
	}
}
