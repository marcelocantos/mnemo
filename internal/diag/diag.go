// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package diag is mnemo's self-diagnostics subsystem (🎯T83).
//
// A Registry holds a set of named health Checks, each tagged with a Tier
// (Fast or Full). Run executes them and returns a Report of per-check
// Results — a severity (ok/warn/fail), a human-facing detail, and a
// remediation hint. The same Report drives three surfaces: the
// mnemo_ops op=doctor MCP tool (on-demand full run), the dashboard health page
// (the /health endpoint), and OS notifications (on a transition into
// fail). Startup runs the full set once; a timer runs Fast checks
// frequently and the Full set hourly.
//
// Checks are plain closures that capture whatever state they inspect
// (the store, the compaction watcher, breakers, config), so the
// subsystem stays decoupled from the things it observes.
package diag

import (
	"context"
	"fmt"
	"time"
)

// Severity is a check outcome, ordered ok < warn < fail.
type Severity int

const (
	OK Severity = iota
	Warn
	Fail
)

// String returns the stable lowercase name used in JSON and the tool.
func (s Severity) String() string {
	switch s {
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "ok"
	}
}

// Tier controls how often a check runs. Fast checks are cheap (read
// counters / a quick stat or SQL) and run on the frequent timer pass;
// Full checks may be expensive (filesystem walks, integrity scans,
// convergence recomputation) and run at startup, hourly, and on demand.
type Tier int

const (
	Fast Tier = iota
	Full
)

// String returns the stable lowercase tier name.
func (t Tier) String() string {
	if t == Full {
		return "full"
	}
	return "fast"
}

// CheckResult is what a check's Run func returns: a severity plus
// human-facing detail and (for warn/fail) a remediation hint. Use the
// Healthy / Warning / Failure constructors.
type CheckResult struct {
	Severity    Severity
	Detail      string
	Remediation string
}

// Healthy reports an ok result with an optional descriptive detail.
func Healthy(detail string) CheckResult { return CheckResult{Severity: OK, Detail: detail} }

// Warning reports a warn result with a detail and a remediation hint.
func Warning(detail, remediation string) CheckResult {
	return CheckResult{Severity: Warn, Detail: detail, Remediation: remediation}
}

// Failure reports a fail result with a detail and a remediation hint.
func Failure(detail, remediation string) CheckResult {
	return CheckResult{Severity: Fail, Detail: detail, Remediation: remediation}
}

// CheckFunc runs a single check. It must be cheap when its Check is Fast.
// It should never panic — Run recovers and reports a fail if it does —
// but should prefer returning a Failure with detail over panicking.
type CheckFunc func(ctx context.Context) CheckResult

// Check is a named, tiered health check.
type Check struct {
	Name string
	Tier Tier
	Run  CheckFunc
}

