// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateLinkedInstancesEmpty(t *testing.T) {
	cfg := Config{}
	if err := cfg.validateLinkedInstances(t.TempDir()); err != nil {
		t.Errorf("empty list: %v", err)
	}
}

func TestWriteConfigToRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := Config{
		WorkspaceRoots:   []string{"/tmp/a", "/tmp/b"},
		ExtraProjectDirs: []string{"/mnt/c"},
		VaultPath:        "~/Documents/v",
	}
	if err := writeConfigTo(path, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.VaultPath != cfg.VaultPath {
		t.Errorf("vault_path: got %q want %q", got.VaultPath, cfg.VaultPath)
	}
	if len(got.WorkspaceRoots) != 2 || got.WorkspaceRoots[0] != "/tmp/a" {
		t.Errorf("workspace_roots: %v", got.WorkspaceRoots)
	}
	if len(got.ExtraProjectDirs) != 1 || got.ExtraProjectDirs[0] != "/mnt/c" {
		t.Errorf("extra_project_dirs: %v", got.ExtraProjectDirs)
	}
}

func TestWriteConfigToAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"vault_path":"/old"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeConfigTo(path, Config{VaultPath: "/new"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), `"vault_path": "/new"`) {
		t.Errorf("expected /new in file, got: %s", data)
	}
	// Tmp file must not be left behind.
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".config.json.") {
			t.Errorf("tmp file left behind: %s", ent.Name())
		}
	}
}

// TestValidateVaultPathRejectsRegularFile mirrors the vault.New failure
// mode that the WriteConfig trial-balloon must catch: a vault_path
// pointing at an existing regular file makes MkdirAll return ENOTDIR.
// Without this guard a "mnemo_config op=write" with a bad path would
// persist, leaving on-disk config that the next daemon start cannot
// bring up cleanly.
func TestValidateVaultPathRejectsRegularFile(t *testing.T) {
	home := t.TempDir()
	bad := filepath.Join(home, "not-a-dir")
	if err := os.WriteFile(bad, []byte("blocking"), 0o644); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}

	cfg := Config{VaultPath: bad}
	if err := cfg.validateVaultPath(home); err == nil {
		t.Fatalf("expected error for vault_path pointing at a regular file, got nil")
	}
}

// TestValidateVaultPathEmptyIsAllowed documents the "vault disabled"
// semantics: an empty VaultPath passes validation without touching the
// filesystem (so disabling vault via mnemo_config never trips the
// trial-balloon).
func TestValidateVaultPathEmptyIsAllowed(t *testing.T) {
	if err := (Config{}).validateVaultPath(t.TempDir()); err != nil {
		t.Errorf("empty vault_path should pass validation, got %v", err)
	}
}

// TestValidateVaultPathExpandsTilde checks that ~ expansion happens
// against the supplied home before MkdirAll runs. A literal "~/v" must
// not be passed to MkdirAll — that would create a directory named "~"
// in the current working directory.
func TestValidateVaultPathExpandsTilde(t *testing.T) {
	home := t.TempDir()
	cfg := Config{VaultPath: "~/v"}
	if err := cfg.validateVaultPath(home); err != nil {
		t.Fatalf("tilde expansion: %v", err)
	}
	wantDir := filepath.Join(home, "v")
	if fi, err := os.Stat(wantDir); err != nil || !fi.IsDir() {
		t.Errorf("expected MkdirAll on %q, stat err=%v", wantDir, err)
	}
}

func TestValidateLinkedInstancesValidPeerByName(t *testing.T) {
	peersDir := t.TempDir()
	writePeerCert(t, filepath.Join(peersDir, "alice.pem"))

	cfg := Config{LinkedInstances: []LinkedInstance{
		{Name: "alice", URL: "https://alice.example:19419/mcp", PeerCert: "alice"},
	}}
	if err := cfg.validateLinkedInstances(peersDir); err != nil {
		t.Errorf("expected valid, got %v", err)
	}
}

