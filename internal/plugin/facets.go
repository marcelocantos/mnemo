// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
)

// FacetHTTPTimeout bounds every facet HTTP call so a hung plugin cannot
// wedge the single scheduler or reconciler dispatcher (🎯T102.7 / 🎯T84).
const FacetHTTPTimeout = 10 * time.Second

// DefaultReconcileInterval is the StreamReconciler cadence hint for
// plugin reconcile facets.
const DefaultReconcileInterval = time.Minute

// CheckBody is the JSON shape of GET …/check (🎯T102.1 §3.3).
type CheckBody struct {
	Severity    string `json:"severity"` // ok | warn | fail
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// ReconcileBody is the optional JSON request for POST …/reconcile.
type ReconcileBody struct {
	Now string `json:"now"` // RFC3339
}

// ReconcileResult is the optional JSON response from POST …/reconcile.
type ReconcileResult struct {
	Changed int `json:"changed"`
}

// NotifyPayload is the JSON body POSTed to …/notify (title/body/url).
type NotifyPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
}

// ReconcileAdapter implements store.StreamReconciler by POSTing the
// plugin's /reconcile endpoint (🎯T102.7). Name is plugin.<name>.reconcile.
// A short HTTP timeout isolates a slow/broken plugin from the dispatcher;
// the registry worker's T84 breaker further backs off after consecutive
// failures.
type ReconcileAdapter struct {
	name     string
	baseURL  string
	client   *http.Client
	interval time.Duration
	timeout  time.Duration
}

// Name implements store.StreamReconciler.
func (a *ReconcileAdapter) Name() string {
	return "plugin." + a.name + ".reconcile"
}

// Interval implements store.StreamReconciler.
func (a *ReconcileAdapter) Interval() time.Duration {
	if a.interval <= 0 {
		return DefaultReconcileInterval
	}
	return a.interval
}

// Reconcile POSTs {BaseURL}/reconcile with a short-timeout context.
// Returns changed count from the JSON body (0 when absent or empty).
func (a *ReconcileAdapter) Reconcile(ctx context.Context, now time.Time) (int, error) {
	if a == nil || a.baseURL == "" {
		return 0, fmt.Errorf("plugin reconcile: no base URL")
	}
	timeout := a.timeout
	if timeout <= 0 {
		timeout = FacetHTTPTimeout
	}
	client := a.client
	if client == nil {
		client = DefaultHTTPClient()
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(ReconcileBody{Now: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return 0, fmt.Errorf("plugin reconcile encode: %w", err)
	}
	url := strings.TrimRight(a.baseURL, "/") + "/reconcile"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("plugin reconcile request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("plugin %q reconcile: %w", a.name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, fmt.Errorf("plugin %q reconcile read: %w", a.name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("plugin %q reconcile HTTP %d: %s", a.name, resp.StatusCode, truncate(string(raw), 200))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return 0, nil
	}
	var result ReconcileResult
	if err := json.Unmarshal(raw, &result); err != nil {
		// Non-JSON 2xx bodies are tolerated (plugin may return "ok").
		return 0, nil
	}
	return result.Changed, nil
}

// StreamReconcilers returns one StreamReconciler per ready plugin that
// declares Facets.Reconcile. Refreshed on each call so hot-reload
// enable/disable is reflected without re-wiring the registry worker.
func (m *Manager) StreamReconcilers() []store.StreamReconciler {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	type item struct {
		name    string
		baseURL string
	}
	var items []item
	for name, inst := range m.instances {
		if inst.State != StateReady || inst.BaseURL == "" || inst.Manifest == nil {
			continue
		}
		if !inst.Manifest.Facets.Reconcile {
			continue
		}
		items = append(items, item{name: name, baseURL: inst.BaseURL})
	}
	client := m.client
	m.mu.Unlock()

	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	out := make([]store.StreamReconciler, 0, len(items))
	for _, it := range items {
		out = append(out, &ReconcileAdapter{
			name:     it.name,
			baseURL:  it.baseURL,
			client:   client,
			interval: DefaultReconcileInterval,
			timeout:  FacetHTTPTimeout,
		})
	}
	return out
}

// FacetChecks returns one Fast-tier diag.Check per ready plugin that
// declares Facets.Check. Each check GETs {BaseURL}/check and maps the
// JSON body onto diag.CheckResult. HTTP failures and timeouts surface
// as Fail so they ride the existing T83 scheduler and T84-adjacent
// isolation (short timeout; a hung plugin never blocks the diag pass
// past FacetHTTPTimeout).
func (m *Manager) FacetChecks() []diag.Check {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	names := make([]string, 0, len(m.instances))
	for name, inst := range m.instances {
		if inst.State != StateReady || inst.BaseURL == "" || inst.Manifest == nil {
			continue
		}
		if !inst.Manifest.Facets.Check {
			continue
		}
		names = append(names, name)
	}
	m.mu.Unlock()
	sort.Strings(names)

	out := make([]diag.Check, 0, len(names))
	for _, name := range names {
		n := name
		out = append(out, diag.Check{
			Name: "plugin." + n + ".check",
			Tier: diag.Fast,
			Run: func(ctx context.Context) diag.CheckResult {
				return m.runCheckFacet(ctx, n)
			},
		})
	}
	return out
}

// DynamicChecks returns ready probes plus check-facet checks so a single
// diag.SetDynamic provider covers both T102.3 and T102.7 surfaces.
func (m *Manager) DynamicChecks() []diag.Check {
	if m == nil {
		return nil
	}
	ready := m.DiagChecks()
	facet := m.FacetChecks()
	if len(facet) == 0 {
		return ready
	}
	out := make([]diag.Check, 0, len(ready)+len(facet))
	out = append(out, ready...)
	out = append(out, facet...)
	return out
}

func (m *Manager) runCheckFacet(ctx context.Context, name string) diag.CheckResult {
	snap, ok := m.Get(name)
	if !ok || snap.State != StateReady || snap.BaseURL == "" {
		return diag.Warning(
			fmt.Sprintf("plugin %q check facet skipped: not ready", name),
			"wait for the plugin to attach, or disable facets.check in its manifest")
	}

	client := m.client
	if client == nil {
		client = DefaultHTTPClient()
	}
	ctx, cancel := context.WithTimeout(ctx, FacetHTTPTimeout)
	defer cancel()

	url := strings.TrimRight(snap.BaseURL, "/") + "/check"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return diag.Failure(
			fmt.Sprintf("plugin %q check: build request: %v", name, err),
			"report a bug in mnemo facet adapters")
	}
	resp, err := client.Do(req)
	if err != nil {
		return diag.Failure(
			fmt.Sprintf("plugin %q check: %v", name, err),
			"ensure the plugin process is healthy and serves GET /check within "+FacetHTTPTimeout.String())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return diag.Failure(
			fmt.Sprintf("plugin %q check: read body: %v", name, err),
			"inspect the plugin logs")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return diag.Failure(
			fmt.Sprintf("plugin %q check HTTP %d: %s", name, resp.StatusCode, truncate(string(raw), 200)),
			"fix the plugin /check handler or disable facets.check")
	}
	var body CheckBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return diag.Failure(
			fmt.Sprintf("plugin %q check: invalid JSON: %v", name, err),
			"return {severity, detail, remediation?} from GET /check")
	}
	return mapCheckBody(name, body)
}

