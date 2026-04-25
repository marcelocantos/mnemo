// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package endpoint manages the mTLS material used by mnemo to expose a
// federated, peer-authenticated MCP endpoint and to call peer mnemo
// instances over mTLS (🎯T15).
//
// On first launch, mnemo generates a self-signed X.509 certificate and
// ECDSA P-256 private key under ~/.mnemo/endpoint/. The key file is
// written with mode 0600. Subsequent launches reload the existing
// files; a corrupt or expired certificate triggers regeneration with a
// warning.
//
// Trusted peer certificates are loaded from ~/.mnemo/peers/<name>.pem
// at startup. Each file contains a single PEM-encoded X.509
// certificate. Invalid entries are skipped with a warning so that one
// bad file does not prevent the daemon from starting.
//
// The returned *Endpoint is the typed accessor consumed by both
// 🎯T15.3 (federated server) and 🎯T15.4 (federated client) — they
// build their tls.Configs from its fields.
package endpoint

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// CertValidity is the lifetime of a generated self-signed cert.
	// mnemo's federation trust model is direct peer-to-peer: each
	// peer's cert is hand-distributed and trusted explicitly, so the
	// usual CA-rotation pressure does not apply. 10 years matches the
	// practical lifetime of an installation.
	CertValidity = 10 * 365 * 24 * time.Hour

	// KeyMode is the required permission bits on the private key file.
	KeyMode os.FileMode = 0o600

	endpointSubdir = "endpoint"
	peersSubdir    = "peers"
	certFile       = "cert.pem"
	keyFile        = "key.pem"
)

// Endpoint holds the mTLS material loaded (or generated) at startup.
type Endpoint struct {
	// CertPEM is the public certificate PEM, suitable for sharing
	// with peers via `mnemo print-endpoint`.
	CertPEM []byte

	// KeyPEM is the private key PEM. Held in memory for callers that
	// need to construct a tls.Certificate without re-reading disk; the
	// file under EndpointDir is the source of truth.
	KeyPEM []byte

	// Cert is the parsed X.509 certificate.
	Cert *x509.Certificate

	// PrivateKey is the parsed ECDSA private key.
	PrivateKey *ecdsa.PrivateKey

	// TrustedPeers is the set of trusted peer certs, populated from
	// ~/.mnemo/peers/<name>.pem at load time. Always non-nil — an
	// empty pool simply means no peers are trusted yet.
	TrustedPeers *x509.CertPool

	// PeerNames lists the basename (without the .pem suffix) of every
	// successfully loaded peer cert, sorted lexicographically.
	PeerNames []string

	// EndpointDir is the resolved ~/.mnemo/endpoint directory.
	EndpointDir string

	// PeersDir is the resolved ~/.mnemo/peers directory.
	PeersDir string
}

// Load reads or generates the endpoint material under mnemoDir
// (typically ~/.mnemo). It never panics on a corrupt or expired
// cert/key — those are regenerated with a slog.Warn. Trusted peer
// certs are loaded best-effort from <mnemoDir>/peers/.
func Load(mnemoDir string) (*Endpoint, error) {
	epDir := filepath.Join(mnemoDir, endpointSubdir)
	peersDir := filepath.Join(mnemoDir, peersSubdir)

	if err := os.MkdirAll(epDir, 0o700); err != nil {
		return nil, fmt.Errorf("create endpoint dir: %w", err)
	}

	certPath := filepath.Join(epDir, certFile)
	keyPath := filepath.Join(epDir, keyFile)

	cert, key, certPEM, keyPEM, err := loadCertAndKey(certPath, keyPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("regenerating endpoint cert/key", "reason", err)
		}
		cert, key, certPEM, keyPEM, err = generateAndWrite(certPath, keyPath)
		if err != nil {
			return nil, err
		}
	}

	// Defensive: enforce 0600 on key file even on reload, in case it
	// was written by an external tool with looser perms.
	if err := os.Chmod(keyPath, KeyMode); err != nil {
		return nil, fmt.Errorf("chmod key file: %w", err)
	}

	pool, names := loadPeers(peersDir)

	return &Endpoint{
		CertPEM:      certPEM,
		KeyPEM:       keyPEM,
		Cert:         cert,
		PrivateKey:   key,
		TrustedPeers: pool,
		PeerNames:    names,
		EndpointDir:  epDir,
		PeersDir:     peersDir,
	}, nil
}

