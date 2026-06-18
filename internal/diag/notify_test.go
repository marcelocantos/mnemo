// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package diag

import (
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
