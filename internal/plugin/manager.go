// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package plugin implements mnemo's plugin registry (🎯T102).
//
// 🎯T102.2 owns config declaration, hot-reload reconcile, and
// manifest-based metadata discovery. 🎯T102.3 owns connect-mode attach:
// ready probe + manifest fetch + protocol validation, producing the
// uniform Instance (base URL + manifest). 🎯T102.4 owns launch-mode:
// spawn executable, stdout port handshake, AttachConnect, crash restart
// with backoff + T84 breaker. In-process (T102.6) later produces the
// same Instance. Readiness is exposed on the T83 diag surface via
// DynamicChecks.
//
// 🎯T102.7 owns backend facet adapters: reconcile → StreamReconciler
// (POST /reconcile), check → diag.Check (GET /check), notify → POST
// /notify. Facets ride the existing single scheduler and diag registry;
// short HTTP timeouts isolate a slow/broken plugin.
package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

// State is the lifecycle state of a plugin instance.
type State string

const (
	// StateConfigured means the entry is enabled but the transport
	// has not yet produced a base URL (launch/inprocess pending).
	StateConfigured State = "configured"
	// StateStarting means a connect/launch is in progress.
	StateStarting State = "starting"
	// StateReady means the instance has a base URL and a valid manifest.
	StateReady State = "ready"
	// StateError means the last start or manifest fetch failed.
	StateError State = "error"
	// StateStopped means the instance was torn down (disable/remove).
	StateStopped State = "stopped"
)

// Instance is the uniform plugin-instance abstraction (🎯T102.3): a
// base URL plus a validated manifest, independent of how the process
// was started (connect / launch / in-process). Metadata comes from
// Manifest, never from config. Transports only differ in how they
// obtain BaseURL; after attach, all three look the same.
type Instance struct {
	Name     string
	Entry    store.PluginEntry
	BaseURL  string // set once the transport has a reachable HTTP root
	Manifest *Manifest
	State    State
	Err      string // last error, if any — also feeds diag.plugin.<name>.ready
	Home     string // ~/.mnemo/plugins/<name> convention path (may not exist)

	// launch is non-nil while a launch-mode supervisor owns this instance
	// (🎯T102.4). Cleared on stop before the supervisor is joined.
	launch *launchSupervisor
	// inproc is non-nil for in-process interpreted hosts (🎯T102.6).
	inproc *inprocessHost
}