func mapCheckBody(name string, body CheckBody) diag.CheckResult {
	detail := body.Detail
	if detail == "" {
		detail = fmt.Sprintf("plugin %q check", name)
	}
	rem := body.Remediation
	if rem == "" {
		rem = "inspect plugin " + name + " logs and /check output"
	}
	switch strings.ToLower(strings.TrimSpace(body.Severity)) {
	case "", "ok", "healthy":
		return diag.Healthy(detail)
	case "warn", "warning":
		return diag.Warning(detail, rem)
	case "fail", "failure", "error", "critical":
		return diag.Failure(detail, rem)
	default:
		return diag.Warning(
			fmt.Sprintf("plugin %q check: unknown severity %q (%s)", name, body.Severity, detail),
			"use severity ok|warn|fail in GET /check")
	}
}

// Notify POSTs payload to the named plugin's /notify endpoint when the
// instance is ready and declares Facets.Notify. Returns nil when the
// plugin is not ready or does not expose notify (no-op for the sink).
func (m *Manager) Notify(ctx context.Context, name string, payload NotifyPayload) error {
	if m == nil {
		return fmt.Errorf("plugin manager is nil")
	}
	snap, ok := m.Get(name)
	if !ok {
		return fmt.Errorf("plugin %q not tracked", name)
	}
	if snap.State != StateReady || snap.BaseURL == "" {
		return fmt.Errorf("plugin %q not ready", name)
	}
	if snap.Manifest == nil || !snap.Manifest.Facets.Notify {
		return fmt.Errorf("plugin %q does not declare notify facet", name)
	}
	return postNotify(ctx, m.client, snap.BaseURL, name, payload)
}

// NotifyAll fans out payload to every ready plugin with Facets.Notify.
// Individual failures are collected; the first error is returned after
// all attempts so one broken plugin does not skip the rest.
func (m *Manager) NotifyAll(ctx context.Context, payload NotifyPayload) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	type target struct {
		name    string
		baseURL string
	}
	var targets []target
	for name, inst := range m.instances {
		if inst.State != StateReady || inst.BaseURL == "" || inst.Manifest == nil {
			continue
		}
		if !inst.Manifest.Facets.Notify {
			continue
		}
		targets = append(targets, target{name: name, baseURL: inst.BaseURL})
	}
	client := m.client
	m.mu.Unlock()

	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	var first error
	for _, t := range targets {
		if err := postNotify(ctx, client, t.baseURL, t.name, payload); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func postNotify(ctx context.Context, client *http.Client, baseURL, name string, payload NotifyPayload) error {
	if client == nil {
		client = DefaultHTTPClient()
	}
	ctx, cancel := context.WithTimeout(ctx, FacetHTTPTimeout)
	defer cancel()

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("plugin notify encode: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/notify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("plugin %q notify request: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plugin %q notify: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("plugin %q notify HTTP %d: %s", name, resp.StatusCode, truncate(string(body), 200))
	}
	return nil
}
