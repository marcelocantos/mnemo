// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestDrainIntakeDoesNotHangOnALongLivedRequest is the regression guard
// for the half of 🎯T122 that made shutdown deterministic-broken.
//
// mnemo serves /mcp over the streamable-HTTP transport, which holds an
// open request for the life of the session. http.Server.Shutdown waits
// for in-flight requests, so with any MCP client attached it never
// returns — 11 of 11 shutdowns in one session hit the deadline and
// force-exited, and the WAL checkpoint that runs after intake stops
// never executed once.
//
// The handler below never returns, standing in for that session.
func TestDrainIntakeDoesNotHangOnALongLivedRequest(t *testing.T) {
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	defer close(releaseHandler)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		close(handlerEntered)
		<-releaseHandler // never returns before the drain
	})
	srv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()

	// Attach a client that keeps its request open, exactly as an MCP
	// session does.
	go func() {
		//nolint:bodyclose // the response never completes; that is the point
		_, _ = http.Get(fmt.Sprintf("http://%s/stream", ln.Addr().String()))
	}()
	select {
	case <-handlerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("client never reached the handler; fixture did not attach")
	}

	const grace = 300 * time.Millisecond
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		drainIntake(grace, []intakeStopper{srv}, []intakeCloser{srv})
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		// Must be bounded by the grace, not by the client disconnecting.
		if elapsed > grace+10*time.Second {
			t.Errorf("drainIntake took %s with a %s grace; it waited on the client", elapsed, grace)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("drainIntake hung on a long-lived request — the exact 🎯T122 failure")
	}

	// And the listener is genuinely torn down, not merely "shutting down".
	if _, err := http.Get(fmt.Sprintf("http://%s/stream", ln.Addr().String())); err == nil {
		t.Error("server still accepting connections after drainIntake")
	}
}

// TestDrainIntakeIsPromptWhenIdle: the grace is an upper bound for a
// stuck client, not a delay everyone pays. With nothing attached the
// drain should return immediately.
func TestDrainIntakeIsPromptWhenIdle(t *testing.T) {
	srv := &http.Server{Handler: http.NewServeMux()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()

	const grace = 5 * time.Second
	start := time.Now()
	drainIntake(grace, []intakeStopper{srv}, []intakeCloser{srv})
	if elapsed := time.Since(start); elapsed >= grace {
		t.Errorf("idle drain took %s; it should not wait out the %s grace", elapsed, grace)
	}
}

// TestDrainIntakeToleratesNilAndErrors: the drain runs during shutdown
// and must never panic — a nil federated server (the common case) or a
// server already closed must both be no-ops.
func TestDrainIntakeToleratesNilAndErrors(t *testing.T) {
	srv := &http.Server{Handler: http.NewServeMux()}
	_ = srv.Close() // already closed: Shutdown/Close will error

	done := make(chan struct{})
	go func() {
		defer close(done)
		drainIntake(time.Second, []intakeStopper{srv, nil}, []intakeCloser{srv, nil})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainIntake hung on nil / already-closed servers")
	}
}
