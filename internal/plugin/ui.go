// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"path"
	"sort"
	"strings"
)

// UIContribution is one enabled plugin's menu/preview surface for the
// native popup (🎯T102.9). Paths are absolute on the mnemo origin
// (/plugins/<name>/…) so the shim can load them same-origin without
// learning the child port.
type UIContribution struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Icon        string `json:"icon,omitempty"`
	PreviewURL  string `json:"preview_url,omitempty"`
	PageURL     string `json:"page_url,omitempty"`
	Menu        string `json:"menu,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// EventPublisher fans typed events to the T86 SSE hub (or any test
// spy). Type names match the SSE event: field (e.g. "plugin.reload").
type EventPublisher func(typ string, data any)

// SetEventPublisher installs the SSE publisher used for plugin.reload
// and similar (🎯T102.9). Safe to call once at startup.
func (m *Manager) SetEventPublisher(p EventPublisher) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.publish = p
	m.mu.Unlock()
}

// UIContributions returns data-driven menu entries for every ready
// plugin that declared a ui block in its manifest. Sorted by name.
func (m *Manager) UIContributions() []UIContribution {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UIContribution, 0)
	for _, inst := range m.instances {
		if c, ok := uiContributionOf(inst); ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func uiContributionOf(inst *Instance) (UIContribution, bool) {
	if inst == nil || !inst.Entry.Enabled || inst.State != StateReady {
		return UIContribution{}, false
	}
	if inst.Manifest == nil || inst.Manifest.UI == nil {
		return UIContribution{}, false
	}
	ui := inst.Manifest.UI
	label := ui.Label
	if label == "" {
		label = inst.Name
	}
	c := UIContribution{
		Name:        inst.Name,
		Label:       label,
		Icon:        ui.Icon,
		Menu:        ui.Menu,
		PreviewURL:  pluginPublicURL(inst.Name, ui.PreviewPath),
		PageURL:     pluginPublicURL(inst.Name, ui.PagePath),
		Version:     inst.Manifest.Version,
		Description: inst.Manifest.Description,
	}
	return c, true
}

// pluginPublicURL builds /plugins/<name>/<rel> with clean path joining.
// Empty rel yields /plugins/<name>/ (the plugin root).
func pluginPublicURL(name, rel string) string {
	base := "/plugins/" + name + "/"
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return base
	}
	return path.Join("/plugins", name, rel)
}

func (m *Manager) emitReload(name string) {
	if m == nil || m.publish == nil {
		return
	}
	// Publish outside the lock if possible — callers typically hold mu.
	// EventHub.Publish is non-blocking, so publishing under lock is fine.
	m.publish("plugin.reload", map[string]string{"name": name})
}