// TLSCertificate returns the daemon's identity as a tls.Certificate,
// reusing the in-memory PEM bytes loaded by Load. Suitable for both
// server-side and client-side mTLS handshakes.
func (e *Endpoint) TLSCertificate() (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(e.CertPEM, e.KeyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("build tls.Certificate: %w", err)
	}
	return cert, nil
}

// ServerTLSConfig returns a tls.Config wired for mTLS as the server:
// the daemon presents its own cert as identity, and clients must
// present a cert that chains to one in the trusted-peer pool.
func (e *Endpoint) ServerTLSConfig() (*tls.Config, error) {
	cert, err := e.TLSCertificate()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    e.TrustedPeers,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLSConfig returns a tls.Config wired for mTLS as the client.
//
// The daemon presents its own cert as identity. The server it dials
// is authenticated by *cert pinning*: the server's cert must verify
// against the trusted-peer pool, but no hostname/SAN check is run.
// This matches mnemo's federation trust model — each peer's exact
// cert is hand-placed under ~/.mnemo/peers/ — so trust is per
// key-holder, not per DNS name. Hostname verification would just
// force operators to keep cert SANs in lockstep with the URL host
// they happen to dial, which is a chronic source of breakage and
// adds nothing security-wise once you already trust the cert.
func (e *Endpoint) ClientTLSConfig() (*tls.Config, error) {
	cert, err := e.TLSCertificate()
	if err != nil {
		return nil, err
	}
	pool := e.TrustedPeers
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // hostname check disabled; pinning below is the verification.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no certificate")
			}
			peer, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse server cert: %w", err)
			}
			if _, err := peer.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
				return fmt.Errorf("server cert not in trusted-peer pool: %w", err)
			}
			return nil
		},
	}, nil
}

// DefaultDir returns ~/.mnemo for the calling process's home.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".mnemo"), nil
}

func loadCertAndKey(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, nil, nil, errors.New("cert.pem: no CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("parse cert: %w", err)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, nil, nil, nil, fmt.Errorf("cert expired at %s", cert.NotAfter.Format(time.RFC3339))
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, nil, nil, errors.New("key.pem: no PEM block")
	}
	var key *ecdsa.PrivateKey
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("parse EC key: %w", err)
		}
	case "PRIVATE KEY":
		ki, perr := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if perr != nil {
			return nil, nil, nil, nil, fmt.Errorf("parse PKCS8 key: %w", perr)
		}
		var ok bool
		key, ok = ki.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("key is not ECDSA: %T", ki)
		}
	default:
		return nil, nil, nil, nil, fmt.Errorf("unsupported key block type %q", keyBlock.Type)
	}

	return cert, key, certPEM, keyPEM, nil
}

func generateAndWrite(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("generate serial: %w", err)
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "mnemo"
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "mnemo-" + hostname,
			Organization: []string{"mnemo"},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("write cert: %w", err)
	}
	if err := writeFileAtomic(keyPath, keyPEM, KeyMode); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("write key: %w", err)
	}

	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("parse generated cert: %w", err)
	}
	return cert, key, certPEM, keyPEM, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".endpoint-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// loadPeers walks <peersDir>/*.pem and adds parseable certs to a pool.
// Malformed entries are skipped with a slog.Warn. Returns a non-nil
// (possibly empty) pool plus the sorted basenames of loaded peers.
func loadPeers(peersDir string) (*x509.CertPool, []string) {
	pool := x509.NewCertPool()
	var names []string

	entries, err := os.ReadDir(peersDir)
	if errors.Is(err, os.ErrNotExist) {
		return pool, names
	}
	if err != nil {
		slog.Warn("read peers dir failed", "dir", peersDir, "err", err)
		return pool, names
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".pem") {
			continue
		}
		path := filepath.Join(peersDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("skip peer cert", "path", path, "err", err)
			continue
		}
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			slog.Warn("skip peer cert: not a CERTIFICATE PEM block", "path", path)
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			slog.Warn("skip peer cert", "path", path, "err", err)
			continue
		}
		pool.AddCert(cert)
		names = append(names, strings.TrimSuffix(name, ".pem"))
	}

	sort.Strings(names)
	return pool, names
}
