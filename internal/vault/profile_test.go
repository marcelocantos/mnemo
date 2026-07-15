// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package vault

import "testing"

func TestProfileLink(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		target  string
		alias   string
		want    string
	}{
		{"obsidian aliased", ProfileObsidian, "_mnemo/themes/auth-redesign", "Auth redesign", "[[_mnemo/themes/auth-redesign|Auth redesign]]"},
		{"obsidian empty alias", ProfileObsidian, "_mnemo/themes/auth-redesign", "", "[[_mnemo/themes/auth-redesign]]"},
		{"obsidian redundant alias == basename", ProfileObsidian, "_mnemo/themes/auth-redesign", "auth-redesign", "[[_mnemo/themes/auth-redesign]]"},
		{"obsidian strips md suffix", ProfileObsidian, "decisions/x.md", "", "[[decisions/x]]"},
		{"logseq drops alias", ProfileLogseq, "_mnemo/themes/auth-redesign", "Auth redesign", "[[_mnemo/themes/auth-redesign]]"},
		{"foam drops alias", ProfileFoam, "_mnemo/themes/auth-redesign", "Auth redesign", "[[_mnemo/themes/auth-redesign]]"},
		{"generic aliased markdown", ProfileGeneric, "_mnemo/themes/auth-redesign", "Auth redesign", "[Auth redesign](_mnemo/themes/auth-redesign.md)"},
		{"generic no alias falls back to target", ProfileGeneric, "_mnemo/themes/auth-redesign", "", "[_mnemo/themes/auth-redesign](_mnemo/themes/auth-redesign.md)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.profile.Link(tc.target, tc.alias); got != tc.want {
				t.Errorf("%s.Link(%q, %q) = %q, want %q", tc.profile, tc.target, tc.alias, got, tc.want)
			}
		})
	}
}

func TestProfileFrom(t *testing.T) {
	cases := map[string]Profile{
		"obsidian": ProfileObsidian,
		"logseq":   ProfileLogseq,
		"foam":     ProfileFoam,
		"generic":  ProfileGeneric,
		"":         ProfileGeneric,
		"bogus":    ProfileGeneric,
	}
	for in, want := range cases {
		if got := profileFrom(in); got != want {
			t.Errorf("profileFrom(%q) = %q, want %q", in, got, want)
		}
	}
}
