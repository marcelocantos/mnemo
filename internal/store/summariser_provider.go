// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveSummariserProvider picks the summariser CLI provider for this
// process. Explicit config wins; when provider is omitted, prefer Grok if
// the binary is available, else Claude. Decision is pure availability of
// the executable (PATH / GROK_BIN / CLAUDE_BIN / known install dirs) — not
// login/auth probes.
//
// source is "config", "auto:grok", "auto:claude", or "auto:none" (neither
// binary found; still returns "claude" so workers start and surface a clear
// spawn error).
func ResolveSummariserProvider(configured string) (provider, source string) {
	p := normalizeSummariserProvider(configured)
	if p != "" {
		return p, "config"
	}
	if providerBinaryAvailable("grok") {
		return "grok", "auto:grok"
	}
	if providerBinaryAvailable("claude") {
		return "claude", "auto:claude"
	}
	// Neither found: fall through to claude so DisallowTools still apply
	// if the user installs claude later without a restart re-probe — and
	// spawn errors mention a familiar path. Auto still logs "none".
	return "claude", "auto:none"
}

// normalizeSummariserProvider maps config aliases to grok|claude|"".
// Empty means "choose automatically".
func normalizeSummariserProvider(s string) string {
	p := strings.ToLower(strings.TrimSpace(s))
	switch p {
	case "", "auto":
		return ""
	case "grok", "xai":
		return "grok"
	case "claude", "anthropic":
		return "claude"
	default:
		return p // invalid non-empty; validateSummariser rejects
	}
}

// EffectiveProvider returns an explicit provider for validation/display.
// Empty config is treated as the *preference* "grok", not the auto result —
// call ResolveSummariserProvider at startup for the availability-aware choice.
func (c SummariserConfig) EffectiveProvider() string {
	if p := normalizeSummariserProvider(c.Provider); p != "" {
		return p
	}
	return "grok"
}

// providerBinaryAvailable reports whether the named provider CLI can be
// resolved the same way claudia would at Task spawn (env override, PATH,
// then known install dirs).
func providerBinaryAvailable(provider string) bool {
	switch provider {
	case "grok":
		_, err := resolveProviderBin("GROK_BIN", "grok", grokBinCandidates())
		return err == nil
	case "claude":
		_, err := resolveProviderBin("CLAUDE_BIN", "claude", claudeBinCandidates())
		return err == nil
	default:
		return false
	}
}

func resolveProviderBin(envKey, name string, candidates []string) (string, error) {
	if p := os.Getenv(envKey); p != "" {
		if filepath.IsAbs(p) {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		} else if abs, err := exec.LookPath(p); err == nil {
			return abs, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s executable not found (set %s to override)", name, envKey)
}

func grokBinCandidates() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".grok", "bin", "grok"),
		filepath.Join(home, ".local", "bin", "grok"),
		"/opt/homebrew/bin/grok",
		"/usr/local/bin/grok",
	}
}

func claudeBinCandidates() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".local", "bin", "claude"),
		filepath.Join(home, ".claude", "local", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	}
}
