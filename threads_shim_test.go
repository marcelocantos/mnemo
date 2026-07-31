// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

// TestShimSupervisorNoAppIsInert verifies the safety properties of the
// multi-purpose shim supervisor without launching anything: when no
// Mnemo.app is resolved (app == ""), run() returns immediately rather
// than hanging or shelling out. Covers the non-darwin / app-not-installed
// path.
func TestShimSupervisorNoAppIsInert(t *testing.T) {
	s := &shimSupervisor{wake: make(chan struct{}, 1)} // app == "" → inert

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
