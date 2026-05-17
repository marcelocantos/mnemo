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

	// VaultPath is the directory where mnemo writes its Markdown knowledge
	// graph. When set, mnemo continuously exports sessions, decisions,
	// memories, skills, configs, plans, targets, CI runs, and PRs as
	// Markdown files compatible with Obsidian and Logseq. Human edits
	// below the <!-- mnemo:generated --> fence are preserved on re-sync.
	// Supports ~ for the user's home directory.
	// When absent or empty, vault export is completely disabled.
	VaultPath string `json:"vault_path,omitempty"`

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

// ConfigPath returns the absolute path to ~/.mnemo/config.json for the
// current process user. Returns an error only when the home directory
// cannot be resolved.
func ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mnemo", "config.json"), nil
}

// WriteConfig persists cfg to ~/.mnemo/config.json atomically and after
// passing the same federation-peer validation that LoadConfig applies on
// startup. The write is atomic in the rename-into-place sense: a tmp
// file is written next to the target and renamed once fsync'd, so a
// crashed writer cannot leave a half-formed config visible to a
// subsequent LoadConfig call.
//
// vault_path is trial-balloon validated (see validateVaultPath) before
// the rename so the persisted config is always loadable cleanly — a
// path that vault.New would reject is rejected here too, leaving the
// previous on-disk config intact.
//
// Used by the mnemo_config MCP tool to apply runtime configuration
// changes (chiefly vault_path) without requiring a daemon restart.
func WriteConfig(cfg Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := cfg.validateLinkedInstances(filepath.Join(home, ".mnemo", "peers")); err != nil {
		return err
	}
	if err := cfg.validateVaultPath(home); err != nil {
		return err
	}
	path := filepath.Join(home, ".mnemo", "config.json")
	return writeConfigTo(path, cfg)
}

// validateVaultPath mirrors the only fallible step of vault.New
// (os.MkdirAll on the resolved root) so a bad vault_path is rejected
// before WriteConfig commits the new config to disk. Without this
// check, a write of e.g. {"vault_path": "/dev/null"} succeeds and
// persists; the subsequent Reload's swapVault fails and surfaces a
// Warning, but the on-disk config is already wrong and the next
// daemon start re-hits the failure during initial setup.
//
// home is the daemon's home directory — used to ~-expand vault_path.
// In the common single-user deployment this matches the per-user
// homeDir that Reload's swapVault uses. On a multi-user Windows
// Service install where different users may resolve ~ differently,
// trial-balloon coverage against the daemon home is still enough to
// catch the typical "garbage path" mistake; user-specific resolution
// failures continue to surface as Reload Warnings.
//
// Empty VaultPath is the documented "vault disabled" state and skips
// validation.
func (c Config) validateVaultPath(home string) error {
	resolved := c.ResolvedVaultPath(home)
	if resolved == "" {
		return nil
	}
	if err := os.MkdirAll(resolved, 0o755); err != nil {
		return fmt.Errorf("vault_path %q is not usable: %w", c.VaultPath, err)
	}
	return nil
}

// writeConfigTo is the testable core of WriteConfig: it writes cfg to
// path using a sibling tmp file + rename so concurrent readers always
// observe either the previous or the new file, never a partial write.
// The parent directory is created if missing (mode 0o755).
func writeConfigTo(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json.*")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
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

// validatePeerCert resolves and parses li.PeerCert via the same logic
// as ResolvePeerCert; the validation pass is just "did this resolve".
func validatePeerCert(name, value, peersDir string) error {
	li := LinkedInstance{Name: name, PeerCert: value}
	if _, err := li.ResolvePeerCert(peersDir); err != nil {
		return err
	}
	return nil
}

// ResolvePeerCert returns the parsed X.509 certificate for
// li.PeerCert. If PeerCert contains a "-----BEGIN" marker it is
// treated as inline PEM; otherwise it is treated as a basename to
// resolve under peersDir/<name>.pem. Errors include the instance
// name so they make sense in startup logs.
func (li LinkedInstance) ResolvePeerCert(peersDir string) (*x509.Certificate, error) {
	if looksLikeInlinePEM(li.PeerCert) {
		block, _ := pem.Decode([]byte(li.PeerCert))
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("linked_instances[%q]: inline peer_cert is not a CERTIFICATE PEM block", li.Name)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("linked_instances[%q]: parse inline peer_cert: %w", li.Name, err)
		}
		return cert, nil
	}

	path := filepath.Join(peersDir, li.PeerCert+".pem")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("linked_instances[%q]: peer_cert %q not found at %s: %w", li.Name, li.PeerCert, path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("linked_instances[%q]: peer_cert %s: no CERTIFICATE PEM block", li.Name, path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("linked_instances[%q]: parse peer_cert %s: %w", li.Name, path, err)
	}
	return cert, nil
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

// ResolvedVaultPath returns VaultPath with ~ expanded using userHome.
// Returns "" when VaultPath is not set (vault export disabled).
// userHome is passed in rather than looked up here so ForUser can
// expand ~ relative to the target user's home directory, not the
// daemon's own home (relevant on Windows where LocalSystem runs the
// daemon but a named user's vault path is configured).
func (c Config) ResolvedVaultPath(userHome string) string {
	p := c.VaultPath
	if p == "" {
		return ""
	}
	if userHome != "" {
		switch {
		case p == "~":
			return userHome
		case strings.HasPrefix(p, "~/"):
			return filepath.Join(userHome, p[2:])
		}
	}
	return p
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
