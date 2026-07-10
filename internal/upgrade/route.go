// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RouteFileName is the edge dynamic routing control file under ~/.mnemo/.
const RouteFileName = "edge-route.json"

// RouteFile is the JSON shape edge polls for backend list + primary
// (🎯T97.5 multi-process handoff).
type RouteFile struct {
	Backends []string `json:"backends"`
	Primary  int      `json:"primary"`
	// PinCounts is written by the edge (length == len(Backends)) so a
	// draining backend can AffinityDrain until its index is zero.
	// Backends must not treat this as an input control plane field.
	PinCounts []int `json:"pin_counts,omitempty"`
	// RepinAll is crash-failover only (FailoverRepin). Happy-path
	// upgrade drain must leave pins on the old backend.
	RepinAll bool `json:"repin_all,omitempty"`
}

// PinCountAt returns PinCounts[i] or 0 if missing.
func (r RouteFile) PinCountAt(i int) int {
	if i < 0 || i >= len(r.PinCounts) {
		return 0
	}
	return r.PinCounts[i]
}

// IndexOfBackend returns the index of backendURL or -1.
func (r RouteFile) IndexOfBackend(backendURL string) int {
	for i, b := range r.Backends {
		if b == backendURL {
			return i
		}
	}
	return -1
}

// RoutePath returns ~/.mnemo/edge-route.json.
func RoutePath(home string) string {
	return filepath.Join(home, ".mnemo", RouteFileName)
}

// RouteConfigured reports whether an edge route file exists.
func RouteConfigured(home string) bool {
	if home == "" {
		return false
	}
	_, err := os.Stat(RoutePath(home))
	return err == nil
}

// ReadRoute loads the route file.
func ReadRoute(home string) (RouteFile, error) {
	data, err := os.ReadFile(RoutePath(home))
	if err != nil {
		return RouteFile{}, err
	}
	var r RouteFile
	if err := json.Unmarshal(data, &r); err != nil {
		return RouteFile{}, err
	}
	if len(r.Backends) == 0 {
		return RouteFile{}, fmt.Errorf("upgrade: edge-route has no backends")
	}
	if r.Primary < 0 || r.Primary >= len(r.Backends) {
		return RouteFile{}, fmt.Errorf("upgrade: edge-route primary %d out of range", r.Primary)
	}
	return r, nil
}

// WriteRoute atomically persists the route file under home.
func WriteRoute(home string, r RouteFile) error {
	if err := os.MkdirAll(filepath.Join(home, ".mnemo"), 0o755); err != nil {
		return err
	}
	return WriteRouteFile(RoutePath(home), r)
}

// WriteRouteFile atomically writes r to path (tmp + rename) so concurrent
// readers never see a partial JSON document.
func WriteRouteFile(path string, r RouteFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + fmt.Sprintf("%d", os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AppendBackend adds a backend URL and sets it as primary.
func AppendBackend(home, backendURL string) (RouteFile, error) {
	r, err := ReadRoute(home)
	if err != nil {
		// First write: single backend as primary.
		r = RouteFile{Backends: []string{backendURL}, Primary: 0}
		if err := WriteRoute(home, r); err != nil {
			return RouteFile{}, err
		}
		return r, nil
	}
	for i, b := range r.Backends {
		if b == backendURL {
			r.Primary = i
			return r, WriteRoute(home, r)
		}
	}
	r.Backends = append(r.Backends, backendURL)
	r.Primary = len(r.Backends) - 1
	if err := WriteRoute(home, r); err != nil {
		return RouteFile{}, err
	}
	return r, nil
}

// RouteWatcher is a testable holder for the last loaded route.
type RouteWatcher struct {
	mu   sync.Mutex
	last RouteFile
	ok   bool
}

// Load reads home's route file into the watcher.
func (w *RouteWatcher) Load(home string) (RouteFile, error) {
	r, err := ReadRoute(home)
	if err != nil {
		return RouteFile{}, err
	}
	w.mu.Lock()
	w.last = r
	w.ok = true
	w.mu.Unlock()
	return r, nil
}
