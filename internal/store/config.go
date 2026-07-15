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
	"sort"
	"strings"
	"time"
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

	// ThreadsRoot is the root directory of the Threads feature (🎯T85):
	// a flat collection of per-initiative thread directories, each with a
	// CLAUDE.md context file. Supports ~ for the user's home. Empty
	// resolves to ~/think/threads via ResolvedThreadsRoot. Hot-reloaded
	// like the other discovery roots, so `mnemo_config` can repoint it
	// without a daemon restart. Adding it to SynthesisRoots (or living
	// under one, as the default does beneath ~/think) is what makes thread
	// content searchable via mnemo's existing FTS index.
	ThreadsRoot string `json:"threads_root,omitempty"`

	// MenuBarApp opts in to the macOS menu-bar Threads navigator app
	// (🎯T85). When false (the default), the daemon does NOT auto-launch
	// Mnemo.app. The Threads feature itself stays fully available
	// regardless — via the mnemo_thread_* MCP tools, the `mnemo thread`
	// CLI, and the HTTP thread routes; only the menu-bar button is gated.
	// Set true to have the daemon launch and supervise the app. Applied
	// live: toggling this via mnemo_config starts/stops supervising the app
	// immediately, with no daemon restart. (Disabling won't force-quit a
	// running app — it just won't be relaunched.)
	MenuBarApp bool `json:"menu_bar_app,omitempty"`

	// TodoGlobs are extra repo-relative globs (filepath.Match semantics)
	// that the TODO indexer matches when discovering TODO files (🎯T78),
	// beyond the default TODO.md / todos.md names found at any depth.
	// E.g. ["TASKS.md", "docs/roadmap.md"]. Empty → defaults only.
	TodoGlobs []string `json:"todo_globs,omitempty"`

	// DisableHealthNotifications turns off the self-diagnostics OS
	// notifications (🎯T83). They are opt-out — enabled by default,
	// fail-severity only, local-only (osascript / notify-send). Set true
	// to silence them; the dashboard health page and mnemo_doctor are
	// unaffected.
	DisableHealthNotifications bool `json:"disable_health_notifications,omitempty"`

	// VaultPath is the directory where mnemo writes its Markdown knowledge
	// graph. When set, mnemo continuously exports sessions, decisions,
	// memories, skills, configs, plans, targets, CI runs, and PRs as
	// Markdown files compatible with Obsidian and Logseq. Human edits
	// below the <!-- mnemo:generated --> fence are preserved on re-sync.
	// Supports ~ for the user's home directory.
	// When absent or empty, vault export is completely disabled.
	VaultPath string `json:"vault_path,omitempty"`

	// VaultLayout selects which directory layout the vault exporter writes
	// to (🎯T64.2). One of:
	//   - "v2"   — write under <vault>/_mnemo/ only (the new wing).
	//   - "both" — dual-write <vault>/_mnemo/ AND the v1 root-level dirs
	//              (sessions/, decisions/, …) during the migration window.
	//   - "v1"   — write to v1 root-level dirs only. Emergency escape
	//              hatch; will be removed when the Slice 9 GC tool ships.
	//
	// Empty resolves via ResolvedVaultLayout at runtime: new vaults default
	// to "v2"; pre-existing v1-populated vaults default to "both" to keep
	// existing files coherent during migration. The detection is documented
	// under "Configuration surface" → "Soak-time TTL" in
	// docs/design/vault-library-wing.md.
	VaultLayout string `json:"vault_layout,omitempty"`

	// VaultProfile selects the PKM tool dialect the vault exporter
	// renders for (🎯T64.5). One of "obsidian", "logseq", "foam", or
	// "generic". Controls link syntax (alias wikilinks vs. plain
	// wikilinks vs. Markdown links) and other tool-specific quirks.
	//
	// Empty resolves via ResolvedVaultProfile at runtime: auto-detected
	// from the recency of each tool's canonical signal file, falling
	// back to "generic" when none is present. A user-set value always
	// wins over auto-detect. See "PKM profile" in
	// docs/design/vault-library-wing.md.
	VaultProfile string `json:"vault_profile,omitempty"`

	// VaultBridges maps a vault entity collection name (one of
	// vaultBridgeCollections — "themes", "patterns", "cross-repo",
	// "lessons", "decisions", "memories") to a vault-relative anchor
	// file path anywhere in the vault (🎯T64.6). On each sync mnemo
	// writes a fenced block of links to that collection into the named
	// file, letting users pull mnemo content into their own MOCs
	// without mnemo owning the file. Unknown collection names are
	// skipped with a warning (fail-soft). Empty disables bridges.
	VaultBridges map[string]string `json:"vault_bridges,omitempty"`

	// VaultBridgesMaxLinks caps how many links a single bridge block
	// emits (per source project for the memories bridge) so a bridge
	// never becomes its own hairball (🎯T64.6). Zero resolves to the
	// default (defaultVaultBridgesMaxLinks).
	VaultBridgesMaxLinks int `json:"vault_bridges_max_links,omitempty"`

	// VaultLayoutSoakWarnAfter is the duration (Go time.ParseDuration
	// format) that vault_layout="both" may sit before mnemo emits the
	// weekly "opt into v2" structured warning. Empty defaults to "720h"
	// (30 days). The warning never auto-narrows the layout — it only
	// surfaces the state on mnemo_vault_status and the daemon log.
	VaultLayoutSoakWarnAfter string `json:"vault_layout_soak_warn_after,omitempty"`

	// VaultIndexingScope selects which subtree of the vault mnemo reads
	// during IngestVaultAnnotations (🎯T64.1 — consent fix). One of:
	//   - "_mnemo_only" — only <vault>/_mnemo/ is walked. Below-fence
	//     annotations on generated pages plus user Markdown placed
	//     inside the wing surface in mnemo_search; nothing outside the
	//     wing is touched.
	//   - "full"        — the entire vault is walked (hidden dirs like
	//     .obsidian/, .git/, .trash/ excluded). Matches v1 behaviour.
	//   - "includes"    — <vault>/_mnemo/ plus each path listed in
	//     VaultIndexingIncludes is walked.
	//
	// Empty resolves via ResolvedVaultIndexingScope at runtime: new
	// vaults default to "_mnemo_only" (the safest scope), pre-existing
	// v1-populated vaults default to "full" for continuity. The detection
	// is documented under "Indexing scope" in docs/design/vault-library-wing.md.
	VaultIndexingScope string `json:"vault_indexing_scope,omitempty"`

	// VaultIndexingIncludes lists vault-relative paths walked in
	// addition to <vault>/_mnemo/ when VaultIndexingScope == "includes".
	// Ignored under "_mnemo_only" or "full". Paths use forward slashes;
	// `..` and absolute paths are rejected at validation.
	VaultIndexingIncludes []string `json:"vault_indexing_includes,omitempty"`

	// VaultIndexingIgnoreFile is the vault-relative path to a
	// gitignore-syntax file applied across the configured scope
	// (including inside _mnemo/). Empty defaults to ".mnemoignore"; an
	// absent file means no extra exclusions. Only this single file is
	// consulted — nested .mnemoignore files are not honoured.
	VaultIndexingIgnoreFile string `json:"vault_indexing_ignore_file,omitempty"`

	// LinkedInstances declares peer mnemo endpoints to federate with
	// (🎯T15). Each peer is identified by a https URL and a trusted
	// peer certificate (either a name resolved under ~/.mnemo/peers/
	// or an inline PEM). An absent or empty list disables federation
	// entirely; the daemon makes no outbound peer calls.
	LinkedInstances []LinkedInstance `json:"linked_instances,omitempty"`

	// Backup controls the daemon's periodic backup worker (🎯T61).
	// Absent in config.json → all defaults apply, backup enabled.
	Backup BackupConfig `json:"backup,omitempty"`

	// CostReconciliation controls the Anthropic Admin API reconciler
	// (🎯T63). Disabled by default: even with ANTHROPIC_ADMIN_API_KEY
	// set in the environment, no outbound Admin API call is made until
	// the user explicitly opts in via this config block. This protects
	// users in security-reviewed environments where unsolicited egress
	// to hosted APIs is undesirable. The estimated-cost path (derived
	// from transcript tokens) is always on and requires zero external
	// calls.
	CostReconciliation CostReconciliationConfig `json:"cost_reconciliation,omitempty"`

	// ConnectionSweep controls the daemon_connections sweeper
	// (🎯T60). Absent in config.json → defaults apply, sweeper
	// enabled. Set {"connection_sweep": {"disabled": true}} to opt
	// out (the open-row count will then grow unbounded as it did
	// before — accepted only if some external mechanism reaps).
	ConnectionSweep ConnectionSweepConfig `json:"connection_sweep,omitempty"`

	// DisableUpgradeCheck turns off the periodic GitHub release poll
	// that powers the upgrade.available diag check (🎯T97.2). The
	// check is on by default (opt-out, like health notifications and
	// the always-on gh-backed PR/CI mirrors). When true, zero outbound
	// release-list calls are made.
	DisableUpgradeCheck bool `json:"disable_upgrade_check,omitempty"`

	// AutoUpgrade controls opt-in connection-preserving auto-apply
	// (🎯T97.5). Zero value / absent → disabled (notify-only via
	// upgrade.available). When enabled, only Homebrew non-Windows
	// installs actually apply; others stay notify-only.
	AutoUpgrade AutoUpgradeConfig `json:"auto_upgrade,omitempty"`
}

