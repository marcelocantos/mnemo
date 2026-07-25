// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"errors"
	"testing"
	"time"
)

func TestSetAdvancesPhaseAndPreservesSinceWithinPhase(t *testing.T) {
	// Isolate from other tests that may have mutated package state.
	Set(PhaseStarting, "test reset")
	st0 := Get()
	if st0.Phase != PhaseStarting {
		t.Fatalf("phase = %s", st0.Phase)
	}

	Set(PhaseListening, "http up")
	st1 := Get()
	if st1.Phase != PhaseListening || st1.Detail != "http up" {
		t.Fatalf("after listen: %+v", st1)
	}
	since1 := st1.Since

	time.Sleep(5 * time.Millisecond)
	Set(PhaseListening, "still listening")
	st2 := Get()
	if !st2.Since.Equal(since1) {
		t.Fatalf("same-phase Set should not reset Since: %v vs %v", st2.Since, since1)
	}
	if st2.Detail != "still listening" {
		t.Fatalf("detail not updated: %q", st2.Detail)
	}

	Set(PhasePreMigrationBackup, "gzipping")
	st3 := Get()
	if !st3.Since.After(since1) {
		t.Fatalf("phase change should advance Since")
	}
	if !Ready() {
		// not ready yet — good
	} else {
		t.Fatal("Ready before PhaseReady")
	}

	Set(PhaseReady, "serving")
	if !Ready() {
		t.Fatal("expected Ready")
	}
	if s := Summary(); s == "" {
		t.Fatal("empty summary")
	}
}

func TestFail(t *testing.T) {
	Set(PhaseOpeningStore, "opening")
	Fail(errors.New("boom"))
	st := Get()
	if st.Phase != PhaseFailed || st.Err != "boom" {
		t.Fatalf("fail status: %+v", st)
	}
	if Ready() {
		t.Fatal("failed should not be ready")
	}
}
