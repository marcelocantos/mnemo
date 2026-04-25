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
