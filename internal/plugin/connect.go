// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ReadyBody is the JSON shape of GET …/ready (🎯T102.1 §3.1).
// Plugins may return {"ok": true}; any 2xx without a body is also ok.
type ReadyBody struct {
	OK bool `json:"ok"`
}

// AttachResult is the uniform outcome of bringing a plugin instance up
// against a known base URL (🎯T102.3). Launch (T102.4) and in-process
// (T102.6) transports produce the same shape once they have a base URL.
type AttachResult struct {
	BaseURL  string
	Manifest *Manifest
}

// ProbeReady GETs baseURL/ready. Success is HTTP 2xx with either an empty
// body or JSON {"ok": true}. Any other status, dial error, or ok:false
// is a clear failure so diag can surface it.
func ProbeReady(ctx context.Context, client *http.Client, baseURL string) error {
	if client == nil {
		client = DefaultHTTPClient()
	}
	url := strings.TrimRight(baseURL, "/") + "/ready"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build ready request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ready probe: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read ready body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ready HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}
	var rb ReadyBody
	if err := json.Unmarshal(body, &rb); err != nil {
		// Non-JSON 2xx bodies are tolerated (some servers return "ok").
		return nil
	}
	if !rb.OK {
		return fmt.Errorf("ready reported ok=false")
	}
	return nil
}

// AttachConnect attaches to an already-running plugin server at baseURL:
// probes readiness, fetches and validates the manifest (protocol version),
// and checks that the manifest name matches configName when both are set.
//
// This is the connect-mode path (🎯T102.3). Launch and in-process
// transports call the same AttachResult shape after they obtain a base URL.
func AttachConnect(ctx context.Context, client *http.Client, baseURL, configName string) (*AttachResult, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("base URL is empty")
	}
	if err := ProbeReady(ctx, client, base); err != nil {
		return nil, err
	}
	man, err := FetchManifest(ctx, client, base)
	if err != nil {
		return nil, err
	}
	if configName != "" && man.Name != configName {
		return nil, fmt.Errorf("manifest name %q does not match config name %q", man.Name, configName)
	}
	return &AttachResult{BaseURL: base, Manifest: man}, nil
}
