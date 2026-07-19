// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/mark3labs/mcp-go/mcp"
)

type memBridge struct {
	mu    sync.Mutex
	tools map[string][]string // plugin → names
	calls []string
}

func (b *memBridge) SyncPluginTools(pluginName string, tools []mcp.Tool, call func(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tools == nil {
		b.tools = map[string][]string{}
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, NamespacedToolName(pluginName, t.Name))
	}
	b.tools[pluginName] = names
	// Exercise call once if tools present.
	if len(tools) > 0 && call != nil {
		_, _ = call(context.Background(), tools[0].Name, map[string]any{"x": 1})
	}
}

func (b *memBridge) ClearPluginTools(pluginName string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tools, pluginName)
}

func TestMCPBridgeHotReload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ready":
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case r.URL.Path == "/manifest":
			_ = json.NewEncoder(w).Encode(Manifest{
				ProtocolVersion: ProtocolVersionCurrent,
				Name:            "lab",
				Version:         "1",
				Facets:          Facets{MCP: true},
				MCP:             &MCPSurface{Transport: "http", Path: "mcp"},
			})
		case r.URL.Path == "/mcp/tools":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tools": []map[string]any{
					{"name": "echo", "description": "echo args", "inputSchema": map[string]any{"type": "object"}},
				},
			})
		case r.URL.Path == "/mcp/call":
			_ = json.NewEncoder(w).Encode(map[string]any{"text": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	br := &memBridge{}
	m := NewManager(t.TempDir(), srv.Client(), testLogger())
	t.Cleanup(m.Close)
	m.SetToolBridge(br)

	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: true, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	br.mu.Lock()
	names := br.tools["lab"]
	br.mu.Unlock()
	if len(names) != 1 || names[0] != "plugin_lab__echo" {
		t.Fatalf("tools: %v", names)
	}

	// Disable clears tools.
	m.Reconcile(context.Background(), []store.PluginEntry{{
		Name: "lab", Enabled: false, Transport: store.PluginTransportConnect, URL: srv.URL,
	}})
	br.mu.Lock()
	_, still := br.tools["lab"]
	br.mu.Unlock()
	if still {
		t.Fatal("tools should be cleared on disable")
	}
}

func TestNamespacedToolName(t *testing.T) {
	if n := NamespacedToolName("lab", "echo"); n != "plugin_lab__echo" {
		t.Fatal(n)
	}
	p, tool, ok := ParseNamespacedTool("plugin_lab__echo")
	if !ok || p != "lab" || tool != "echo" {
		t.Fatalf("%s %s %v", p, tool, ok)
	}
}
