// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package diag is mnemo's self-diagnostics subsystem (🎯T83).
//
// A Registry holds a set of named health Checks, each tagged with a Tier
// (Fast or Full). Run executes them and returns a Report of per-check
// Results — a severity (ok/warn/fail), a human-facing detail, and a
// remediation hint. The same Report drives three surfaces: the
// mnemo_doctor MCP tool (on-demand full run), the dashboard health page
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
// stable string forms.
type Result struct {
	Name        string `json:"name"`
	Severity    string `json:"severity"`
	Tier        string `json:"tier"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
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
// mnemo_doctor tool, and the /health endpoint.
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
func runOne(ctx context.Context, c Check) (res Result) {
	res = Result{Name: c.Name, Tier: c.Tier.String(), Severity: Fail.String()}
	defer func() {
		if r := recover(); r != nil {
			res.Severity = Fail.String()
			res.Detail = "check panicked"
			res.Remediation = "file a mnemo bug — a diagnostic check should never panic"
		}
	}()
	cr := c.Run(ctx)
	return Result{
		Name:        c.Name,
		Tier:        c.Tier.String(),
		Severity:    cr.Severity.String(),
		Detail:      cr.Detail,
		Remediation: cr.Remediation,
	}
}
