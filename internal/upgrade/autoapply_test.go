// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestApplyEnvNotifyOnly(t *testing.T) {
	t.Parallel()
	if !(ApplyEnv{Homebrew: false, GOOS: "darwin"}).NotifyOnly() {
		t.Fatal("non-homebrew should notify-only")
	}
	if !(ApplyEnv{Homebrew: true, GOOS: "windows"}).NotifyOnly() {
		t.Fatal("windows should notify-only")
	}
	if (ApplyEnv{Homebrew: true, GOOS: "darwin"}).NotifyOnly() {
		t.Fatal("homebrew darwin may apply")
	}
}

func TestOrchestratorDisabledAndNotifyOnly(t *testing.T) {
	t.Parallel()
	d := NewDetector(&DetectorArgs{
		CurrentVersion: "0.61.0",
		Fetch:          func(ctx context.Context) (string, error) { return "v0.62.0", nil },
	})
	o := NewOrchestrator(&OrchestratorArgs{
		Enabled:  false,
		Env:      ApplyEnv{Homebrew: true, GOOS: "darwin"},
		Detector: d,
	})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.Phase() != PhaseDisabled {
		t.Fatalf("phase %s", o.Phase())
	}

	o2 := NewOrchestrator(&OrchestratorArgs{
		Enabled:  true,
		Env:      ApplyEnv{Homebrew: false, GOOS: "darwin"},
		Detector: d,
	})
	if err := o2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o2.Phase() != PhaseNotifyOnly {
		t.Fatalf("phase %s", o2.Phase())
	}
}

func TestOrchestratorFullStateMachine(t *testing.T) {
	t.Parallel()
	var clock time.Time
	clock = time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	lastAct := clock // recent activity
	d := NewDetector(&DetectorArgs{
		CurrentVersion: "0.61.0",
		Fetch:          func(ctx context.Context) (string, error) { return "v0.62.0", nil },
		Now:            now,
		MinInterval:    time.Millisecond,
	})
	var steps []string
	var noticeFrom, noticeTo string
	o := NewOrchestrator(&OrchestratorArgs{
		Enabled:      true,
		Env:          ApplyEnv{Homebrew: true, GOOS: "darwin"},
		Quiescence:   30 * time.Minute,
		Detector:     d,
		LastActivity: func() time.Time { return lastAct },
		Apply: func(ctx context.Context) error {
			steps = append(steps, "apply")
			return nil
		},
		SpawnBackend: func(ctx context.Context) error {
			steps = append(steps, "spawn")
			return nil
		},
		FlipPrimary: func(ctx context.Context) error {
			steps = append(steps, "flip")
			return nil
		},
		DrainOld: func(ctx context.Context) error {
			steps = append(steps, "drain")
			return nil
		},
		OnUpgrade: func(from, to string) {
			noticeFrom, noticeTo = from, to
			steps = append(steps, "notice")
		},
		Now: now,
	})

	// Tick 1: discover upgrade
	if err := o.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.Phase() != PhaseAvailable {
		t.Fatalf("want available, got %s", o.Phase())
	}

	// Still busy — stay available
	if err := o.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.Phase() != PhaseAvailable {
		t.Fatalf("still available while busy, got %s", o.Phase())
	}

	// Quiesce
	clock = clock.Add(31 * time.Minute)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.Phase() != PhaseQuiescent {
		t.Fatalf("want quiescent, got %s", o.Phase())
	}

	// Apply through done
	if err := o.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o.Phase() != PhaseDone {
		t.Fatalf("want done, got %s hist=%v", o.Phase(), o.History())
	}
	// OnUpgrade before spawn so the sibling can LoadAndConsumePending.
	wantSteps := "apply,notice,spawn,flip,drain"
	if got := strings.Join(steps, ","); got != wantSteps {
		t.Fatalf("steps %s want %s", got, wantSteps)
	}
	if noticeFrom == "" || noticeTo == "" {
		t.Fatal("onUpgrade not called")
	}
}
