// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestStartupCapabilitiesCheckReports exercises the shipped
// startup.capabilities check through BuildDiagRegistry (🎯T154 R4).
//
// The point of the check is that a phase which failed or never resolved
// is visible in production rather than only as a downstream symptom, so
// "it compiles" is not evidence — this runs the real registry and reads
// the real result.
func TestStartupCapabilitiesCheckReports(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(filepath.Join(dir, "t.db"), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// A store on a current schema resolves every capability; wait for the
	// phases so the assertion is about the check, not about timing.
	s.AwaitStartup()

	r := NewRegistry(context.Background(), store.Config{}, dir)
	r.mu.Lock()
	r.stores["default"] = &userEntry{store: s, homeDir: dir}
	r.mu.Unlock()

	rep := r.BuildDiagRegistry("default", time.Now()).Run(context.Background(), true, time.Now())
	var seen bool
	for _, res := range rep.Results {
		if res.Name != "startup.capabilities" {
			continue
		}
		seen = true
		if res.Severity != "ok" {
			t.Errorf("startup.capabilities severity=%s detail=%s", res.Severity, res.Detail)
		}
	}
	if !seen {
		t.Fatal("startup.capabilities is not in the shipped check set")
	}

	// Every declared capability is reported, and reports a state the
	// check knows how to classify.
	report := s.StartupReport()
	if len(report) == 0 {
		t.Fatal("StartupReport is empty on a booted store")
	}
	for _, c := range report {
		switch c.State {
		case "available", "unavailable", "pending":
		default:
			t.Errorf("capability %s has unknown state %q", c.Name, c.State)
		}
		if c.State == "unavailable" && c.Reason == "" {
			t.Errorf("capability %s is unavailable with no reason", c.Name)
		}
	}
}
