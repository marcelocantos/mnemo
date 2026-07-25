// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/diag"
)

// wantsHTML reports whether the client prefers a human-readable HTML
// health page over JSON. Defaults stay JSON for curl, scripts, and the
// dashboard's fetch() (Accept: */*). Browsers send text/html first.
//
// Overrides:
//
//	?format=html  → HTML
//	?format=json  → JSON
func wantsHTML(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format"))) {
	case "html":
		return true
	case "json":
		return false
	}

	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	// Explicit JSON preference wins even when text/html is also listed.
	if prefersJSON(accept) {
		return false
	}
	return strings.Contains(accept, "text/html")
}

// prefersJSON is true when application/json appears with q higher than
// (or equal to, when text/html is absent) text/html, or when JSON is
// listed and HTML is not.
func prefersJSON(accept string) bool {
	jsonQ, hasJSON := acceptQuality(accept, "application/json")
	htmlQ, hasHTML := acceptQuality(accept, "text/html")
	if !hasJSON {
		return false
	}
	if !hasHTML {
		return true
	}
	return jsonQ >= htmlQ
}

// acceptQuality returns the q-value for mediaType in an Accept header
// (default 1.0 when present without q). has is false when the type is
// absent. */* is ignored so bare Accept: */* stays JSON.
func acceptQuality(accept, mediaType string) (q float64, has bool) {
	for _, part := range strings.Split(accept, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		params := strings.Split(part, ";")
		typ := strings.TrimSpace(params[0])
		if !strings.EqualFold(typ, mediaType) {
			continue
		}
		q = 1.0
		for _, p := range params[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(strings.ToLower(p), "q=") {
				var parsed float64
				if _, err := fmt.Sscanf(p[2:], "%f", &parsed); err == nil {
					q = parsed
				}
			}
		}
		return q, true
	}
	return 0, false
}

func writeHealthHTML(w http.ResponseWriter, report diag.Report, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(renderHealthHTML(report)))
}

func renderHealthHTML(report diag.Report) string {
	worst := report.Worst()
	statusLabel := "Healthy"
	statusClass := "ok"
	switch worst {
	case diag.Fail:
		statusLabel = "Unhealthy"
		statusClass = "fail"
	case diag.Warn:
		statusLabel = "Degraded"
		statusClass = "warn"
	}

	results := append([]diag.Result(nil), report.Results...)
	sort.SliceStable(results, func(i, j int) bool {
		oi, oj := severityRank(results[i].Severity), severityRank(results[j].Severity)
		if oi != oj {
			return oi < oj
		}
		return results[i].Name < results[j].Name
	})

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>mnemo health — `)
	b.WriteString(html.EscapeString(statusLabel))
	b.WriteString(`</title>
<style>
  :root {
    --bg: #0f1117; --bg2: #1a1d27; --bg3: #21263a; --border: #2a2f45;
    --text: #c9d1e0; --text-dim: #6b7894; --text-bright: #e8eaf6;
    --ok: #3dd68c; --warn: #f7874f; --fail: #f75f5f; --accent: #4f8ef7;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --bg: #f0f2f8; --bg2: #fff; --bg3: #e8eaf0; --border: #d0d4e0;
      --text: #333a50; --text-dim: #7a829a; --text-bright: #1a2030;
      --ok: #16a34a; --warn: #ea580c; --fail: #dc2626; --accent: #2563eb;
    }
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    background: var(--bg); color: var(--text);
    font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
    font-size: 15px; line-height: 1.45; min-height: 100vh; padding: 24px 16px 48px;
  }
  .wrap { max-width: 720px; margin: 0 auto; }
  header { margin-bottom: 20px; }
  .brand {
    display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap;
    margin-bottom: 8px;
  }
  .brand h1 { font-size: 1.25rem; color: var(--text-bright); font-weight: 650; }
  .brand a { color: var(--accent); text-decoration: none; font-size: 0.9rem; }
  .brand a:hover { text-decoration: underline; }
  .status {
    display: inline-flex; align-items: center; gap: 8px;
    font-size: 1.05rem; font-weight: 600; color: var(--text-bright);
  }
  .status .dot {
    width: 10px; height: 10px; border-radius: 50%; background: var(--ok);
  }
  .status.warn .dot { background: var(--warn); }
  .status.fail .dot { background: var(--fail); }
  .meta {
    color: var(--text-dim); font-size: 0.85rem; margin-top: 6px;
  }
  .counts {
    display: flex; gap: 10px; flex-wrap: wrap; margin: 16px 0 20px;
  }
  .count {
    background: var(--bg2); border: 1px solid var(--border);
    border-radius: 8px; padding: 8px 14px; min-width: 88px;
  }
  .count .n { font-size: 1.4rem; font-weight: 700; font-variant-numeric: tabular-nums; }
  .count .l { font-size: 0.72rem; text-transform: uppercase; letter-spacing: 0.06em; color: var(--text-dim); }
  .count.ok .n { color: var(--ok); }
  .count.warn .n { color: var(--warn); }
  .count.fail .n { color: var(--fail); }
  .check {
    background: var(--bg2); border: 1px solid var(--border);
    border-left-width: 4px; border-radius: 8px; padding: 12px 14px;
    margin-bottom: 10px;
  }
  .check.ok { border-left-color: var(--ok); }
  .check.warn { border-left-color: var(--warn); }
  .check.fail { border-left-color: var(--fail); }
  .check-head {
    display: flex; justify-content: space-between; gap: 12px;
    align-items: baseline; flex-wrap: wrap;
  }
  .check-name { font-weight: 600; color: var(--text-bright); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.92rem; }
  .badges { display: flex; gap: 6px; flex-wrap: wrap; }
  .badge {
    font-size: 0.7rem; text-transform: uppercase; letter-spacing: 0.05em;
    padding: 2px 7px; border-radius: 999px; border: 1px solid var(--border);
    color: var(--text-dim); background: var(--bg3);
  }
  .badge.ok { color: var(--ok); border-color: color-mix(in srgb, var(--ok) 40%, var(--border)); }
  .badge.warn { color: var(--warn); border-color: color-mix(in srgb, var(--warn) 40%, var(--border)); }
  .badge.fail { color: var(--fail); border-color: color-mix(in srgb, var(--fail) 40%, var(--border)); }
  .detail { margin-top: 8px; color: var(--text); white-space: pre-wrap; word-break: break-word; }
  .remediation {
    margin-top: 10px; padding: 8px 10px; border-radius: 6px;
    background: var(--bg3); color: var(--text-dim); font-size: 0.9rem;
  }
  .remediation strong { color: var(--text); font-weight: 600; }
  footer { margin-top: 28px; color: var(--text-dim); font-size: 0.8rem; }
  footer code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