// Snapshot is a copy-safe view of an Instance for callers.
type Snapshot struct {
	Name      string         `json:"name"`
	Enabled   bool           `json:"enabled"`
	Transport string         `json:"transport"`
	BaseURL   string         `json:"base_url,omitempty"`
	State     State          `json:"state"`
	Err       string         `json:"error,omitempty"`
	Home      string         `json:"home,omitempty"`
	Manifest  *Manifest      `json:"manifest,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
}

// Manager owns the set of plugin instances and reconciles them against
// config on every hot-reload. Safe for concurrent use.
type Manager struct {
	mu        sync.Mutex
	home      string // user home for path expansion
	client    *http.Client
	log       *slog.Logger
	instances map[string]*Instance
	// launchCfg is applied to every new launch supervisor (tests inject
	// short timeouts). Zero value → production defaults.
	launchCfg launchConfig
	// publish fans events to the T86 SSE hub (plugin.reload, 🎯T102.9).
	publish EventPublisher
	// onToolsChanged is invoked after reconcile when MCP-bridged tools
	// may have changed (🎯T102.10). Optional.
	onToolsChanged func()
	tools          ToolBridge
	mcpPlugins     map[string]struct{} // plugins that last synced MCP tools
}

// NewManager builds a Manager. userHome is used for ~ expansion and the
// ~/.mnemo/plugins/<name> convention. client may be nil (default probe
// client). log may be nil (slog.Default).
func NewManager(userHome string, client *http.Client, log *slog.Logger) *Manager {
	if client == nil {
		client = DefaultHTTPClient()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		home:      userHome,
		client:    client,
		log:       log.With("component", "plugin"),
		instances: map[string]*Instance{},
	}
}

// Reconcile brings the live instance set in line with entries:
// enable starts (or restarts on material change), disable/remove
// tears down, param-only changes update the entry without restart
// when the instance is already ready. Mirrors vault_path hot-swap:
// no daemon restart required.
func (m *Manager) Reconcile(ctx context.Context, entries []store.PluginEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	wanted := make(map[string]store.PluginEntry, len(entries))
	for _, e := range entries {
		wanted[e.Name] = e
	}

	// Tear down instances that disappeared or were disabled.
	for name, inst := range m.instances {
		e, ok := wanted[name]
		if !ok || !e.Enabled {
			m.stopLocked(inst)
			if !ok {
				delete(m.instances, name)
			}
		}
	}

	// Start or update enabled entries.
	for name, e := range wanted {
		if !e.Enabled {
			// Keep a stopped placeholder so List can still show the
			// configured-but-disabled entry if it was previously known;
			// otherwise leave it absent until first enable.
			if inst, ok := m.instances[name]; ok {
				inst.Entry = e
				if inst.State != StateStopped {
					m.stopLocked(inst)
				}
			}
			continue
		}
		inst, ok := m.instances[name]
		if !ok {
			inst = m.newInstance(e)
			m.instances[name] = inst
			m.startLocked(ctx, inst)
			continue
		}
		// Compare against the previous entry before overwriting, so
		// URL/command/script changes still trigger restart.
		restart := needsRestart(inst, e)
		inst.Entry = e
		inst.Home = store.PluginHome(m.home, e.Name)
		if restart {
			m.stopLocked(inst)
			m.startLocked(ctx, inst)
		}
	}
	// Refresh MCP tool bridge after set stabilises (🎯T102.10).
	m.mu.Unlock()
	m.syncMCPTools()
	m.mu.Lock()
}

// List returns a snapshot of every currently tracked instance.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, snapshotOf(inst))
	}
	return out
}

// Get returns a snapshot of the named instance, if any.
func (m *Manager) Get(name string) (Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.instances[name]
	if !ok {
		return Snapshot{}, false
	}
	return snapshotOf(inst), true
}

// PluginHomes returns conventional on-disk homes for the given entries
// (🎯T102.12 fence registration). Does not create directories.
func (m *Manager) PluginHomes(entries []store.PluginEntry) []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		out = append(out, store.PluginHome(m.home, e.Name))
	}
	return out
}

// Close stops every instance. Safe to call multiple times.
func (m *Manager) Close() {
	m.mu.Lock()
	var launches []*launchSupervisor
	var inprocs []*inprocessHost
	for _, inst := range m.instances {
		if inst.launch != nil {
			launches = append(launches, inst.launch)
			inst.launch = nil
		}
		if inst.inproc != nil {
			inprocs = append(inprocs, inst.inproc)
			inst.inproc = nil
		}
		inst.BaseURL = ""
		inst.Manifest = nil
		inst.State = StateStopped
		inst.Err = ""
	}
	m.instances = map[string]*Instance{}
	m.mu.Unlock()
	// Join supervisors outside the lock so their state-update callbacks
	// never deadlock against us.
	for _, s := range launches {
		s.Stop()
	}
	for _, h := range inprocs {
		h.Stop()
	}
	if m.onToolsChanged != nil {
		m.onToolsChanged()
	}
}

func (m *Manager) newInstance(e store.PluginEntry) *Instance {
	return &Instance{
		Name:  e.Name,
		Entry: e,
		Home:  store.PluginHome(m.home, e.Name),
		State: StateConfigured,
	}
}

func (m *Manager) startLocked(ctx context.Context, inst *Instance) {
	inst.State = StateStarting
	inst.Err = ""
	inst.Manifest = nil
	inst.BaseURL = ""

	switch inst.Entry.Transport {
	case store.PluginTransportConnect:
		// 🎯T102.3: attach to an already-running server via the uniform path.
		att, err := AttachConnect(ctx, m.client, inst.Entry.URL, inst.Name)
		if err != nil {
			inst.State = StateError
			inst.Err = err.Error()
			// Keep BaseURL so diag can show what we tried to reach.
			inst.BaseURL = strings.TrimRight(inst.Entry.URL, "/")
			m.log.Warn("plugin connect failed", "name", inst.Name, "url", inst.BaseURL, "err", err)
			return
		}
		m.applyAttachLocked(inst, att)
		m.log.Info("plugin ready", "name", inst.Name, "transport", "connect",
			"version", att.Manifest.Version, "base_url", att.BaseURL)
	case store.PluginTransportLaunch:
		m.startLaunchLocked(ctx, inst)
	case store.PluginTransportInProcess:
		att, host, err := startInProcess(ctx, m, inst)
		if err != nil {
			inst.State = StateError
			inst.Err = err.Error()
			m.log.Warn("plugin in-process failed", "name", inst.Name, "err", err)
			return
		}
		inst.inproc = host
		m.applyAttachLocked(inst, att)
		m.log.Info("plugin ready", "name", inst.Name, "transport", "inprocess",
			"version", att.Manifest.Version, "base_url", att.BaseURL)
	default:
		inst.State = StateError
		inst.Err = fmt.Sprintf("unknown transport %q", inst.Entry.Transport)
	}
}

// startLaunchLocked spawns the configured executable, waits for the
// MNEMO_PLUGIN_PORT handshake, AttachConnects, and starts the restart
// supervisor (🎯T102.4). First attach is synchronous so Reconcile
// returns with Ready/Error; the background loop owns crash restarts.
func (m *Manager) startLaunchLocked(ctx context.Context, inst *Instance) {
	cmdPath := store.ExpandPluginPath(inst.Entry.Command, m.home, inst.Home)
	sup := newLaunchSupervisor(m, inst, cmdPath, m.launchCfg)
	inst.launch = sup

	att, err := sup.startOnce(ctx)
	firstOK := err == nil
	if err != nil {
		inst.State = StateError
		inst.Err = err.Error()
		// Count the failed first start toward the breaker so a binary
		// that never handshakes still trips after threshold retries.
		sup.br.Record(time.Now(), false, err.Error())
		m.log.Warn("plugin launch failed", "name", inst.Name, "command", cmdPath, "err", err)
	} else {
		m.applyAttachLocked(inst, att)
		m.log.Info("plugin ready", "name", inst.Name, "transport", "launch",
			"version", att.Manifest.Version, "base_url", att.BaseURL)
	}
	// Always run the supervisor: on first-start failure it retries with
	// backoff/breaker; on success it waits for exit and restarts.
	go sup.run(firstOK)
}

// applyAttachLocked records a successful attach on inst. Shared by
// connect and launch (and later in-process) once they have a base URL.
func (m *Manager) applyAttachLocked(inst *Instance, att *AttachResult) {
	inst.BaseURL = att.BaseURL
	inst.Manifest = att.Manifest
	inst.State = StateReady
	inst.Err = ""
	// Force-reload any live WKWebView for this plugin (🎯T102.9).
	if att.Manifest != nil && att.Manifest.UI != nil {
		m.emitReload(inst.Name)
	}
}

// noteLaunchReady applies a successful (re)attach from a live supervisor.
// No-op when the instance was stopped or the supervisor replaced.
func (m *Manager) noteLaunchReady(inst *Instance, sup *launchSupervisor, att *AttachResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst == nil || inst.launch != sup {
		return
	}
	m.applyAttachLocked(inst, att)
}

// noteLaunchError records a launch failure/crash from a live supervisor.
func (m *Manager) noteLaunchError(inst *Instance, sup *launchSupervisor, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst == nil || inst.launch != sup {
		return
	}
	inst.State = StateError
	inst.Err = errMsg
	// Clear ready metadata so proxy/diag do not advertise a dead base URL.
	inst.BaseURL = ""
	inst.Manifest = nil
}

func (m *Manager) stopLocked(inst *Instance) {
	if inst == nil {
		return
	}
	launch := inst.launch
	inst.launch = nil
	inproc := inst.inproc
	inst.inproc = nil
	inst.BaseURL = ""
	inst.Manifest = nil
	inst.State = StateStopped
	inst.Err = ""
	if launch != nil {
		// Join outside the lock: the supervise loop may take m.mu to
		// publish state and must not deadlock against Stop's wait.
		m.mu.Unlock()
		launch.Stop()
		m.mu.Lock()
	}
	if inproc != nil {
		m.mu.Unlock()
		inproc.Stop()
		m.mu.Lock()
	}
	m.log.Info("plugin stopped", "name", inst.Name)
}

// needsRestart reports whether the transport identity of e differs from
// the running instance enough that a stop+start is required. Pure param
// changes do not force a restart here; later targets may re-POST config.
func needsRestart(inst *Instance, e store.PluginEntry) bool {
	if inst.State == StateStopped || inst.State == StateError || inst.State == StateConfigured {
		// Always (re)try start for enabled entries that are not ready.
		return e.Enabled
	}
	old := inst.Entry
	if old.Transport != e.Transport {
		return true
	}
	switch e.Transport {
	case store.PluginTransportConnect:
		return old.URL != e.URL
	case store.PluginTransportLaunch:
		return old.Command != e.Command || !stringSlicesEqual(old.Args, e.Args)
	case store.PluginTransportInProcess:
		return old.Script != e.Script
	}
	return false
}

func snapshotOf(inst *Instance) Snapshot {
	var params map[string]any
	if inst.Entry.Params != nil {
		params = inst.Entry.Params
	}
	return Snapshot{
		Name:      inst.Name,
		Enabled:   inst.Entry.Enabled,
		Transport: inst.Entry.Transport,
		BaseURL:   inst.BaseURL,
		State:     inst.State,
		Err:       inst.Err,
		Home:      inst.Home,
		Manifest:  inst.Manifest,
		Params:    params,
	}
}

func stringSlicesEqual(a, b []string) bool {
	return reflect.DeepEqual(a, b)
}
