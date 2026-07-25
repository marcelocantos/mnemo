// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package boot tracks process-wide startup phases so /health can stay
// up and report useful status while the daemon is still opening the
// store, taking a pre-migration backup, or applying schema.
package boot

import (
	"fmt"
	"sync"
	"time"
)

// Phase is a coarse startup stage.
type Phase string

const (
	// PhaseStarting is the initial state before serve wiring begins.
	PhaseStarting Phase = "starting"
	// PhaseListening means the HTTP listener is up; store may still be closed.
	PhaseListening Phase = "listening"
	// PhaseOpeningStore covers store.New before/around schema work.
	PhaseOpeningStore Phase = "opening_store"
	// PhasePreMigrationBackup is VACUUM INTO + gzip before sqlift.Apply.
	PhasePreMigrationBackup Phase = "pre_migration_backup"
	// PhaseApplyingSchema is sqlift.Apply / post-migration ANALYZE.
	PhaseApplyingSchema Phase = "applying_schema"
	// PhaseStartingWorkers is registry worker bring-up after the store opens.
	PhaseStartingWorkers Phase = "starting_workers"
	// PhaseReady means the default-user store is open and serving tools.
	PhaseReady Phase = "ready"
	// PhaseFailed means default-user open failed fatally.
	PhaseFailed Phase = "failed"
)

// Status is a snapshot of the current boot phase.
type Status struct {
	Phase  Phase
	Detail string
	Since  time.Time
	Err    string // non-empty when PhaseFailed
}

var (
	mu  sync.RWMutex
	cur = Status{Phase: PhaseStarting, Detail: "process starting", Since: time.Now()}
)

// Set records the current phase and optional human detail. Since is
// updated only when the phase changes so elapsed time in /health reflects
// how long the current stage has been running.
func Set(phase Phase, detail string) {
	mu.Lock()
	defer mu.Unlock()
	if cur.Phase != phase {
		cur.Since = time.Now()
	}
	cur.Phase = phase
	cur.Detail = detail
	if phase != PhaseFailed {
		cur.Err = ""
	}
}

// Fail marks startup failed with the given error.
func Fail(err error) {
	mu.Lock()
	defer mu.Unlock()
	if cur.Phase != PhaseFailed {
		cur.Since = time.Now()
	}
	cur.Phase = PhaseFailed
	if err != nil {
		cur.Err = err.Error()
		cur.Detail = err.Error()
	} else {
		cur.Err = "startup failed"
		cur.Detail = cur.Err
	}
}

// Get returns a copy of the current status.
func Get() Status {
	mu.RLock()
	defer mu.RUnlock()
	return cur
}

// Ready reports whether the default-user path has reached PhaseReady.
func Ready() bool {
	return Get().Phase == PhaseReady
}

// Summary is a one-line description for logs and health detail.
func Summary() string {
	st := Get()
	elapsed := time.Since(st.Since).Round(time.Second)
	if st.Detail == "" {
		return fmt.Sprintf("%s (%s)", st.Phase, elapsed)
	}
	return fmt.Sprintf("%s: %s (%s)", st.Phase, st.Detail, elapsed)
}