</head>
<body>
<div class="wrap">
<header>
  <div class="brand">
    <h1>mnemo health</h1>
    <a href="/">dashboard</a>
  </div>
  <div class="status `)
	b.WriteString(statusClass)
	b.WriteString(`"><span class="dot" aria-hidden="true"></span>`)
	b.WriteString(html.EscapeString(statusLabel))
	b.WriteString(`</div>
  <div class="meta">Checked `)
	b.WriteString(html.EscapeString(formatCheckedAt(report.GeneratedAt)))
	b.WriteString(` · auto-refreshes every 15s</div>
</header>

<div class="counts" aria-label="summary">
  <div class="count fail"><div class="n">`)
	fmt.Fprintf(&b, "%d", report.Fail)
	b.WriteString(`</div><div class="l">fail</div></div>
  <div class="count warn"><div class="n">`)
	fmt.Fprintf(&b, "%d", report.Warn)
	b.WriteString(`</div><div class="l">warn</div></div>
  <div class="count ok"><div class="n">`)
	fmt.Fprintf(&b, "%d", report.OK)
	b.WriteString(`</div><div class="l">ok</div></div>
</div>
`)

	if len(results) == 0 {
		b.WriteString(`<p class="meta">No checks registered yet.</p>`)
	}
	for _, res := range results {
		sev := res.Severity
		if sev == "" {
			sev = "ok"
		}
		b.WriteString(`<article class="check `)
		b.WriteString(html.EscapeString(sev))
		b.WriteString(`">
  <div class="check-head">
    <div class="check-name">`)
		b.WriteString(html.EscapeString(res.Name))
		b.WriteString(`</div>
    <div class="badges">
      <span class="badge `)
		b.WriteString(html.EscapeString(sev))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(sev))
		b.WriteString(`</span>`)
		if res.Tier != "" {
			b.WriteString(`<span class="badge">`)
			b.WriteString(html.EscapeString(res.Tier))
			b.WriteString(`</span>`)
		}
		b.WriteString(`
    </div>
  </div>`)
		if res.Detail != "" {
			b.WriteString(`
  <div class="detail">`)
			b.WriteString(html.EscapeString(res.Detail))
			b.WriteString(`</div>`)
		}
		if strings.TrimSpace(res.Remediation) != "" {
			b.WriteString(`
  <div class="remediation"><strong>Fix:</strong> `)
			b.WriteString(html.EscapeString(res.Remediation))
			b.WriteString(`</div>`)
		}
		b.WriteString(`
</article>
`)
	}

	b.WriteString(`
<footer>
  Machine-readable: <code>GET /health</code> with <code>Accept: application/json</code>
  or <code>?format=json</code>. This page: <code>?format=html</code>.
</footer>
</div>
<script>setTimeout(function(){ location.reload(); }, 15000);</script>
</body>
</html>
`)
	return b.String()
}

func severityRank(s string) int {
	switch s {
	case "fail":
		return 0
	case "warn":
		return 1
	default:
		return 2
	}
}

func formatCheckedAt(t time.Time) string {
	if t.IsZero() {
		return "just now"
	}
	// Local wall clock is more natural in a browser than UTC Zulu.
	return t.Local().Format("15:04:05 MST · 2006-01-02")
}