func TestValidateLinkedInstancesDuplicateName(t *testing.T) {
	peersDir := t.TempDir()
	writePeerCert(t, filepath.Join(peersDir, "alice.pem"))

	cfg := Config{LinkedInstances: []LinkedInstance{
		{Name: "alice", URL: "https://a.example/mcp", PeerCert: "alice"},
		{Name: "alice", URL: "https://b.example/mcp", PeerCert: "alice"},
	}}
	err := cfg.validateLinkedInstances(peersDir)
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	if !strings.Contains(err.Error(), `duplicate name "alice"`) ||
		!strings.Contains(err.Error(), "indexes 0 and 1") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateLinkedInstancesMalformedURL(t *testing.T) {
	peersDir := t.TempDir()
	writePeerCert(t, filepath.Join(peersDir, "alice.pem"))

	cases := []struct {
		name, url string
		wantSub   string
	}{
		{"http-scheme", "http://alice.example/mcp", "scheme must be https"},
		{"missing-scheme", "alice.example/mcp", "scheme must be https"},
		{"unparseable", "https://[::1", "parse url"},
		{"empty", "", "url is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{LinkedInstances: []LinkedInstance{
				{Name: "alice", URL: c.url, PeerCert: "alice"},
			}}
			err := cfg.validateLinkedInstances(peersDir)
			if err == nil {
				t.Fatalf("expected error for url=%q", c.url)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestValidateLinkedInstancesUnresolvablePeerCert(t *testing.T) {
	peersDir := t.TempDir() // empty — no alice.pem inside

	cfg := Config{LinkedInstances: []LinkedInstance{
		{Name: "alice", URL: "https://alice.example/mcp", PeerCert: "alice"},
	}}
	err := cfg.validateLinkedInstances(peersDir)
	if err == nil {
		t.Fatal("expected unresolvable peer-cert error")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "alice.pem") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateLinkedInstancesInlinePEMHappy(t *testing.T) {
	peersDir := t.TempDir() // not consulted for inline PEM
	pemStr := newPeerPEM(t)

	cfg := Config{LinkedInstances: []LinkedInstance{
		{Name: "alice", URL: "https://alice.example/mcp", PeerCert: pemStr},
	}}
	if err := cfg.validateLinkedInstances(peersDir); err != nil {
		t.Errorf("inline PEM should validate, got %v", err)
	}
}

func TestValidateLinkedInstancesInlinePEMMalformed(t *testing.T) {
	peersDir := t.TempDir()

	cases := []struct {
		name, value, wantSub string
	}{
		{
			"not-a-cert-block",
			"-----BEGIN RSA PRIVATE KEY-----\nbm9wZQ==\n-----END RSA PRIVATE KEY-----\n",
			"not a CERTIFICATE PEM block",
		},
		{
			"truncated-cert-bytes",
			"-----BEGIN CERTIFICATE-----\nbm9wZQ==\n-----END CERTIFICATE-----\n",
			"parse inline peer_cert",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{LinkedInstances: []LinkedInstance{
				{Name: "alice", URL: "https://alice.example/mcp", PeerCert: c.value},
			}}
			err := cfg.validateLinkedInstances(peersDir)
			if err == nil {
				t.Fatalf("expected malformed-PEM error for case %q", c.name)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestValidateLinkedInstancesMissingFields(t *testing.T) {
	peersDir := t.TempDir()
	writePeerCert(t, filepath.Join(peersDir, "alice.pem"))

	cases := []struct {
		name    string
		entry   LinkedInstance
		wantSub string
	}{
		{"no-name", LinkedInstance{URL: "https://x/", PeerCert: "alice"}, "name is required"},
		{"no-peer-cert", LinkedInstance{Name: "alice", URL: "https://x/"}, "peer_cert is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{LinkedInstances: []LinkedInstance{c.entry}}
			err := cfg.validateLinkedInstances(peersDir)
			if err == nil {
				t.Fatalf("expected error for case %q", c.name)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

func writePeerCert(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(newPeerPEM(t)), 0o644); err != nil {
		t.Fatalf("write peer cert: %v", err)
	}
}

func newPeerPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
