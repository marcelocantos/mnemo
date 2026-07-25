// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/diag"
)

type stubDiags struct {
	report diag.Report
}

func (s stubDiags) Run(context.Context, bool, time.Time) diag.Report { return s.report }

func TestWantsHTML(t *testing.T) {
	cases := []struct {
		name   string
		accept string
		query  string
		want   bool
	}{
		{name: "curl default", accept: "*/*", want: false},
		{name: "empty accept", accept: "", want: false},
		{name: "browser", accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", want: true},
		{name: "explicit json", accept: "application/json", want: false},
		{name: "json and html prefers json", accept: "application/json, text/html;q=0.9", want: false},
		{name: "html preferred over json", accept: "text/html, application/json;q=0.8", want: true},
		{name: "format html override", accept: "*/*", query: "format=html", want: true},
		{name: "format json override", accept: "text/html", query: "format=json", want: false},
		{name: "dashboard fetch", accept: "application/json", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := "/health"
			if tc.query != "" {
				url += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			if got := wantsHTML(req); got != tc.want {
				t.Fatalf("wantsHTML=%v want %v (Accept=%q query=%q)", got, tc.want, tc.accept, tc.query)
			}
		})
	}
}

func TestHealthContentNegotiation(t *testing.T) {
	report := diag.Report{
		GeneratedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		OK:          1,
		Warn:        1,
		Fail:        0,
		Results: []diag.Result{
			{Name: "db.wal", Severity: "warn", Tier: "fast", Detail: "WAL is large (582 MiB)", Remediation: "restart if growing"},
			{Name: "startup.ready", Severity: "ok", Tier: "fast", Detail: "default-user store ready"},
		},
	}
	h := &Handler{diags: stubDiags{report: report}}

	t.Run("json default", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rr := httptest.NewRecorder()
		h.health(rr, req)
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("Content-Type=%q", ct)
		}
		if !strings.Contains(rr.Body.String(), `"name":"db.wal"`) {
			t.Fatalf("body missing json: %s", rr.Body.String())
		}
	})

	t.Run("html browser", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
		rr := httptest.NewRecorder()
		h.health(rr, req)
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("Content-Type=%q", ct)
		}
		body := rr.Body.String()
		for _, want := range []string{
			"mnemo health",
			"Degraded",
			"db.wal",
			"WAL is large",
			"Fix:",
			"restart if growing",
			"startup.ready",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("HTML missing %q\n%s", want, body)
			}
		}
		// Escaping: no raw angle injection from details (details are plain here).
		if strings.Contains(body, "<script>alert") {
			t.Fatal("unexpected script injection")
		}
	})

	t.Run("format html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health?format=html", nil)
		rr := httptest.NewRecorder()
		h.health(rr, req)
		if !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("Content-Type=%q", rr.Header().Get("Content-Type"))
		}
	})
}

func TestRenderHealthHTMLEscapes(t *testing.T) {
	html := renderHealthHTML(diag.Report{
		GeneratedAt: time.Now().UTC(),
		Fail:        1,
		Results: []diag.Result{{
			Name:        "x",
			Severity:    "fail",
			Detail:      `<script>alert(1)</script>`,
			Remediation: `rm -rf / & <b>`,
		}},
	})
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("detail not escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("expected escaped detail: %s", html)
	}
	if !strings.Contains(html, "&lt;b&gt;") {
		t.Fatalf("expected escaped remediation: %s", html)
	}
}

func TestHealthUnavailableHTML(t *testing.T) {
	h := &Handler{} // diags nil
	req := httptest.NewRequest(http.MethodGet, "/health?format=html", nil)
	rr := httptest.NewRecorder()
	h.health(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "diag registry not yet wired") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}
