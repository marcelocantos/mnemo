// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store/fswatch"
)

func TestEvaluateWatchHealth(t *testing.T) {
	cases := []struct {
		name string
		tel  WatchTelemetry
		want string
	}{
		{"not running", WatchTelemetry{}, "warn"},
		{"healthy fsevents", WatchTelemetry{Running: true, Backend: "fsevents", Roots: 3, DirWatches: 3, ProcessOpenFDs: 40}, "ok"},
		{"cap hit", WatchTelemetry{Running: true, Backend: "fsnotify", CapHit: true, ProcessOpenFDs: 100}, "warn"},
		{"fd warn", WatchTelemetry{Running: true, Backend: "fsevents", ProcessOpenFDs: DefaultWatchFDWarn}, "warn"},
		{"fd fail", WatchTelemetry{Running: true, Backend: "fsevents", ProcessOpenFDs: DefaultWatchFDFail}, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sev, _, _ := EvaluateWatchHealth(tc.tel)
			if sev != tc.want {
				t.Fatalf("sev=%s want %s", sev, tc.want)
			}
		})
	}
}

func TestWatchTelemetryLifecycle(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if s.WatchTelemetrySnapshot().Running {
		t.Fatal("expected not running before Watch")
	}
	s.noteWatchStarted("fsevents", 2, 2, false)
	snap := s.WatchTelemetrySnapshot()
	if !snap.Running || snap.Backend != "fsevents" || snap.Roots != 2 {
		t.Fatalf("after start: %+v", snap)
	}
	s.noteWatchEvent()
	s.noteWatchEvent()
	poller := fswatch.NewPollTracker(time.Minute, 10)
	s.noteWatchPoll(3, poller)
	snap = s.WatchTelemetrySnapshot()
	if snap.EventsReceived != 2 || snap.PollTicks != 1 || snap.PollCandidates != 3 {
		t.Fatalf("counters: %+v", snap)
	}
	s.noteWatchStopped()
	if s.WatchTelemetrySnapshot().Running {
		t.Fatal("expected stopped")
	}
}

func TestIngestDiagnosticsIncludesWatch(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.noteWatchStarted("fsevents", 1, 1, false)
	d := s.IngestDiagnostics("")
	if d == nil || d.Watch == nil {
		t.Fatal("expected diagnostics.watch")
	}
	if d.Watch.Backend != "fsevents" || !d.Watch.Running {
		t.Fatalf("watch=%+v", d.Watch)
	}
}
