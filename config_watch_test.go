// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/registry"
	"github.com/marcelocantos/mnemo/internal/store"
)

// The config watcher is what makes file-only configuration work
// (🎯T156). Removing mnemo_config removed the only thing that adopted a
// change, so if this does not fire, every hot-reloadable key silently
// became restart-required. It shipped without a test and a live smoke
// test proved inconclusive — these are the tests that should have come
// first.

// waitFor polls until cond or the deadline, so the tests do not depend
// on a fixed multiple of the watch interval.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConfigWatcherAdoptsAnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"vault_path":"/tmp/one"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	adopted := make(chan store.Config, 8)
	load := func() (store.Config, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return store.Config{}, err
		}
		var c store.Config
		if err := json.Unmarshal(b, &c); err != nil {
			return store.Config{}, err
		}
		return c, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfigPath(ctx, path, load, func(c store.Config) registry.ReloadReport {
		adopted <- c
		return registry.ReloadReport{Changed: []string{"vault_path"}, Adopted: []string{"vault_path"}}
	})

	// An edit must reach adopt with the new value.
	time.Sleep(50 * time.Millisecond) // let the watcher record the starting mtime
	if err := os.WriteFile(path, []byte(`{"vault_path":"/tmp/two"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-adopted:
		if got.VaultPath != "/tmp/two" {
			t.Errorf("adopted vault_path = %q, want /tmp/two", got.VaultPath)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher never adopted the edit — file-only configuration is inert")
	}
}

func TestConfigWatcherKeepsRunningConfigOnParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"vault_path":"/tmp/one"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var adopts int
	loadErr := errors.New("unexpected end of JSON input")
	load := func() (store.Config, error) { return store.Config{}, loadErr }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchConfigPath(ctx, path, load, func(store.Config) registry.ReloadReport {
		adopts++
		return registry.ReloadReport{}
	})

	time.Sleep(50 * time.Millisecond)
	// A half-written file: mid-save, not a request for defaults.
	if err := os.WriteFile(path, []byte(`{"vault_path":`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * configWatchInterval)
	if adopts != 0 {
		t.Errorf("adopt ran %d time(s) on an unparseable file; a half-saved edit must never replace the live config", adopts)
	}
}

func TestConfigWatcherStopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchConfigPath(ctx, path, func() (store.Config, error) { return store.Config{}, nil },
			func(store.Config) registry.ReloadReport { return registry.ReloadReport{} })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher ignored context cancellation; Close would hang on it")
	}
}
