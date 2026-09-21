// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/diag"
)

// countingDiags records how many times the checks were run live.
type countingDiags struct {
	runs   *int
	report diag.Report
}

func (c countingDiags) Run(context.Context, bool, time.Time) diag.Report {
	*c.runs++
	return c.report
}

type fixedSource struct {
	report diag.Report
	ok     bool
}

func (f fixedSource) Latest() (diag.Report, bool) { return f.report, f.ok }

func getHealth(t *testing.T, h *Handler, url string) (*httptest.ResponseRecorder, diag.Report) {
	t.Helper()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
	var rep diag.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode %s: %v (%s)", url, err, w.Body.String())
	}
	return w, rep
}

// The point of the change: reading health runs nothing.
func TestHealthServesTheSnapshotWithoutRunningChecks(t *testing.T) {
	runs := 0
	h := New(nil)
	h.SetDiagRunner(countingDiags{runs: &runs,
		report: diag.Report{Results: []diag.Result{{Name: "live"}}}})
	h.SetHealthSource(fixedSource{ok: true,
		report: diag.Report{Results: []diag.Result{{Name: "snapshot"}}}})

	w, rep := getHealth(t, h, "/health")
	if runs != 0 {
		t.Fatalf("GET /health ran the checks %d time(s); it should read the snapshot", runs)
	}
	if len(rep.Results) != 1 || rep.Results[0].Name != "snapshot" {
		t.Errorf("served %+v, want the snapshot", rep.Results)
	}
	if got := w.Header().Get("X-Mnemo-Health"); got != "snapshot" {
		t.Errorf("X-Mnemo-Health = %q, want snapshot", got)
	}
}

func TestHealthFreshRunsLive(t *testing.T) {
	runs := 0
	h := New(nil)
	h.SetDiagRunner(countingDiags{runs: &runs,
		report: diag.Report{Results: []diag.Result{{Name: "live"}}}})
	h.SetHealthSource(fixedSource{ok: true,
		report: diag.Report{Results: []diag.Result{{Name: "snapshot"}}}})

	w, rep := getHealth(t, h, "/health?fresh=1")
	if runs != 1 {
		t.Fatalf("?fresh=1 ran the checks %d time(s), want 1", runs)
	}
	if rep.Results[0].Name != "live" {
		t.Errorf("served %+v, want the live run", rep.Results)
	}
	if got := w.Header().Get("X-Mnemo-Health"); got != "live" {
		t.Errorf("X-Mnemo-Health = %q, want live", got)
	}
}

// Before the scheduler's first run finishes there is no snapshot, and
// serving an empty report would read as "no checks exist".
func TestHealthFallsBackToLiveBeforeTheFirstRun(t *testing.T) {
	runs := 0
	h := New(nil)
	h.SetDiagRunner(countingDiags{runs: &runs,
		report: diag.Report{Results: []diag.Result{{Name: "live"}}}})
	h.SetHealthSource(fixedSource{ok: false})

	_, rep := getHealth(t, h, "/health")
	if runs != 1 || rep.Results[0].Name != "live" {
		t.Fatalf("runs=%d served=%+v, want a live fallback", runs, rep.Results)
	}
}
