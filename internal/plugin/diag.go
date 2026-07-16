// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/marcelocantos/mnemo/internal/diag"
	"github.com/marcelocantos/mnemo/internal/store"
)

// DiagChecks returns one Fast-tier T83 check per tracked plugin instance
// (🎯T102.3): name plugin.<name>.ready. Closures read live Manager state
// so hot-reload enable/disable is reflected without re-registering.
//
// Callers that need a stable set of check names across reconcile should
// use DynamicChecks instead (registered once via diag.SetDynamic).
func (m *Manager) DiagChecks() []diag.Check {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	names := make([]string, 0, len(m.instances))
	for name := range m.instances {
		names = append(names, name)
	}
	m.mu.Unlock()
	sort.Strings(names)

	out := make([]diag.Check, 0, len(names))
	for _, name := range names {
		n := name
		out = append(out, diag.Check{
			Name: "plugin." + n + ".ready",
			Tier: diag.Fast,
			Run: func(ctx context.Context) diag.CheckResult {
				return m.checkOne(ctx, n)
			},
		})
	}
	return out
}

// DynamicChecks implements the live-expanding diag provider so every
// currently tracked plugin appears as plugin.<name>.ready on each
// mnemo_doctor / /health / scheduler pass, including plugins added
// after daemon startup via mnemo_config hot-reload.
func (m *Manager) DynamicChecks() []diag.Check {
	return m.DiagChecks()
}

func (m *Manager) checkOne(ctx context.Context, name string) diag.CheckResult {
	_ = ctx
	snap, ok := m.Get(name)
	if !ok {
		return diag.Warning(
			fmt.Sprintf("plugin %q is no longer tracked", name),
			"remove stale plugin config or restart mnemo")
	}
	if !snap.Enabled {
		return diag.Healthy(fmt.Sprintf("plugin %q disabled", name))
	}
	switch snap.State {
	case StateReady:
		ver := ""
		if snap.Manifest != nil {
			ver = " v" + snap.Manifest.Version
		}
		return diag.Healthy(fmt.Sprintf("plugin %q ready%s at %s", name, ver, snap.BaseURL))
	case StateConfigured:
		return diag.Warning(
			fmt.Sprintf("plugin %q is configured (%s) but not yet attached — transport pending", name, snap.Transport),
			"in-process transport lands in T102.6; use transport=launch or transport=connect for now")
	case StateStarting:
		return diag.Warning(
			fmt.Sprintf("plugin %q is still starting", name),
			"wait for the attach to finish; if stuck, check the plugin process and URL")
	case StateStopped:
		return diag.Healthy(fmt.Sprintf("plugin %q stopped", name))
	case StateError:
		detail := fmt.Sprintf("plugin %q not ready", name)
		if snap.BaseURL != "" {
			detail += " at " + snap.BaseURL
		}
		if snap.Err != "" {
			detail += ": " + snap.Err
		}
		return diag.Failure(detail, pluginRemediation(snap))
	default:
		return diag.Warning(
			fmt.Sprintf("plugin %q in unknown state %q", name, snap.State),
			"report a bug; restart mnemo")
	}
}

func pluginRemediation(snap Snapshot) string {
	switch {
	case strings.Contains(snap.Err, "protocol_version"):
		return "upgrade the plugin or mnemo so protocol_version matches (currently 1)"
	case strings.Contains(snap.Err, "does not match config name"):
		return "set plugins[].name to the plugin's manifest name, or fix the plugin manifest"
	case strings.Contains(snap.Err, "circuit breaker"):
		return "fix the plugin process; mnemo stopped restarting after repeated failures — disable/re-enable the plugin or restart mnemo after the cooldown"
	case snap.Transport == store.PluginTransportConnect:
		return "ensure the plugin process is listening at " + snap.BaseURL +
			" and serves GET /ready and GET /manifest; check firewall and URL in config"
	case snap.Transport == store.PluginTransportLaunch:
		return "check plugins[].command exists and is executable; the child must print " +
			"'MNEMO_PLUGIN_PORT <port>' on stdout then serve GET /ready and GET /manifest on 127.0.0.1"
	default:
		return "inspect plugins entry in ~/.mnemo/config.json and the plugin logs"
	}
}
