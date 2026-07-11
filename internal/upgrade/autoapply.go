// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// DefaultQuiescence is the MCP-idle window before auto-apply (🎯T97.5).
const DefaultQuiescence = 30 * time.Minute

// ApplyEnv describes install environment constraints for auto-apply.
type ApplyEnv struct {
	// Homebrew is true when the binary is a Homebrew install.
	Homebrew bool
	// GOOS overrides runtime.GOOS in tests (empty → runtime.GOOS).
	GOOS string
}

// NotifyOnly reports whether this environment must not auto-apply
// (non-Homebrew or Windows).
func (e ApplyEnv) NotifyOnly() bool {
	goos := e.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "windows" {
		return true
	}
	return !e.Homebrew
}

// Phase is one step of the auto-apply state machine.
type Phase string

const (
	PhaseIdle       Phase = "idle"
	PhaseAvailable  Phase = "available"
	PhaseQuiescent  Phase = "quiescent"
	PhaseApplying   Phase = "applying"
	PhaseFlipping   Phase = "flipping"
	PhaseDraining   Phase = "draining"
	PhaseDone       Phase = "done"
	PhaseNotifyOnly Phase = "notify_only"
	PhaseDisabled   Phase = "disabled"
)

// OrchestratorArgs configures auto-apply orchestration (🎯T97.5).
type OrchestratorArgs struct {
	Enabled    bool
	Env        ApplyEnv
	Quiescence time.Duration
	Detector   *Detector
	// LastActivity returns the time of the most recent MCP traffic.
	LastActivity func() time.Time
	// Apply installs the new binary (e.g. brew upgrade mnemo). Optional
	// in tests; when nil, Apply step is a no-op success.
	Apply func(ctx context.Context) error
	// SpawnBackend starts the new backend process. Optional.
	SpawnBackend func(ctx context.Context) error
	// FlipPrimary points new initializes at the new backend. Required
	// for a full apply; tests inject fakes.
	FlipPrimary func(ctx context.Context) error
	// DrainOld stops the previous backend after flip. Optional.
	DrainOld func(ctx context.Context) error
	// OnUpgrade is best-effort tools/list_changed + session notices.
	OnUpgrade func(from, to string)
	Now       func() time.Time
}

// Orchestrator drives opt-in auto-apply after quiescence.
type Orchestrator struct {
	mu           sync.Mutex
	enabled      bool
	env          ApplyEnv
	quiescence   time.Duration
	detector     *Detector
	lastActivity func() time.Time
	apply        func(ctx context.Context) error
	spawn        func(ctx context.Context) error
	flip         func(ctx context.Context) error
	drain        func(ctx context.Context) error
	onUpgrade    func(from, to string)
	now          func() time.Time

	phase   Phase
	fromVer string
	toVer   string
	// history of phases entered (tests).
	history []Phase
}

// NewOrchestrator builds an Orchestrator.
func NewOrchestrator(args *OrchestratorArgs) *Orchestrator {
	if args == nil {
		args = &OrchestratorArgs{}
	}
	q := args.Quiescence
	if q <= 0 {
		q = DefaultQuiescence
	}
	now := args.Now
	if now == nil {
		now = time.Now
	}
	last := args.LastActivity
	if last == nil {
		last = func() time.Time { return time.Time{} }
	}
	o := &Orchestrator{
		enabled:      args.Enabled,
		env:          args.Env,
		quiescence:   q,
		detector:     args.Detector,
		lastActivity: last,
		apply:        args.Apply,
		spawn:        args.SpawnBackend,
		flip:         args.FlipPrimary,
		drain:        args.DrainOld,
		onUpgrade:    args.OnUpgrade,
		now:          now,
		phase:        PhaseIdle,
	}
	if !args.Enabled {
		o.phase = PhaseDisabled
	} else if args.Env.NotifyOnly() {
		o.phase = PhaseNotifyOnly
	}
	return o
}

// SetEnabled toggles auto-apply (config hot-reload).
func (o *Orchestrator) SetEnabled(enabled bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.enabled = enabled
	if !enabled {
		o.phase = PhaseDisabled
	} else if o.env.NotifyOnly() {
		o.phase = PhaseNotifyOnly
	} else if o.phase == PhaseDisabled || o.phase == PhaseNotifyOnly {
		o.phase = PhaseIdle
	}
}

