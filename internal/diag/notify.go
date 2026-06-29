// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Notifier turns diagnostic Reports into OS-level notifications (🎯T83).
//
// It is opt-out: enabled by default and fires only on fail severity
// (config can disable it or widen the threshold). Notifications are
// deduped per check name — a check that stays failing re-notifies only
// after Cooldown — and a check that recovers (fail→ok) notifies once. The
// body carries a deep-link to the dashboard health page so the user can
// jump straight to remediation.
//
// Delivery is local-only and best-effort: macOS osascript, Linux
// notify-send. No network, nothing cached — safe in environments where
// outbound calls require review. send is injectable for tests.
type Notifier struct {
	mu sync.Mutex

	enabled      bool
	threshold    Severity
	cooldown     time.Duration
	dashboardURL string
	send         func(title, body string)

	// onAlert, when set, receives structured alerts so a richer consumer (the
	// native menu-bar shim) can format them itself. shimPresent gates it: an
	// alert routes to onAlert only when a shim is actually connected, else it
	// falls back to send (osascript/notify-send). Both nil → today's behaviour
	// (always send), which the tests rely on. (🎯T86)
	onAlert     func(Alert)
	shimPresent func() bool

	lastSeverity map[string]Severity
	lastNotified map[string]time.Time
}

// Alert is a health transition worth surfacing: a check that newly crossed the
// threshold ("fail") or a previously-failing check that recovered ("recovery").
// It carries everything the shim needs to render a native notification without
// calling back to the daemon.
type Alert struct {
	Name        string `json:"name"`
	Severity    string `json:"severity"` // ok / warn / fail
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
	Kind        string `json:"kind"` // "fail" | "recovery"
	// DashboardURL deep-links the dashboard health page (the shim may open its
	// native panel instead).
	DashboardURL string `json:"dashboard_url,omitempty"`
}

// NotifierConfig configures a Notifier. The zero value is a disabled
// notifier; use DefaultNotifierConfig for the opt-out default.
type NotifierConfig struct {
	Enabled      bool
	Threshold    Severity      // notify when a check's severity >= this (default Fail)
	Cooldown     time.Duration // re-notify a still-failing check after this (default 6h)
	DashboardURL string        // deep-link target, e.g. http://localhost:19419/#health
}

// DefaultNotifierConfig is the opt-out default: enabled, fail-only, 6h
// re-notify cooldown.
func DefaultNotifierConfig(dashboardURL string) NotifierConfig {
	return NotifierConfig{
		Enabled:      true,
		Threshold:    Fail,
		Cooldown:     6 * time.Hour,
		DashboardURL: dashboardURL,
	}
}

// NewNotifier builds a Notifier from cfg. The OS sender is chosen by
// platform; tests can swap it via SetSender.
func NewNotifier(cfg NotifierConfig) *Notifier {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 6 * time.Hour
	}
	n := &Notifier{
		enabled:      cfg.Enabled,
		threshold:    cfg.Threshold,
		cooldown:     cfg.Cooldown,
		dashboardURL: cfg.DashboardURL,
		lastSeverity: map[string]Severity{},
		lastNotified: map[string]time.Time{},
	}
	n.send = osSend
	return n
}

// SetSender overrides the OS delivery function (for tests, and the headless
// fallback).
func (n *Notifier) SetSender(send func(title, body string)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.send = send
}

// OnAlert registers a structured-alert consumer (the native shim path). When
// set and a shim is connected (see SetShimPresent), alerts route here instead
// of to the OS sender. (🎯T86)
func (n *Notifier) OnAlert(fn func(Alert)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onAlert = fn
}

// SetShimPresent supplies the predicate that decides whether a native shim is
// connected. An alert routes to OnAlert only when this returns true; otherwise
// it falls back to the OS sender. (🎯T86)
func (n *Notifier) SetShimPresent(fn func() bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.shimPresent = fn
}

// Observe folds a report into the notifier, emitting notifications for
// checks that newly cross the threshold (or re-cross after the cooldown)
// and a recovery notification when a previously-failing check returns to
// ok. now drives the cooldown.
func (n *Notifier) Observe(report Report, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.enabled {
		return
	}
	for _, res := range report.Results {
		sev := parseSeverity(res.Severity)
		prev := n.lastSeverity[res.Name]
		n.lastSeverity[res.Name] = sev

		switch {
		case sev >= n.threshold:
			// Newly failing, or still failing past the cooldown.
			last, seen := n.lastNotified[res.Name]
			if prev < n.threshold || !seen || now.Sub(last) >= n.cooldown {
				n.emit(Alert{
					Name:         res.Name,
					Severity:     sev.String(),
					Detail:       res.Detail,
					Remediation:  res.Remediation,
					Kind:         "fail",
					DashboardURL: n.dashboardURL,
				})
				n.lastNotified[res.Name] = now
			}
		case prev >= n.threshold:
			// Recovered.
			n.emit(Alert{
				Name:         res.Name,
				Severity:     OK.String(),
				Kind:         "recovery",
				DashboardURL: n.dashboardURL,
			})
			delete(n.lastNotified, res.Name)
		}
	}
}

// emit routes a decided alert (the threshold/dedup/cooldown gate has already
// passed): to the native shim when one is connected, else to the OS sender. The
// OS title/body are preserved verbatim from the pre-T86 behaviour so headless
// notifications (and the notifier tests) are unchanged.
func (n *Notifier) emit(a Alert) {
	if n.onAlert != nil && n.shimPresent != nil && n.shimPresent() {
		n.onAlert(a)
		return
	}
	if a.Kind == "recovery" {
		n.send(fmt.Sprintf("mnemo: %s recovered", a.Name), "This check is healthy again.")
		return
	}
	n.send(fmt.Sprintf("mnemo: %s %s", a.Name, a.Severity), n.alertBody(a))
}

// alertBody composes the OS notification text: the detail plus a remediation
// hint and a deep-link to the dashboard health page.
func (n *Notifier) alertBody(a Alert) string {
	var b strings.Builder
	if a.Detail != "" {
		b.WriteString(a.Detail)
	}
	if a.Remediation != "" {
		fmt.Fprintf(&b, "\nFix: %s", a.Remediation)
	}
	if n.dashboardURL != "" {
		fmt.Fprintf(&b, "\n%s", n.dashboardURL)
	}
	return b.String()
}

func parseSeverity(s string) Severity {
	switch s {
	case "fail":
		return Fail
	case "warn":
		return Warn
	default:
		return OK
	}
}

// osSend delivers a notification through the platform's native mechanism,
// best-effort. Failures are logged at debug and otherwise ignored — a
// missing notifier must never wedge the diagnostics loop.
func osSend(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		// AppleScript notification bodies are single-line; collapse
		// newlines. %q produces a valid double-quoted AppleScript literal
		// for normal text (escapes embedded quotes/backslashes), and the
		// osascript -e arg keeps it clear of the shell.
		oneLine := strings.ReplaceAll(body, "\n", " — ")
		script := fmt.Sprintf("display notification %q with title %q", oneLine, title)
		runNotify("osascript", "-e", script)
	case "linux":
		runNotify("notify-send", title, body)
	}
}

func runNotify(name string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		slog.Debug("diag: notification delivery failed", "err", err)
	}
}
