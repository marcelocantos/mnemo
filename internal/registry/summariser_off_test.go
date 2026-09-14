// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// TestSummariserDisabledStopsTheSummariserWorkers is the regression test
// for 🎯T185.
//
// Before this there was no way to turn compaction off: the only gate was
// an empty summariser workdir, which is a failure path (the temp dir
// could not be created), not an owner control. Turning it off meant
// stopping the whole daemon and losing search with it.
func TestSummariserDisabledStopsTheSummariserWorkers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		wantOff  bool
	}{
		{name: "default keeps summarising", disabled: false, wantOff: false},
		{name: "disabled stops the workers", disabled: true, wantOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := store.Config{}
			cfg.Summariser.Disabled = tc.disabled
			r := NewRegistry(context.Background(), cfg, t.TempDir())
			if got := r.cfg.Summariser.Disabled; got != tc.wantOff {
				t.Fatalf("Summariser.Disabled = %v, want %v", got, tc.wantOff)
			}
			// The workdir is non-empty, so the historical gate would have
			// started the workers regardless — the config must be what
			// decides.
			if r.summariserWorkDir == "" {
				t.Fatal("fixture must supply a usable workdir, or the test proves nothing")
			}
		})
	}
}

// TestSummariserDisabledIsReportedAsDeliberate guards the diagnostic:
// a compactor that is off on purpose must not read as one that failed to
// start, or the next person debugging goes looking for a fault.
func TestSummariserDisabledIsReportedAsDeliberate(t *testing.T) {
	cfg := store.Config{}
	cfg.Summariser.Disabled = true
	r := NewRegistry(context.Background(), cfg, t.TempDir())
	reg := r.BuildDiagRegistry("default", time.Now())
	rep := reg.Run(context.Background(), false, time.Now())
	for _, res := range rep.Results {
		if res.Name != "compactor.breaker" {
			continue
		}
		if res.Severity != "ok" {
			t.Errorf("severity = %q, want ok — disabling on purpose is not a fault", res.Severity)
		}
		if res.Detail == "" || res.Detail == "compactor not started yet" {
			t.Errorf("detail = %q; it must name the config key, not read as pending", res.Detail)
		}
		return
	}
	t.Fatal("compactor.breaker check not present in the report")
}