// Phase returns the current state machine phase.
func (o *Orchestrator) Phase() Phase {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.phase
}

// History returns phases entered (test helper).
func (o *Orchestrator) History() []Phase {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Phase, len(o.history))
	copy(out, o.history)
	return out
}

func (o *Orchestrator) enter(p Phase) {
	o.phase = p
	o.history = append(o.history, p)
}

// Tick advances the state machine one step. Safe to call on a timer.
func (o *Orchestrator) Tick(ctx context.Context) error {
	o.mu.Lock()
	enabled := o.enabled
	env := o.env
	phase := o.phase
	detector := o.detector
	quiescence := o.quiescence
	lastActivity := o.lastActivity
	now := o.now
	o.mu.Unlock()

	if !enabled {
		o.mu.Lock()
		o.enter(PhaseDisabled)
		o.mu.Unlock()
		return nil
	}
	if env.NotifyOnly() {
		// Detection still runs elsewhere; apply path is a no-op.
		o.mu.Lock()
		o.enter(PhaseNotifyOnly)
		o.mu.Unlock()
		return nil
	}
	if detector == nil {
		return fmt.Errorf("upgrade: orchestrator has no detector")
	}

	switch phase {
	case PhaseIdle, PhaseDisabled, PhaseNotifyOnly, PhaseDone:
		cr := detector.Check(ctx)
		if cr.Err != nil {
			return cr.Err
		}
		if !cr.UpgradeAvailable {
			o.mu.Lock()
			o.enter(PhaseIdle)
			o.mu.Unlock()
			return nil
		}
		snap := detector.Snapshot()
		o.mu.Lock()
		o.fromVer = snap.Current
		o.toVer = cr.Latest
		o.enter(PhaseAvailable)
		o.mu.Unlock()
		return nil

	case PhaseAvailable:
		idleFor := now().Sub(lastActivity())
		if lastActivity().IsZero() || idleFor >= quiescence {
			o.mu.Lock()
			o.enter(PhaseQuiescent)
			o.mu.Unlock()
		}
		return nil

	case PhaseQuiescent:
		o.mu.Lock()
		o.enter(PhaseApplying)
		apply := o.apply
		spawn := o.spawn
		from, to := o.fromVer, o.toVer
		onUp := o.onUpgrade
		o.mu.Unlock()
		if apply != nil {
			if err := apply(ctx); err != nil {
				o.mu.Lock()
				o.enter(PhaseAvailable)
				o.mu.Unlock()
				return fmt.Errorf("apply: %w", err)
			}
		}
		// Write pending notices BEFORE spawn so the sibling loads them
		// at startup (LoadAndConsumePending). Also lets the old process
		// mark in-process banners for sessions it still serves.
		if onUp != nil {
			onUp(from, to)
		}
		if spawn != nil {
			if err := spawn(ctx); err != nil {
				o.mu.Lock()
				o.enter(PhaseAvailable)
				o.mu.Unlock()
				return fmt.Errorf("spawn backend: %w", err)
			}
		}
		o.mu.Lock()
		o.enter(PhaseFlipping)
		flip := o.flip
		o.mu.Unlock()
		if flip != nil {
			if err := flip(ctx); err != nil {
				o.mu.Lock()
				o.enter(PhaseAvailable)
				o.mu.Unlock()
				return fmt.Errorf("flip primary: %w", err)
			}
		}
		o.mu.Lock()
		o.enter(PhaseDraining)
		drain := o.drain
		o.mu.Unlock()
		if drain != nil {
			if err := drain(ctx); err != nil {
				return fmt.Errorf("drain old: %w", err)
			}
		}
		o.mu.Lock()
		o.enter(PhaseDone)
		o.mu.Unlock()
		return nil

	case PhaseApplying, PhaseFlipping, PhaseDraining:
		// Intermediate phases advance inside PhaseQuiescent; if we land
		// here from a concurrent tick, no-op.
		return nil
	}
	return nil
}
