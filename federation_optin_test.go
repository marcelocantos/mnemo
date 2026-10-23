// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/e2e"
)

// Exercise the real CLI launch path, not just federation.Start: an omitted
// flag must not generate endpoint material or attempt to bind a public port.
func TestFederationDefaultOff(t *testing.T) {
	if defaultFederatedAddr != "" {
		t.Fatalf("serve default = %q, want off", defaultFederatedAddr)
	}
	if conventionalFederatedAddr != ":19420" {
		t.Fatalf("URL convention = %q", conventionalFederatedAddr)
	}
	d := e2e.Start(t)
	if _, err := os.Stat(filepath.Join(d.Home, ".mnemo", "endpoint")); !os.IsNotExist(err) {
		t.Fatalf("disabled federation generated endpoint material: %v", err)
	}
	resp, err := http.Get(strings.Replace(d.URL, "/mcp", "/health", 1))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health HTTP %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(strings.ToLower(string(body)), "federated listener disabled") {
		t.Fatalf("disabled listener reported as unhealthy: %s", body)
	}
}

func TestFederationExplicitAddrStartsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	d := e2e.Start(t, e2e.Options{Args: []string{"--federated-addr=" + addr}})
	if _, err := os.Stat(filepath.Join(d.Home, ".mnemo", "endpoint", "cert.pem")); err != nil {
		t.Fatalf("enabled federation did not generate certificate: %v", err)
	}
	// The local /health and MCP endpoints can become ready before the
	// optional TLS listener has finished binding, especially on Windows.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("federated listener at %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
