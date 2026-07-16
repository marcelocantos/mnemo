// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package plugin implements mnemo's plugin registry (🎯T102).
//
// 🎯T102.2 owns config declaration, hot-reload reconcile, and
// manifest-based metadata discovery. Transports fill in progressively:
// connect (🎯T102.3) attaches to a base URL and fetches the manifest;
// launch (🎯T102.4) and in-process (🎯T102.6) produce the same Instance
// abstraction once wired. This package starts connect-mode instances
// fully enough for registry tests; launch/inprocess are tracked as
// configured instances until those targets land.
package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"

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

// Instance is one running (or configured) plugin, keyed by name.
// Metadata comes from Manifest, never from config.
type Instance struct {
	Name     string
	Entry    store.PluginEntry
	BaseURL  string // set for connect (and later launch); empty for pending transports
	Manifest *Manifest
	State    State
	Err      string // last error, if any
	Home     string // ~/.mnemo/plugins/<name> convention path (may not exist)
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
		inst.Entry = e
		inst.Home = store.PluginHome(m.home, e.Name)
		if needsRestart(inst, e) {
			m.stopLocked(inst)
			m.startLocked(ctx, inst)
		}
	}
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

// Close stops every instance. Safe to call multiple times.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.instances {
		m.stopLocked(inst)
	}
	m.instances = map[string]*Instance{}
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
		base := strings.TrimRight(inst.Entry.URL, "/")
		inst.BaseURL = base
		man, err := FetchManifest(ctx, m.client, base)
		if err != nil {
			inst.State = StateError
			inst.Err = err.Error()
			m.log.Warn("plugin start failed", "name", inst.Name, "transport", "connect", "err", err)
			return
		}
		if man.Name != "" && man.Name != inst.Name {
			inst.State = StateError
			inst.Err = fmt.Sprintf("manifest name %q does not match config name %q", man.Name, inst.Name)
			m.log.Warn("plugin name mismatch", "name", inst.Name, "manifest_name", man.Name)
			return
		}
		inst.Manifest = man
		inst.State = StateReady
		m.log.Info("plugin ready", "name", inst.Name, "transport", "connect", "version", man.Version)
	case store.PluginTransportLaunch, store.PluginTransportInProcess:
		// Transport wiring lands in 🎯T102.4 / 🎯T102.6. Track the
		// instance so enable/disable reconcile is real; leave state
		// configured until a base URL exists.
		inst.State = StateConfigured
		m.log.Info("plugin configured (transport pending)",
			"name", inst.Name, "transport", inst.Entry.Transport)
	default:
		inst.State = StateError
		inst.Err = fmt.Sprintf("unknown transport %q", inst.Entry.Transport)
	}
}

func (m *Manager) stopLocked(inst *Instance) {
	if inst == nil {
		return
	}
	// Connect-mode has no child process; launch-mode will SIGTERM here (T102.4).
	inst.BaseURL = ""
	inst.Manifest = nil
	inst.State = StateStopped
	inst.Err = ""
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
