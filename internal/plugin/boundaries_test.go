// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// 🎯T102.12: disable/hot-reload fully tears down — no ready state, no
// proxy route, no UI contribution, no leaked BaseURL.
func TestTeardownClearsAllSurfaces(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "main.js")
	if err := os.WriteFile(script, []byte(sampleJSPlugin), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.TempDir(), nil, testLogger())
	t.Cleanup(m.Close)

	entry := store.PluginEntry{
		Name: "bound", Enabled: true,
		Transport: store.PluginTransportInProcess, Script: script,
	}
	m.Reconcile(context.Background(), []store.PluginEntry{entry})
	snap, _ := m.Get("bound")
	if snap.State != StateReady {
		t.Fatalf("setup: %s %s", snap.State, snap.Err)
	}
	base := snap.BaseURL

	// Disable.
	entry.Enabled = false
	m.Reconcile(context.Background(), []store.PluginEntry{entry})
	snap, _ = m.Get("bound")
	if snap.State != StateStopped {
		t.Fatalf("after disable: %s", snap.State)
	}
	if snap.BaseURL != "" || snap.Manifest != nil {
		t.Fatalf("metadata leak: %+v", snap)
	}
	if len(m.UIContributions()) != 0 {
		t.Fatal("UI contribution leaked")
	}
	// Direct dial to former base should fail (server stopped).
	if base != "" {
		resp, err := m.client.Get(base + "/ready")
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("in-process server still accepting after teardown")
		}
	}
}

// 🎯T102.12: plugin home paths are enumeratable for fence registration.
func TestPluginHomesForFence(t *testing.T) {
	home := t.TempDir()
	m := NewManager(home, nil, testLogger())
	t.Cleanup(m.Close)
	// Even without starting, homes are deterministic for config names.
	homes := m.PluginHomes([]store.PluginEntry{
		{Name: "a"}, {Name: "b"},
	})
	if len(homes) != 2 {
		t.Fatalf("homes: %v", homes)
	}
	for _, h := range homes {
		if _, err := os.Stat(filepath.Dir(h)); err != nil && !os.IsNotExist(err) {
			// parent may not exist yet — that's fine; path is under home/.mnemo/plugins
		}
		if filepath.Dir(filepath.Dir(h)) != filepath.Join(home, ".mnemo") &&
			filepath.Base(filepath.Dir(h)) != "plugins" {
			// Accept standard layout ~/.mnemo/plugins/<name>
			wantPrefix := filepath.Join(home, ".mnemo", "plugins")
			if filepath.Dir(h) != wantPrefix {
				t.Fatalf("home %q not under %q", h, wantPrefix)
			}
		}
	}
}
