// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func rep(results ...Result) Report {
	r := Report{}
	for _, res := range results {
		switch res.Severity {
		case "fail":
			r.Fail++
		case "warn":
			r.Warn++
		default:
			r.OK++
		}
		r.Results = append(r.Results, res)
	}
	return r
}

func TestNotifierTransitionsAndCooldown(t *testing.T) {
	n := NewNotifier(DefaultNotifierConfig("http://x/#health"))
	var sent []string
	n.SetSender(func(title, _ string) { sent = append(sent, title) })

	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	fail := Result{Name: "compactor.workdir", Severity: "fail", Detail: "missing", Remediation: "restart"}

	n.Observe(rep(fail), base) // newly failing → notify
	if len(sent) != 1 {
		t.Fatalf("first fail: got %d notifications, want 1", len(sent))
	}
	n.Observe(rep(fail), base.Add(time.Hour)) // still failing, within 6h → silent
	if len(sent) != 1 {
		t.Fatalf("re-notified within cooldown: %d", len(sent))
	}
	n.Observe(rep(fail), base.Add(7*time.Hour)) // past cooldown → re-notify
	if len(sent) != 2 {
		t.Fatalf("no re-notify past cooldown: %d", len(sent))
	}
	n.Observe(rep(Result{Name: "compactor.workdir", Severity: "ok"}), base.Add(8*time.Hour)) // recovered
	if len(sent) != 3 {
		t.Fatalf("no recovery notification: %d", len(sent))
	}
}

// When OnAlert is wired, every decided alert goes there — never the OS
// fallback. There is no subscriber/presence gate. (🎯T86)
func TestNotifierRoutesToOnAlert(t *testing.T) {
	n := NewNotifier(DefaultNotifierConfig("http://x/#health"))
	var sends int
	var alerts []Alert
	n.SetSender(func(string, string) { sends++ })
	n.OnAlert(func(a Alert) { alerts = append(alerts, a) })

	base := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	fail := Result{Name: "claude.path", Severity: "fail", Detail: "missing", Remediation: "install"}

	n.Observe(rep(fail), base)
	if len(alerts) != 1 || sends != 0 {
		t.Fatalf("OnAlert path: alerts=%d sends=%d, want 1/0", len(alerts), sends)
	}
	if a := alerts[0]; a.Name != "claude.path" || a.Severity != "fail" || a.Kind != "fail" ||
		a.Detail != "missing" || a.Remediation != "install" || a.DashboardURL != "http://x/#health" {
		t.Fatalf("alert payload mismatch: %+v", a)
	}

	// Second check: still OnAlert only, even with no "presence" concept.
	n.Observe(rep(Result{Name: "db.readable", Severity: "fail"}), base)
	if sends != 0 || len(alerts) != 2 {
		t.Fatalf("still OnAlert only: sends=%d alerts=%d, want 0/2", sends, len(alerts))
	}
}

func TestNotifierDisabledAndThreshold(t *testing.T) {
	off := NewNotifier(NotifierConfig{Enabled: false, Threshold: Fail})
	var a int
	off.SetSender(func(string, string) { a++ })
	off.Observe(rep(Result{Name: "x", Severity: "fail"}), time.Now())
	if a != 0 {
		t.Error("disabled notifier delivered")
	}

	on := NewNotifier(DefaultNotifierConfig(""))
	var b int
	on.SetSender(func(string, string) { b++ })
	on.Observe(rep(Result{Name: "y", Severity: "warn"}), time.Now())
	if b != 0 {
		t.Error("warn notified at fail threshold")
	}
}

// logHealthAlert is pure: fail → WARN, recovery → INFO, attrs carry the
// check identity. Captured via a temporary default logger so a missing
// log line is a regression of the forensics path that left db.readable
// notifications untraceable in the daemon log.
func TestLogHealthAlertLevels(t *testing.T) {
	var buf strings.Builder
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logHealthAlert(Alert{
		Name: "db.readable", Kind: "fail", Severity: "fail",
		Detail: "the database is not responding", Remediation: "restart",
		DashboardURL: "http://localhost:19419/#health",
	}, "os")
	logHealthAlert(Alert{
		Name: "db.readable", Kind: "recovery", Severity: "ok",
	}, "shim")

	out := buf.String()
	if !strings.Contains(out, `level=WARN`) || !strings.Contains(out, `check=db.readable`) {
		t.Fatalf("fail alert not logged at WARN with check name:\n%s", out)
	}
	if !strings.Contains(out, `kind=fail`) || !strings.Contains(out, `detail=`) {
		t.Fatalf("fail alert missing kind/detail:\n%s", out)
	}
	if !strings.Contains(out, `level=INFO`) || !strings.Contains(out, `kind=recovery`) {
		t.Fatalf("recovery alert not logged at INFO:\n%s", out)
	}
	if !strings.Contains(out, `via=os`) || !strings.Contains(out, `via=shim`) {
		t.Fatalf("via attribute missing:\n%s", out)
	}
}
