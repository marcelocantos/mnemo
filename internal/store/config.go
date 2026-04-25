// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config holds runtime configuration loaded from ~/.mnemo/config.json.
//
// This file is optional. If absent, sensible defaults apply. Its purpose
// is to let the daemon discover repos and project directories that live
// outside the places mnemo would guess on its own (~/work for repos,
// ~/.claude/projects for transcripts).
type Config struct {
	// WorkspaceRoots are filesystem roots under which repo-level streams
	// (targets, audit logs, plans, CLAUDE.md, CI) discover repositories.
	// Each root is walked for .git entries to identify repos. An empty
	// list falls back to DefaultWorkspaceRoots.
	WorkspaceRoots []string `json:"workspace_roots,omitempty"`

	// ExtraProjectDirs lists extra Claude Code project directories to
	// index beyond ~/.claude/projects/. Used for cross-platform
	// transcript ingest (🎯T15) — e.g. a Windows VM's Claude projects
	// dir exposed via SMB mount. Missing or unavailable entries are
	// skipped at ingest/watch time rather than failing.
	ExtraProjectDirs []string `json:"extra_project_dirs,omitempty"`

	// SynthesisRoots are filesystem roots walked by the synthesis-doc
	// indexer (🎯T34) to index analysis/research/design/planning docs
	// under docs/{papers,design,analysis,plans}/ plus docs/audit-log.md
	// and docs/convergence-report.md. Unlike WorkspaceRoots, these roots
	// do not require a .git marker — suitable for non-repo planning
	// spaces such as ~/think. Entries support ~ for the user's home.
	// An empty list disables synthesis-doc ingest (repo-level docs are
	// still indexed via WorkspaceRoots + IngestDocs).
	SynthesisRoots []string `json:"synthesis_roots,omitempty"`

	// LinkedInstances declares peer mnemo endpoints to federate with
	// (🎯T15). Each peer is identified by a https URL and a trusted
	// peer certificate (either a name resolved under ~/.mnemo/peers/
	// or an inline PEM). An absent or empty list disables federation
	// entirely; the daemon makes no outbound peer calls.
	LinkedInstances []LinkedInstance `json:"linked_instances,omitempty"`
}

// LinkedInstance is one peer mnemo endpoint the daemon may query.
type LinkedInstance struct {
	// Name uniquely identifies the peer. Used for log lines and to
	// attribute federated query results back to the source peer.
	Name string `json:"name"`

	// URL is the peer's MCP endpoint, https only.
	URL string `json:"url"`

	// PeerCert is either the bare basename of a file under
	// ~/.mnemo/peers/ (e.g. "alice" → ~/.mnemo/peers/alice.pem) or an
	// inline PEM-encoded X.509 certificate. The first form is the
	// usual case; inline PEM is for small deployments that want
	// everything in one config file.
	PeerCert string `json:"peer_cert"`
}

// LoadConfig reads ~/.mnemo/config.json. Returns a zero Config if the
// file doesn't exist. Federation peers (LinkedInstances) are validated
// against ~/.mnemo/peers/; any structural problem (duplicate name,
// non-https URL, unresolvable peer cert) returns an error so startup
// fails loud rather than silently disabling federation.
func LoadConfig() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	cfg, err := loadConfigFrom(filepath.Join(home, ".mnemo", "config.json"))
	if err != nil {
		return Config{}, err
	}
	if err := cfg.validateLinkedInstances(filepath.Join(home, ".mnemo", "peers")); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func loadConfigFrom(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateLinkedInstances enforces the rules documented on
// LinkedInstance: unique names, https-only URLs, and resolvable
// peer certificates (either as a name under peersDir or as inline
// PEM that parses as an X.509 certificate). Returns the first
// violation encountered; the error message names the offending entry
// (or pair) so the user can correct config.json without grep.
func (c Config) validateLinkedInstances(peersDir string) error {
	seen := map[string]int{}
	for i, li := range c.LinkedInstances {
		if li.Name == "" {
			return fmt.Errorf("linked_instances[%d]: name is required", i)
		}
		if prev, dup := seen[li.Name]; dup {
			return fmt.Errorf("linked_instances: duplicate name %q at indexes %d and %d", li.Name, prev, i)
		}
		seen[li.Name] = i

		if li.URL == "" {
			return fmt.Errorf("linked_instances[%q]: url is required", li.Name)
		}
		u, err := url.Parse(li.URL)
		if err != nil {
			return fmt.Errorf("linked_instances[%q]: parse url %q: %w", li.Name, li.URL, err)
		}
		if u.Scheme != "https" {
			return fmt.Errorf("linked_instances[%q]: url scheme must be https, got %q", li.Name, u.Scheme)
		}

		if li.PeerCert == "" {
			return fmt.Errorf("linked_instances[%q]: peer_cert is required", li.Name)
		}
		if err := validatePeerCert(li.Name, li.PeerCert, peersDir); err != nil {
			return err
		}
	}
	return nil
}

// validatePeerCert resolves a PeerCert value: if it parses as PEM
// containing a CERTIFICATE block we treat it as inline; otherwise we
// look it up under peersDir/<value>.pem. Either path must produce a
// valid X.509 certificate.
func validatePeerCert(name, value, peersDir string) error {
	if looksLikeInlinePEM(value) {
		block, _ := pem.Decode([]byte(value))
		if block == nil || block.Type != "CERTIFICATE" {
			return fmt.Errorf("linked_instances[%q]: inline peer_cert is not a CERTIFICATE PEM block", name)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("linked_instances[%q]: parse inline peer_cert: %w", name, err)
		}
		return nil
	}

	path := filepath.Join(peersDir, value+".pem")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("linked_instances[%q]: peer_cert %q not found at %s: %w", name, value, path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("linked_instances[%q]: peer_cert %s: no CERTIFICATE PEM block", name, path)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return fmt.Errorf("linked_instances[%q]: parse peer_cert %s: %w", name, path, err)
	}
	return nil
}

func looksLikeInlinePEM(s string) bool {
	return strings.Contains(s, "-----BEGIN")
}

// DefaultWorkspaceRoots returns the default workspace roots: [~/work].
// This matches the convention used across the global CLAUDE.md for
// Go-style repo layouts (~/work/github.com/org/repo).
func DefaultWorkspaceRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, "work")}
}

// ResolvedWorkspaceRoots returns the WorkspaceRoots as configured, or
// DefaultWorkspaceRoots if none are set.
func (c Config) ResolvedWorkspaceRoots() []string {
	if len(c.WorkspaceRoots) == 0 {
		return DefaultWorkspaceRoots()
	}
	return c.WorkspaceRoots
}

// ResolvedSynthesisRoots returns SynthesisRoots with ~ expanded to the
// user's home directory. Unset entries return an empty slice (the
// indexer skips synthesis ingest entirely when no roots are configured;
// there is no default, unlike WorkspaceRoots).
func (c Config) ResolvedSynthesisRoots() []string {
	if len(c.SynthesisRoots) == 0 {
		return nil
	}
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(c.SynthesisRoots))
	for _, r := range c.SynthesisRoots {
		if r == "" {
			continue
		}
		if home != "" {
			switch {
			case r == "~":
				r = home
			case strings.HasPrefix(r, "~/"):
				r = filepath.Join(home, r[2:])
			}
		}
		out = append(out, r)
	}
	return out
}
