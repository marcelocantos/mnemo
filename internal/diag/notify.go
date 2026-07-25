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

// Notifier turns diagnostic Reports into health alerts (🎯T83 / 🎯T86).
//
// It is opt-out: enabled by default and fires only on fail severity
// (config can disable it or widen the threshold). Alerts are deduped per
// check name — a check that stays failing re-notifies only after
// Cooldown — and a check that recovers (fail→ok) notifies once.
//
// Delivery is a single path: when OnAlert is wired (production daemon),
// every decided alert goes there — the multi-purpose native shim
// (notifications, optional menu-bar chrome, dashboard) is the sole
// presenter. There is no "shim connected?" branch and no parallel
// osascript path for health. When OnAlert is nil (unit tests, or a
// build that never wired a consumer), send is a last-resort fallback
// for test observability only.
type Notifier struct {
	mu sync.Mutex

	enabled      bool
	threshold    Severity
	cooldown     time.Duration
	dashboardURL string
	send         func(title, body string)

	// onAlert, when set, is the sole production delivery path for decided
	// alerts (SSE → native shim). (🎯T86)
	onAlert func(Alert)

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
	// Capture the dashboard URL so osSend can deep-link (terminal-notifier
	// -open, or open(1) after osascript). Bare osascript "display
	// notification" is attributed to Script Editor — clicking the banner
	// opens an empty Script Editor, which is useless.
	dash := cfg.DashboardURL
	n.send = func(title, body string) { osSend(title, body, dash) }
	return n
}

// SetSender overrides the OS delivery function (for tests, and the headless
// fallback).
func (n *Notifier) SetSender(send func(title, body string)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.send = send
}

// OnAlert registers the sole production consumer for decided alerts (SSE →
// native shim). When set, every emit goes here; the OS send path is only
// used if OnAlert is nil. (🎯T86)
func (n *Notifier) OnAlert(fn func(Alert)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onAlert = fn
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

// emit delivers a decided alert (threshold/dedup/cooldown already passed).
// Production always uses onAlert (shim path). send is only the nil-OnAlert
// fallback for tests. Every emit is also logged at WARN (fail) or INFO
// (recovery) so the daemon log is a durable record independent of UI.
func (n *Notifier) emit(a Alert) {
	via := "shim"
	if n.onAlert != nil {
		n.onAlert(a)
	} else {
		via = "fallback"
		if a.Kind == "recovery" {
			n.send(fmt.Sprintf("mnemo: %s recovered", a.Name), "This check is healthy again.")
		} else {
			n.send(fmt.Sprintf("mnemo: %s %s", a.Name, a.Severity), n.alertBody(a))
		}
	}
	logHealthAlert(a, via)
}

// logHealthAlert writes a durable line for every notification decision so a
// transient OS banner is not the only evidence of a health transition.
func logHealthAlert(a Alert, via string) {
	attrs := []any{
		"check", a.Name,
		"kind", a.Kind,
		"severity", a.Severity,
		"via", via,
	}
	if a.Detail != "" {
		attrs = append(attrs, "detail", a.Detail)
	}
	if a.Remediation != "" {
		attrs = append(attrs, "remediation", a.Remediation)
	}
	if a.DashboardURL != "" {
		attrs = append(attrs, "dashboard", a.DashboardURL)
	}
	if a.Kind == "recovery" {
		slog.Info("diag: health alert", attrs...)
		return
	}
	slog.Warn("diag: health alert", attrs...)
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
//
// openURL, when non-empty, is the deep-link target (usually the dashboard
// #health page). On macOS we prefer terminal-notifier -open so a click
// opens that URL; plain osascript notifications are attributed to Script
// Editor and a click opens an empty Script Editor window — so the
// osascript path also runs `open <url>` as a usable fallback.
func osSend(title, body, openURL string) {
	switch runtime.GOOS {
	case "darwin":
		oneLine := strings.ReplaceAll(body, "\n", " — ")
		if tn, err := exec.LookPath("terminal-notifier"); err == nil {
			args := []string{
				"-title", title,
				"-message", oneLine,
				"-group", "mnemo-health",
			}
			if openURL != "" {
				args = append(args, "-open", openURL)
			}
			runNotify(tn, args...)
			return
		}
		// AppleScript notification bodies are single-line; collapse
		// newlines. %q produces a valid double-quoted AppleScript literal
		// for normal text (escapes embedded quotes/backslashes), and the
		// osascript -e arg keeps it clear of the shell.
		script := fmt.Sprintf("display notification %q with title %q", oneLine, title)
		runNotify("osascript", "-e", script)
		// Clicking an osascript banner opens Script Editor (empty). Open
		// the dashboard so the deep-link is actually actionable.
		if openURL != "" {
			runNotify("open", openURL)
		}
	case "linux":
		runNotify("notify-send", title, body)
		// notify-send has no portable click-action without a desktop
		// entry; leave the URL in the body for copy/paste.
	}
}

func runNotify(name string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		// WARN: the user may have only the OS banner as a cue; if delivery
		// itself fails we still need a durable breadcrumb.
		slog.Warn("diag: notification delivery failed", "cmd", name, "err", err)
	}
}
