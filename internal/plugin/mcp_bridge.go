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
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// ToolBridge registers/unregisters plugin MCP tools on the mnemo MCP
// server (🎯T102.10). Namespaced as plugin_<name>__<tool>.
type ToolBridge interface {
	// SyncPluginTools replaces all tools for pluginName with tools.
	// call invokes the remote tool by its original (un-namespaced) name.
	SyncPluginTools(pluginName string, tools []mcp.Tool, call func(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error))
	// ClearPluginTools removes every tool previously registered for pluginName.
	ClearPluginTools(pluginName string)
}

// SetToolBridge installs the MCP tool registrar. Called from main once
// the MCP server exists. Optional — without it tools are not surfaced.
func (m *Manager) SetToolBridge(b ToolBridge) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.tools = b
	m.mu.Unlock()
}

// SetOnToolsChanged registers a callback after MCP tool set changes
// (for tools/list_changed notifications).
func (m *Manager) SetOnToolsChanged(fn func()) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onToolsChanged = fn
	m.mu.Unlock()
}

// BridgedTool is a namespaced tool exported for inspection/tests.
type BridgedTool struct {
	Plugin string
	Name   string // full namespaced name: plugin_<p>__<tool>
	Tool   mcp.Tool
}

// NamespacedToolName builds the public tool id for a plugin tool.
func NamespacedToolName(plugin, tool string) string {
	return "plugin_" + plugin + "__" + tool
}

// ParseNamespacedTool splits plugin_<p>__<tool>.
func ParseNamespacedTool(full string) (plugin, tool string, ok bool) {
	if !strings.HasPrefix(full, "plugin_") {
		return "", "", false
	}
	rest := strings.TrimPrefix(full, "plugin_")
	i := strings.Index(rest, "__")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], rest[i+2:], true
}

// syncMCPToolsLocked discovers tools from ready plugins with an MCP
// facet and pushes them through the ToolBridge. Caller may hold m.mu.
func (m *Manager) syncMCPTools() {
	if m == nil {
		return
	}
	m.mu.Lock()
	bridge := m.tools
	client := m.client
	// Snapshot ready MCP plugins.
	type item struct {
		name string
		base string
		path string
	}
	var items []item
	for _, inst := range m.instances {
		if inst.State != StateReady || inst.Manifest == nil || !inst.Manifest.Facets.MCP {
			continue
		}
		path := "mcp"
		if inst.Manifest.MCP != nil && inst.Manifest.MCP.Path != "" {
			path = strings.Trim(inst.Manifest.MCP.Path, "/")
		}
		items = append(items, item{name: inst.Name, base: inst.BaseURL, path: path})
	}
	// Names currently ready with MCP.
	ready := map[string]struct{}{}
	for _, it := range items {
		ready[it.name] = struct{}{}
	}
	// Clear tools for plugins no longer ready (need previous set — track in m.mcpPlugins).
	prev := m.mcpPlugins
	m.mcpPlugins = ready
	m.mu.Unlock()

	if bridge == nil {
		return
	}
	for name := range prev {
		if _, ok := ready[name]; !ok {
			bridge.ClearPluginTools(name)
		}
	}
	for _, it := range items {
		tools, err := listPluginHTTPTools(context.Background(), client, it.base, it.path)
		if err != nil {
			m.log.Warn("plugin MCP list tools failed", "name", it.name, "err", err)
			bridge.ClearPluginTools(it.name)
			continue
		}
		base, path := it.base, it.path
		pluginName := it.name
		bridge.SyncPluginTools(pluginName, tools, func(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
			return callPluginHTTPTool(ctx, client, base, path, name, args)
		})
	}
	if m.onToolsChanged != nil {
		m.onToolsChanged()
	}
}

// listPluginHTTPTools fetches GET {base}/{path}/tools.
// Response: {"tools":[{"name":"...","description":"...","inputSchema":{...}}]}
func listPluginHTTPTools(ctx context.Context, client *http.Client, base, path string) ([]mcp.Tool, error) {
	if client == nil {
		client = DefaultHTTPClient()
	}
	url := strings.TrimRight(base, "/") + "/" + path + "/tools"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tools HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var env struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	out := make([]mcp.Tool, 0, len(env.Tools))
	for _, t := range env.Tools {
		if t.Name == "" {
			continue
		}
		tool := mcp.Tool{Name: t.Name, Description: t.Description}
		if t.InputSchema != nil {
			raw, _ := json.Marshal(t.InputSchema)
			_ = json.Unmarshal(raw, &tool.InputSchema)
		}
		out = append(out, tool)
	}
	return out, nil
}

func callPluginHTTPTool(ctx context.Context, client *http.Client, base, path, name string, args map[string]any) (*mcp.CallToolResult, error) {
	if client == nil {
		client = DefaultHTTPClient()
	}
	payload, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	url := strings.TrimRight(base, "/") + "/" + path + "/call"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Bound call time.
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req = req.WithContext(cctx)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("plugin tool HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))), nil
	}
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool   `json:"isError"`
		Text    string `json:"text"` // convenience single-text
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// Treat raw body as text result.
		return mcp.NewToolResultText(string(body)), nil
	}
	if env.IsError {
		msg := env.Text
		if msg == "" && len(env.Content) > 0 {
			msg = env.Content[0].Text
		}
		return mcp.NewToolResultError(msg), nil
	}
	if env.Text != "" {
		return mcp.NewToolResultText(env.Text), nil
	}
	if len(env.Content) > 0 {
		return mcp.NewToolResultText(env.Content[0].Text), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}

// MCPServerBridge adapts mark3labs MCPServer AddTool/DeleteTools to ToolBridge.
type MCPServerBridge struct {
	mu  sync.Mutex
	add func(tool mcp.Tool, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error))
	del func(names ...string)
	// registered[plugin] = full tool names
	registered map[string][]string
}

// NewMCPServerBridge builds a bridge using add/delete callbacks so main
// can wire without importing plugin into a cycle (main already has both).
func NewMCPServerBridge(
	add func(tool mcp.Tool, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)),
	del func(names ...string),
) *MCPServerBridge {
	return &MCPServerBridge{add: add, del: del, registered: map[string][]string{}}
}

// SyncPluginTools implements ToolBridge.
func (b *MCPServerBridge) SyncPluginTools(pluginName string, tools []mcp.Tool, call func(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.registered[pluginName]; len(old) > 0 {
		b.del(old...)
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		full := NamespacedToolName(pluginName, t.Name)
		orig := t.Name
		t.Name = full
		if t.Description != "" {
			t.Description = "[plugin:" + pluginName + "] " + t.Description
		} else {
			t.Description = "plugin " + pluginName + " tool " + orig
		}
		b.add(t, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if req.Params.Arguments != nil {
				if m, ok := req.Params.Arguments.(map[string]any); ok {
					args = m
				}
			}
			return call(ctx, orig, args)
		})
		names = append(names, full)
	}
	b.registered[pluginName] = names
}

// ClearPluginTools implements ToolBridge.
func (b *MCPServerBridge) ClearPluginTools(pluginName string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.registered[pluginName]; len(old) > 0 {
		b.del(old...)
		delete(b.registered, pluginName)
	}
}