// AutoUpgradeConfig gates automatic backend swaps after quiescence.
type AutoUpgradeConfig struct {
	// Enabled opts in to auto-apply. Default false — detection and
	// notification still work via upgrade.available when the upgrade
	// check is not disabled.
	Enabled bool `json:"enabled,omitempty"`

	// Quiescence is how long MCP traffic must be idle before apply
	// (Go duration string). Empty → "5m".
	Quiescence string `json:"quiescence,omitempty"`
}

// EffectiveQuiescence returns the parsed idle window or 5m.
func (a AutoUpgradeConfig) EffectiveQuiescence() (time.Duration, error) {
	if a.Quiescence == "" {
		return 5 * time.Minute, nil
	}
	d, err := time.ParseDuration(a.Quiescence)
	if err != nil {
		return 0, fmt.Errorf("auto_upgrade.quiescence: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("auto_upgrade.quiescence: must be non-negative, got %v", d)
	}
	return d, nil
}

// ConnectionSweepConfig controls how often the daemon checks for
// stale daemon_connections rows and how long a row must be idle
// before being marked closed. A zero-value ConnectionSweepConfig
// (i.e. the section absent from config.json) means enabled, sweep
// every minute, idle threshold 10 minutes.
type ConnectionSweepConfig struct {
	// Disabled opts out of the sweeper. Negated form so the zero
	// value (absent section) means enabled.
	Disabled bool `json:"disabled,omitempty"`

	// Interval between sweep ticks. Format: a Go time.ParseDuration
	// string. Empty → "1m".
	Interval string `json:"interval,omitempty"`

	// StaleAfter is the duration since last_seen_at after which a
	// connection is considered stale and marked closed. Format: a
	// Go time.ParseDuration string. Empty → "10m".
	StaleAfter string `json:"stale_after,omitempty"`
}

// IsEnabled reports whether the sweeper should run. Defaults to true;
// only an explicit `"disabled": true` opts out.
func (c ConnectionSweepConfig) IsEnabled() bool { return !c.Disabled }

// EffectiveInterval returns the parsed sweep interval, or 1 minute
// when unset.
func (c ConnectionSweepConfig) EffectiveInterval() (time.Duration, error) {
	if c.Interval == "" {
		return time.Minute, nil
	}
	d, err := time.ParseDuration(c.Interval)
	if err != nil {
		return 0, fmt.Errorf("interval: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("interval must be positive, got %v", d)
	}
	return d, nil
}

// EffectiveStaleAfter returns the parsed idle threshold, or 10 minutes
// when unset.
func (c ConnectionSweepConfig) EffectiveStaleAfter() (time.Duration, error) {
	if c.StaleAfter == "" {
		return 10 * time.Minute, nil
	}
	d, err := time.ParseDuration(c.StaleAfter)
	if err != nil {
		return 0, fmt.Errorf("stale_after: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("stale_after must be positive, got %v", d)
	}
	return d, nil
}

// CostReconciliationConfig gates the Anthropic Admin API reconciler.
// A zero-value CostReconciliationConfig (i.e. the section omitted from
// config.json) means disabled — opposite of BackupConfig's safety
// default, because the safe default for unsolicited outbound API calls
// is "do not call".
type CostReconciliationConfig struct {
	// Enabled opts in to the reconciler. When false (the zero value
	// and documented default), StartReconciler exits immediately and
	// makes zero Admin API calls regardless of whether
	// ANTHROPIC_ADMIN_API_KEY is set in the daemon's environment.
	Enabled bool `json:"enabled,omitempty"`
}

// IsEnabled reports whether the reconciler should run. False by
// default (zero-value config = no Admin API calls).
func (c CostReconciliationConfig) IsEnabled() bool { return c.Enabled }

// BackupConfig controls the periodic backup worker. Field defaults are
// resolved via the Effective* methods so a zero-value BackupConfig (the
// state when config.json omits the "backup" section entirely) gets
// sensible behaviour: enabled, ~/.mnemo/backups, 7 dailies, 03:00–04:00
// local window, 15 min quiescence threshold.
type BackupConfig struct {
	// Disabled opts out of periodic backups. Negated form chosen so
	// the zero value (BackupConfig{}, what you get when config.json
	// omits the "backup" key entirely) means enabled — backups are the
	// safe default.
	Disabled bool `json:"disabled,omitempty"`

	// Dir is the backup directory. Supports ~ for the user's home.
	// Empty → ~/.mnemo/backups.
	Dir string `json:"dir,omitempty"`

	// KeepDailies caps the number of snapshots retained. Older
	// backups beyond this count are deleted after each successful run.
	// 0 or unset → 7.
	KeepDailies int `json:"keep_dailies,omitempty"`

	// WindowStart and WindowEnd bound the local time-of-day during
	// which the worker may take its daily snapshot. Format "HH:MM"
	// (24h). Empty → "03:00" / "04:00".
	WindowStart string `json:"window_start,omitempty"`
	WindowEnd   string `json:"window_end,omitempty"`

	// QuiescenceMin is the minimum time since the last recorded write
	// activity (Store.NoteActivity) before the worker will snapshot.
	// Format: a Go time.ParseDuration string. Empty → "15m".
	QuiescenceMin string `json:"quiescence_min,omitempty"`
}

// IsEnabled reports whether the periodic backup worker should run.
// Defaults to true; only an explicit `"disabled": true` opts out.
func (b BackupConfig) IsEnabled() bool { return !b.Disabled }

// EffectiveDir returns Dir with ~ expanded, or ~/.mnemo/backups when
// Dir is empty. userHome is the home directory used for expansion.
func (b BackupConfig) EffectiveDir(userHome string) string {
	d := b.Dir
	if d == "" {
		return filepath.Join(userHome, ".mnemo", "backups")
	}
	if userHome != "" {
		switch {
		case d == "~":
			return userHome
		case strings.HasPrefix(d, "~/"):
			return filepath.Join(userHome, d[2:])
		}
	}
	return d
}

// EffectiveKeepDailies returns KeepDailies or 7 when unset.
func (b BackupConfig) EffectiveKeepDailies() int {
	if b.KeepDailies > 0 {
		return b.KeepDailies
	}
	return 7
}

// EffectiveWindow returns the [start, end) local time-of-day window for
// the daily backup attempt. Returns parsed time.Duration offsets from
// midnight rather than parsed times, since the worker needs to compute
// "next 03:17 today or tomorrow" against the wall clock.
//
// Returns an error if WindowStart/WindowEnd are set but malformed.
// Defaults: 3h, 4h (03:00, 04:00).
func (b BackupConfig) EffectiveWindow() (start, end time.Duration, err error) {
	start, err = parseHHMM(b.WindowStart, 3*time.Hour)
	if err != nil {
		return 0, 0, fmt.Errorf("window_start: %w", err)
	}
	end, err = parseHHMM(b.WindowEnd, 4*time.Hour)
	if err != nil {
		return 0, 0, fmt.Errorf("window_end: %w", err)
	}
	if end <= start {
		return 0, 0, fmt.Errorf("window_end (%v) must be > window_start (%v)", end, start)
	}
	return start, end, nil
}

// EffectiveQuiescenceMin returns the parsed quiescence threshold, or
// 15 minutes when unset. Errors are surfaced so config validation can
// catch a typo'd value at write time.
func (b BackupConfig) EffectiveQuiescenceMin() (time.Duration, error) {
	if b.QuiescenceMin == "" {
		return 15 * time.Minute, nil
	}
	d, err := time.ParseDuration(b.QuiescenceMin)
	if err != nil {
		return 0, fmt.Errorf("quiescence_min: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("quiescence_min: must be non-negative, got %v", d)
	}
	return d, nil
}

// parseHHMM parses "HH:MM" (24-hour, leading zeros optional) and
// returns the offset from midnight. Empty input returns dflt.
func parseHHMM(s string, dflt time.Duration) (time.Duration, error) {
	if s == "" {
		return dflt, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	h, err := parseUintBounded(parts[0], 0, 23)
	if err != nil {
		return 0, fmt.Errorf("hour: %w", err)
	}
	m, err := parseUintBounded(parts[1], 0, 59)
	if err != nil {
		return 0, fmt.Errorf("minute: %w", err)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

func parseUintBounded(s string, lo, hi int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit in %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d out of range [%d,%d]", n, lo, hi)
	}
	return n, nil
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
	home, err := EffectiveHome()
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
	home, err := EffectiveHome()
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
	home, err := EffectiveHome()
	if err != nil {
		return err
	}
	if err := cfg.validateLinkedInstances(filepath.Join(home, ".mnemo", "peers")); err != nil {
		return err
	}
	if err := cfg.validateVaultPath(home); err != nil {
		return err
	}
	if err := cfg.validateVaultLayout(); err != nil {
		return err
	}
	if err := cfg.validateVaultProfile(); err != nil {
		return err
	}
	path := filepath.Join(home, ".mnemo", "config.json")
	return writeConfigTo(path, cfg)
}

// validateVaultProfile rejects unknown vault_profile values so a typo
// (e.g. "obsidan", "Logseq") never persists into config.json. Empty is
// permitted — ResolvedVaultProfile auto-detects at runtime. (🎯T64.5)
func (c Config) validateVaultProfile() error {
	switch c.VaultProfile {
	case "", VaultProfileObsidian, VaultProfileLogseq, VaultProfileFoam, VaultProfileGeneric:
		return nil
	default:
		return fmt.Errorf("vault_profile %q is invalid; must be one of %q, %q, %q, %q",
			c.VaultProfile, VaultProfileObsidian, VaultProfileLogseq, VaultProfileFoam, VaultProfileGeneric)
	}
}

// validateVaultLayout rejects unknown vault_layout values so a typo
// (e.g. "v3", "Both") never persists into config.json. Empty is
// permitted — the resolver picks a default at runtime.
func (c Config) validateVaultLayout() error {
	switch c.VaultLayout {
	case "", VaultLayoutV1, VaultLayoutBoth, VaultLayoutV2:
		// nil check is on VaultLayoutSoakWarnAfter format.
	default:
		return fmt.Errorf("vault_layout %q is invalid; must be one of \"v1\", \"both\", \"v2\"", c.VaultLayout)
	}
	if c.VaultLayoutSoakWarnAfter != "" {
		d, err := time.ParseDuration(c.VaultLayoutSoakWarnAfter)
		if err != nil {
			return fmt.Errorf("vault_layout_soak_warn_after: %w", err)
		}
		if d <= 0 {
			return fmt.Errorf("vault_layout_soak_warn_after must be positive, got %v", d)
		}
	}
	return nil
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
	home, err := EffectiveHome()
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

// Vault indexing scope constants. Use these instead of bare string
// literals so a typo surfaces at compile time.
const (
	VaultIndexingScopeMnemoOnly = "_mnemo_only"
	VaultIndexingScopeFull      = "full"
	VaultIndexingScopeIncludes  = "includes"
)

// Vault layout constants (🎯T64.2). Use these instead of bare strings.
const (
	VaultLayoutV1   = "v1"
	VaultLayoutBoth = "both"
	VaultLayoutV2   = "v2"
)

// Vault PKM profile constants (🎯T64.5). Select the tool-specific
// rendering dialect for the vault. Use these instead of bare strings.
const (
	VaultProfileObsidian = "obsidian"
	VaultProfileLogseq   = "logseq"
	VaultProfileFoam     = "foam"
	VaultProfileGeneric  = "generic"
)

// vaultProfileSignalFiles maps a PKM profile to the canonical file
// whose modification time signals that the tool is actively used
// against this vault (🎯T64.5). Auto-detect stats these — recency of
// mtime, not mere presence, decides the profile, because a stale
// `.obsidian/` can linger for months after one exploratory open while
// a multi-tool user's real editor keeps touching its own config.
//
// generic has no signal file: it is the fallback when none of these
// exist.
var vaultProfileSignalFiles = map[string]string{
	VaultProfileObsidian: filepath.Join(".obsidian", "workspace.json"),
	VaultProfileLogseq:   filepath.Join("logseq", "config", "config.edn"),
	VaultProfileFoam:     filepath.Join(".foam", "settings.json"),
}

// vaultProfileTieBreak is the deterministic preference order applied
// when two signal files' mtimes are within vaultProfileTieWindow of
// each other: obsidian (most common) → logseq → foam. (🎯T64.5)
var vaultProfileTieBreak = []string{
	VaultProfileObsidian, VaultProfileLogseq, VaultProfileFoam,
}

// vaultProfileTieWindow is the mtime proximity within which two signal
// files are considered "simultaneously touched" and the deterministic
// tie-break order applies rather than raw most-recent-wins. (🎯T64.5)
const vaultProfileTieWindow = time.Hour

// vaultBridgeCollections is the closed set of entity collection names a
// bridge may target (🎯T64.6). A vault_bridges entry keyed outside this
// set is a typo or a future/unknown collection and is skipped fail-soft.
var vaultBridgeCollections = []string{
	"themes", "patterns", "cross-repo", "lessons", "decisions", "memories",
}

// defaultVaultBridgesMaxLinks caps a bridge block's link count when the
// user has not set vault_bridges_max_links. (🎯T64.6)
const defaultVaultBridgesMaxLinks = 50

// IsVaultBridgeCollection reports whether name is a recognised bridge
// target collection. (🎯T64.6)
func IsVaultBridgeCollection(name string) bool {
	for _, c := range vaultBridgeCollections {
		if c == name {
			return true
		}
	}
	return false
}

// ResolvedVaultBridgesMaxLinks returns the effective per-bridge link cap:
// the configured value when positive, else defaultVaultBridgesMaxLinks.
// (🎯T64.6)
func (c Config) ResolvedVaultBridgesMaxLinks() int {
	if c.VaultBridgesMaxLinks > 0 {
		return c.VaultBridgesMaxLinks
	}
	return defaultVaultBridgesMaxLinks
}

// defaultVaultLayoutSoakWarnAfter is the soak window before the
// "both"-layout warning fires. 30 days, hours-based per the design's
// state-machine spec.
const defaultVaultLayoutSoakWarnAfter = 720 * time.Hour

// defaultVaultIgnoreFile is the conventional name of the gitignore-
// syntax exclude file at the vault root.
const defaultVaultIgnoreFile = ".mnemoignore"

// v1VaultMarkerDirs are the root-level subdirectories the v1 vault
// layout writes to. The presence of any of these without a sibling
// `_mnemo/` indicates a pre-Slice 1 vault that should default to
// "full" indexing scope for continuity.
var v1VaultMarkerDirs = []string{
	"sessions", "decisions", "memories", "skills", "configs",
	"plans", "targets", "ci", "prs", "repos",
}

// HasV1Leftovers reports whether the vault at resolvedVaultPath has
// any v1 root-level marker directories (sessions/, decisions/, ...).
// Returns false on empty path or any stat error. Used by the
// "run gc_legacy" recommendation in mnemo_vault_status — a vault
// running under vault_layout="v2" with v1 dirs still present is the
// canonical "ready to clean up" state. (🎯T64.2)
func HasV1Leftovers(resolvedVaultPath string) bool {
	if resolvedVaultPath == "" {
		return false
	}
	for _, d := range v1VaultMarkerDirs {
		if fi, err := os.Stat(filepath.Join(resolvedVaultPath, d)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// ResolvedVaultIndexingScope returns the effective indexing scope for
// the vault at resolvedVaultPath. When VaultIndexingScope is set, it
// wins. Otherwise the default is computed by inspecting the vault:
//
//   - <vault>/_mnemo/ exists                       → "_mnemo_only"
//   - any v1 marker dir exists (sessions/, ...)    → "full"  (continuity)
//   - otherwise (empty or missing directory)       → "_mnemo_only"
//
// An empty resolvedVaultPath returns "_mnemo_only" — the call site is
// responsible for not invoking the walker when vault is disabled.
func (c Config) ResolvedVaultIndexingScope(resolvedVaultPath string) string {
	switch c.VaultIndexingScope {
	case VaultIndexingScopeMnemoOnly, VaultIndexingScopeFull, VaultIndexingScopeIncludes:
		return c.VaultIndexingScope
	}
	if resolvedVaultPath == "" {
		return VaultIndexingScopeMnemoOnly
	}
	if fi, err := os.Stat(filepath.Join(resolvedVaultPath, "_mnemo")); err == nil && fi.IsDir() {
		return VaultIndexingScopeMnemoOnly
	}
	for _, d := range v1VaultMarkerDirs {
		if fi, err := os.Stat(filepath.Join(resolvedVaultPath, d)); err == nil && fi.IsDir() {
			return VaultIndexingScopeFull
		}
	}
	return VaultIndexingScopeMnemoOnly
}

// ResolvedVaultLayout returns the effective vault layout for the vault
// at resolvedVaultPath. When VaultLayout is set to a recognised value,
// it wins. Otherwise the default is computed by inspecting the vault:
//
//   - <vault>/_mnemo/ exists                       → "v2"  (already on the wing)
//   - any v1 marker dir exists (sessions/, ...)    → "both" (migrate from v1)
//   - otherwise (empty or missing directory)       → "v2"
//
// An empty resolvedVaultPath returns "v2" — the call site is responsible
// for not invoking the writer when vault is disabled.
func (c Config) ResolvedVaultLayout(resolvedVaultPath string) string {
	switch c.VaultLayout {
	case VaultLayoutV1, VaultLayoutBoth, VaultLayoutV2:
		return c.VaultLayout
	}
	if resolvedVaultPath == "" {
		return VaultLayoutV2
	}
	if fi, err := os.Stat(filepath.Join(resolvedVaultPath, "_mnemo")); err == nil && fi.IsDir() {
		return VaultLayoutV2
	}
	for _, d := range v1VaultMarkerDirs {
		if fi, err := os.Stat(filepath.Join(resolvedVaultPath, d)); err == nil && fi.IsDir() {
			return VaultLayoutBoth
		}
	}
	return VaultLayoutV2
}

// VaultProfileDetection is the outcome of profile resolution, carrying
// enough provenance for mnemo_vault_status to explain *why* a profile
// was chosen (🎯T64.5).
type VaultProfileDetection struct {
	// Profile is the resolved profile ("obsidian" | "logseq" | "foam"
	// | "generic").
	Profile string
	// Source is how Profile was decided: "config" (user override),
	// "auto" (a signal file was found), or "default" (no signal →
	// generic).
	Source string
	// SignalFile is the vault-relative signal path that won auto-detect;
	// empty for config/default sources.
	SignalFile string
	// SignalMtime is the modification time of SignalFile; zero when
	// SignalFile is empty.
	SignalMtime time.Time
	// Alternatives lists the other profiles whose signal files also
	// exist (sorted by descending mtime), so the user can spot a
	// mis-detection. Empty for config/default sources.
	Alternatives []string
}

// DetectVaultProfile resolves the effective PKM profile for the vault at
// resolvedVaultPath and returns the full provenance (🎯T64.5).
//
// A user-set VaultProfile always wins (Source "config"). Otherwise the
// signal files are stat'd and the rule from docs/design/vault-library-wing.md
// applies:
//
//  1. drop signal files that do not exist;
//  2. exactly one present → that profile;
//  3. multiple present → most-recent mtime wins; ties within
//     vaultProfileTieWindow break by vaultProfileTieBreak order;
//  4. none present → "generic".
//
// An empty resolvedVaultPath yields generic (the caller is responsible
// for not exporting when the vault is disabled).
func (c Config) DetectVaultProfile(resolvedVaultPath string) VaultProfileDetection {
	if p := c.VaultProfile; p != "" {
		return VaultProfileDetection{Profile: p, Source: "config"}
	}
	if resolvedVaultPath == "" {
		return VaultProfileDetection{Profile: VaultProfileGeneric, Source: "default"}
	}

	type sig struct {
		profile string
		rel     string
		mtime   time.Time
	}
	var found []sig
	for profile, rel := range vaultProfileSignalFiles {
		fi, err := os.Stat(filepath.Join(resolvedVaultPath, rel))
		if err != nil || fi.IsDir() {
			continue
		}
		found = append(found, sig{profile: profile, rel: rel, mtime: fi.ModTime()})
	}
	if len(found) == 0 {
		return VaultProfileDetection{Profile: VaultProfileGeneric, Source: "default"}
	}

	// Sort most-recent first; within the tie window, order by the
	// deterministic tie-break preference so the result is stable across
	// runs and filesystems regardless of map iteration order.
	sort.SliceStable(found, func(i, j int) bool {
		di := found[i].mtime.Sub(found[j].mtime)
		if di > vaultProfileTieWindow {
			return true
		}
		if di < -vaultProfileTieWindow {
			return false
		}
		return tieBreakRank(found[i].profile) < tieBreakRank(found[j].profile)
	})

	winner := found[0]
	alts := make([]string, 0, len(found)-1)
	for _, s := range found[1:] {
		alts = append(alts, s.profile)
	}
	return VaultProfileDetection{
		Profile:      winner.profile,
		Source:       "auto",
		SignalFile:   winner.rel,
		SignalMtime:  winner.mtime,
		Alternatives: alts,
	}
}

// tieBreakRank returns the index of profile in vaultProfileTieBreak, or
// a large sentinel for anything unlisted so it sorts last. (🎯T64.5)
func tieBreakRank(profile string) int {
	for i, p := range vaultProfileTieBreak {
		if p == profile {
			return i
		}
	}
	return len(vaultProfileTieBreak)
}

// ResolvedVaultProfile returns just the effective profile string for the
// vault at resolvedVaultPath. Convenience wrapper over DetectVaultProfile
// for call sites that do not need the detection provenance. (🎯T64.5)
func (c Config) ResolvedVaultProfile(resolvedVaultPath string) string {
	return c.DetectVaultProfile(resolvedVaultPath).Profile
}

// ResolvedVaultLayoutSoakWarnAfter returns the configured soak duration
// for the "both" → "opt into v2" recommendation, or 720h when unset.
// An unparseable or non-positive value also falls back to the default
// so a corrupted config never silently disables the warning.
func (c Config) ResolvedVaultLayoutSoakWarnAfter() time.Duration {
	if c.VaultLayoutSoakWarnAfter == "" {
		return defaultVaultLayoutSoakWarnAfter
	}
	d, err := time.ParseDuration(c.VaultLayoutSoakWarnAfter)
	if err != nil || d <= 0 {
		return defaultVaultLayoutSoakWarnAfter
	}
	return d
}

// ResolvedVaultIndexingIgnoreFile returns the configured ignore-file
// basename, or ".mnemoignore" when unset.
func (c Config) ResolvedVaultIndexingIgnoreFile() string {
	if c.VaultIndexingIgnoreFile != "" {
		return c.VaultIndexingIgnoreFile
	}
	return defaultVaultIgnoreFile
}

// DefaultThreadsRoot returns the default Threads root: ~/think/threads.
func DefaultThreadsRoot() string {
	home, err := EffectiveHome()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "think", "threads")
}

// ResolvedThreadsRoot returns ThreadsRoot with ~ expanded, or
// DefaultThreadsRoot (~/think/threads) when unset.
func (c Config) ResolvedThreadsRoot() string {
	p := c.ThreadsRoot
	if p == "" {
		return DefaultThreadsRoot()
	}
	home, _ := EffectiveHome()
	if home != "" {
		switch {
		case p == "~":
			return home
		case strings.HasPrefix(p, "~/"):
			return filepath.Join(home, p[2:])
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
	home, _ := EffectiveHome()
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
