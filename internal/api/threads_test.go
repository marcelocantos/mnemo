// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/mnemo/internal/store"
)

// setupThreadsHome points MNEMO_HOME at a temp dir with a config.json whose
// threads_root contains one seeded thread, and returns a router with the
// thread routes attached.
func setupThreadsHome(t *testing.T) *http.ServeMux {
	t.Helper()
	home := t.TempDir()
	t.Setenv(store.MnemoHomeEnv, home)

	root := filepath.Join(home, "think", "threads", "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "# demo\n\n## Status\n\nactive and humming\n\n## Current focus\n\nship it\n"
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(home, ".mnemo")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"threads_root": "` + filepath.Join(home, "think", "threads") + `"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	h := New(func(string) (store.Backend, error) { return nil, nil })
	mux := http.NewServeMux()
	h.registerThreadRoutes(mux)
	return mux
}

func TestAPIThreadList(t *testing.T) {
	mux := setupThreadsHome(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thread/list", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Count   int `json:"count"`
		Threads []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || len(out.Threads) != 1 || out.Threads[0].Name != "demo" {
		t.Fatalf("unexpected list result: %s", rec.Body.String())
	}
	if out.Threads[0].State != "active" {
		t.Errorf("state = %q, want active", out.Threads[0].State)
	}
}

func TestAPIThreadPreview(t *testing.T) {
	mux := setupThreadsHome(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thread/preview?name=demo&theme=light", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "<!DOCTYPE html>") {
		t.Errorf("preview not a standalone document: %.40q", body)
	}
	// Synthetic H1 (the dir name) is prepended at render time.
	if !strings.Contains(body, "<h1>demo</h1>") {
		t.Errorf("synthetic H1 missing from preview")
	}
}

func TestAPIThreadConfig(t *testing.T) {
	mux := setupThreadsHome(t)

	// GET returns the configured root.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thread/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["threads_root"] == "" || got["default"] == "" {
		t.Errorf("config GET missing fields: %v", got)
	}

	// POST repoints the root; a subsequent list reads from the new folder.
	newRoot := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(newRoot, "solo"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/thread/config?root="+newRoot, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thread/list", nil))
	if !strings.Contains(rec.Body.String(), "solo") {
		t.Errorf("list after repoint did not read the new root: %s", rec.Body.String())
	}
}

func TestAPIThreadNewRejectsGet(t *testing.T) {
	mux := setupThreadsHome(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thread/new?name=x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/thread/new status = %d, want 405", rec.Code)
	}
}
