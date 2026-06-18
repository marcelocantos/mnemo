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

	lastSeverity map[string]Severity
	lastNotified map[string]time.Time
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

// SetSender overrides the delivery function (for tests).
func (n *Notifier) SetSender(send func(title, body string)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.send = send
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
				n.send(
					fmt.Sprintf("mnemo: %s %s", res.Name, sev),
					n.body(res),
				)
				n.lastNotified[res.Name] = now
			}
		case prev >= n.threshold:
			// Recovered.
			n.send(
				fmt.Sprintf("mnemo: %s recovered", res.Name),
				"This check is healthy again.",
			)
			delete(n.lastNotified, res.Name)
		}
	}
}

// body composes the notification text: the detail plus a remediation hint
// and a deep-link to the dashboard health page.
func (n *Notifier) body(res Result) string {
	var b strings.Builder
	if res.Detail != "" {
		b.WriteString(res.Detail)
	}
	if res.Remediation != "" {
		fmt.Fprintf(&b, "\nFix: %s", res.Remediation)
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
