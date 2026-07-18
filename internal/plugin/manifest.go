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
	"time"
)

// ProtocolVersionCurrent is the only protocol version mnemo accepts
// today (🎯T102.1 contract). Bump when the wire contract breaks.
const ProtocolVersionCurrent = 1

// Manifest is the JSON body of GET …/manifest (🎯T102.1 §3.2).
// Metadata lives here, not in config — config only names transport,
// enable, and params (🎯T102.2).
type Manifest struct {
	ProtocolVersion int            `json:"protocol_version"`
	Name            string         `json:"name"`
	Version         string         `json:"version"`
	Description     string         `json:"description,omitempty"`
	Facets          Facets         `json:"facets"`
	UI              *UISurface     `json:"ui,omitempty"`
	ConfigSchema    map[string]any `json:"config_schema,omitempty"`
	MCP             *MCPSurface    `json:"mcp,omitempty"`
}

// Facets declares which facet endpoints the plugin implements.
type Facets struct {
	Signal    bool `json:"signal"`
	Reconcile bool `json:"reconcile"`
	Check     bool `json:"check"`
	Notify    bool `json:"notify"`
	MCP       bool `json:"mcp"`
}

// UISurface is the optional popup/menu contribution (🎯T102.9).
type UISurface struct {
	Label       string `json:"label"`
	Icon        string `json:"icon,omitempty"`
	PreviewPath string `json:"preview_path,omitempty"`
	PagePath    string `json:"page_path,omitempty"`
	Menu        string `json:"menu,omitempty"`
}

// MCPSurface declares an MCP endpoint to bridge (🎯T102.10).
type MCPSurface struct {
	Transport string `json:"transport,omitempty"`
	Path      string `json:"path,omitempty"`
}

// FetchManifest GETs baseURL/manifest (or baseURL when it already ends
// in /manifest) and decodes the required shape. baseURL is the plugin
// instance root (e.g. http://127.0.0.1:9091), not the mnemo proxy path.
//
// Returns a clear error on dial failure, non-2xx, bad JSON, missing
// required fields, or an unsupported protocol_version. Unknown JSON
// fields are ignored for forward compatibility.
func FetchManifest(ctx context.Context, client *http.Client, baseURL string) (*Manifest, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := strings.TrimRight(baseURL, "/") + "/manifest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build manifest request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read manifest body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("manifest HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks required fields and protocol version.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("manifest is nil")
	}
	if m.ProtocolVersion == 0 {
		return fmt.Errorf("manifest: protocol_version is required")
	}
	if m.ProtocolVersion != ProtocolVersionCurrent {
		return fmt.Errorf("manifest: unsupported protocol_version %d (accepted: %d)", m.ProtocolVersion, ProtocolVersionCurrent)
	}
	if m.Name == "" {
		return fmt.Errorf("manifest: name is required")
	}
	if m.Version == "" {
		return fmt.Errorf("manifest: version is required")
	}
	return nil
}

// DefaultHTTPClient returns a short-timeout client for plugin probes.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