// Result is a check's outcome enriched with its identity for the Report
// (and the dashboard / tool / notifications). Severity and Tier are the
// stable string forms. DurationMs is wall time of the check itself so a
// /health regression is visible in the payload without a separate probe.
type Result struct {
	Name        string `json:"name"`
	Severity    string `json:"severity"`
	Tier        string `json:"tier"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
}

// Report is the outcome of running a set of checks at a point in time.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	OK          int       `json:"ok"`
	Warn        int       `json:"warn"`
	Fail        int       `json:"fail"`
	Results     []Result  `json:"results"`
}

// Worst returns the most severe severity across the report's results
// (OK when empty). Callers use this to decide whether to notify.
func (r Report) Worst() Severity {
	worst := OK
	for _, res := range r.Results {
		var s Severity
		switch res.Severity {
		case "fail":
			s = Fail
		case "warn":
			s = Warn
		}
		if s > worst {
			worst = s
		}
	}
	return worst
}

// DynamicProvider returns additional checks evaluated at Run time.
// Used for hot-reloadable surfaces (e.g. plugin.<name>.ready, 🎯T102.3)
// whose membership changes without rebuilding the registry.
type DynamicProvider func() []Check

// Registry holds the registered checks. Build one with NewRegistry,
// Register checks at startup, then Run it from the scheduler, the
// mnemo_ops(op=doctor) tool, and the /health endpoint.
type Registry struct {
	checks  []Check
	dynamic DynamicProvider // optional; expanded on every Run
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds checks to the registry. Not safe for concurrent use with
// Run; call it only during startup wiring before the scheduler starts.
func (r *Registry) Register(checks ...Check) {
	r.checks = append(r.checks, checks...)
}

// SetDynamic installs a provider of checks that are expanded on every
// Run. Safe to call once at startup after Register. The provider itself
// must be safe for concurrent use with the subsystem it observes.
func (r *Registry) SetDynamic(p DynamicProvider) {
	r.dynamic = p
}

// Checks returns the registered static checks (read-only view). Dynamic
// checks are not included — call Run to observe them.
func (r *Registry) Checks() []Check { return r.checks }

// Run executes the registered checks and returns a Report stamped at now.
// When full is false only Fast-tier checks run (the frequent timer pass);
// when true every check runs (startup, hourly, on-demand). A check that
// panics is reported as a fail rather than crashing the run.
//
// Dynamic checks from SetDynamic are appended after static ones each Run,
// so plugin enable/disable via hot-reload is visible without re-wiring.
func (r *Registry) Run(ctx context.Context, full bool, now time.Time) Report {
	rep := Report{GeneratedAt: now}
	all := r.checks
	if r.dynamic != nil {
		if dyn := r.dynamic(); len(dyn) > 0 {
			all = append(append([]Check{}, r.checks...), dyn...)
		}
	}
	for _, c := range all {
		if !full && c.Tier == Full {
			continue
		}
		res := runOne(ctx, c)
		switch res.Severity {
		case "fail":
			rep.Fail++
		case "warn":
			rep.Warn++
		default:
			rep.OK++
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

// runOne runs a single check with panic recovery and maps it to a Result.
// CheckTimeout bounds a single check. The report is assembled
// sequentially, so without it one slow check holds the whole /health
// response — and has: a Fast-tier check running a full-table blob scan
// took the endpoint to 95s on a large store. A check that cannot answer
// inside this budget is reported as a failed check, which is information,
// rather than being allowed to delay every other check's answer.
//
// Generous on purpose. This is a backstop against pathology, not a
// performance target; a check that legitimately needs longer than this
// is a check that should be moved to the Full tier.
var CheckTimeout = 20 * time.Second

// CheckTimeoutForTest sets the per-check bound and returns the previous
// value. Tests only.
func CheckTimeoutForTest(d time.Duration) time.Duration {
	prev := CheckTimeout
	CheckTimeout = d
	return prev
}

func runOne(ctx context.Context, c Check) (res Result) {
	res = Result{Name: c.Name, Tier: c.Tier.String(), Severity: Fail.String()}
	start := time.Now()
	defer func() {
		res.DurationMs = time.Since(start).Milliseconds()
		if r := recover(); r != nil {
			res.Severity = Fail.String()
			res.Detail = "check panicked"
			res.Remediation = "file a mnemo bug — a diagnostic check should never panic"
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	done := make(chan CheckResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Re-panic on the caller's goroutine so the deferred
				// recover above still turns it into a fail result.
				done <- CheckResult{Severity: Fail,
					Detail:      "check panicked",
					Remediation: "file a mnemo bug — a diagnostic check should never panic"}
			}
		}()
		done <- c.Run(ctx)
	}()

	var cr CheckResult
	select {
	case cr = <-done:
	case <-ctx.Done():
		// Deliberately no second look at done. A cancelled context
		// unblocks any check that waits on it, so an answer arriving now
		// is a consequence of the timeout rather than despite it, and
		// crediting it would report a wedged check as healthy. Both cases
		// are only ready together once the deadline has genuinely passed,
		// so reporting the timeout is the honest verdict either way.
		//
		// The check's own goroutine is left to unwind on the cancelled
		// context; the report does not wait for it.
		return Result{
			Name:     c.Name,
			Tier:     c.Tier.String(),
			Severity: Fail.String(),
			Detail: fmt.Sprintf("check did not answer within %s",
				CheckTimeout),
			Remediation: "this check is doing more work than a health probe should; " +
				"look for a full-table scan, and move it to the Full tier if it genuinely needs the time",
		}
	}
	res = Result{
		Name:        c.Name,
		Tier:        c.Tier.String(),
		Severity:    cr.Severity.String(),
		Detail:      cr.Detail,
		Remediation: cr.Remediation,
	}
	return res
}
