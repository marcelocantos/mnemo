// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package endpoint

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
	"runtime"
	"testing"
	"time"
)

func TestFirstRunGeneratesCertAndKey(t *testing.T) {
	dir := t.TempDir()
	ep, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ep.Cert == nil || ep.PrivateKey == nil {
		t.Fatal("expected cert+key populated")
	}
	if len(ep.CertPEM) == 0 || len(ep.KeyPEM) == 0 {
		t.Fatal("expected PEM bytes populated")
	}
	if _, err := os.Stat(filepath.Join(dir, "endpoint", "cert.pem")); err != nil {
		t.Fatalf("cert.pem missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "endpoint", "key.pem")); err != nil {
		t.Fatalf("key.pem missing: %v", err)
	}
	if !ep.Cert.NotAfter.After(time.Now().Add(365 * 24 * time.Hour)) {
		t.Errorf("cert validity too short: NotAfter=%s", ep.Cert.NotAfter)
	}
}

func TestKeyFileIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are POSIX-only")
	}
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "endpoint", "key.pem"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := info.Mode().Perm(); got != KeyMode {
		t.Errorf("key mode = %o, want %o", got, KeyMode)
	}
}

func TestKeyFileChmodOnReload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are POSIX-only")
	}
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("first Load: %v", err)
	}
	keyPath := filepath.Join(dir, "endpoint", "key.pem")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := info.Mode().Perm(); got != KeyMode {
		t.Errorf("key mode after reload = %o, want %o", got, KeyMode)
	}
}

func TestReloadDoesNotRegenerate(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if first.Cert.SerialNumber.Cmp(second.Cert.SerialNumber) != 0 {
		t.Errorf("cert regenerated on reload: serial differs (first=%s, second=%s)",
			first.Cert.SerialNumber, second.Cert.SerialNumber)
	}
}

func TestCorruptCertTriggersRegen(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	certPath := filepath.Join(dir, "endpoint", "cert.pem")
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after corruption: %v", err)
	}
	if first.Cert.SerialNumber.Cmp(second.Cert.SerialNumber) == 0 {
		t.Error("expected regenerated cert after corruption (serials match)")
	}
}

func TestExpiredCertTriggersRegen(t *testing.T) {
	dir := t.TempDir()
	epDir := filepath.Join(dir, "endpoint")
	if err := os.MkdirAll(epDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeExpiredCertAndKey(t, filepath.Join(epDir, "cert.pem"), filepath.Join(epDir, "key.pem"))

	ep, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ep.Cert.NotAfter.After(time.Now()) {
		t.Errorf("cert was not regenerated; still expired (NotAfter=%s)", ep.Cert.NotAfter)
	}
}

func TestPeersLoadValidAndSkipInvalid(t *testing.T) {
	dir := t.TempDir()
	peersDir := filepath.Join(dir, "peers")
	if err := os.MkdirAll(peersDir, 0o700); err != nil {
		t.Fatalf("mkdir peers: %v", err)
	}

	writeRandomCert(t, filepath.Join(peersDir, "alice.pem"))
	writeRandomCert(t, filepath.Join(peersDir, "carol.pem"))

	if err := os.WriteFile(filepath.Join(peersDir, "bob.pem"), []byte("not a cert"), 0o644); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	wrongType := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{0x01, 0x02, 0x03}})
	if err := os.WriteFile(filepath.Join(peersDir, "dave.pem"), wrongType, 0o644); err != nil {
		t.Fatalf("write wrong type: %v", err)
	}
	if err := os.WriteFile(filepath.Join(peersDir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write non-pem: %v", err)
	}

	ep, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"alice", "carol"}
	if len(ep.PeerNames) != len(want) {
		t.Fatalf("peers loaded = %v, want %v", ep.PeerNames, want)
	}
	for i, name := range want {
		if ep.PeerNames[i] != name {
			t.Errorf("peer[%d] = %q, want %q", i, ep.PeerNames[i], name)
		}
	}
	if ep.TrustedPeers == nil {
		t.Fatal("TrustedPeers nil")
	}
}

func TestPeersDirMissingIsOK(t *testing.T) {
	dir := t.TempDir()
	ep, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(ep.PeerNames) != 0 {
		t.Errorf("expected no peers, got %v", ep.PeerNames)
	}
	if ep.TrustedPeers == nil {
		t.Error("TrustedPeers should be non-nil empty pool")
	}
}

// writeExpiredCertAndKey writes a self-signed cert + matching key
// whose NotAfter is 24h in the past.
func writeExpiredCertAndKey(t *testing.T, certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "expired"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	if err := os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}),
		0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

func writeRandomCert(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "peer"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	if err := os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}),
		0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
}
