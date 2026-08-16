// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

func TestSummariserEffectiveProvider(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "grok"},
		{"grok", "grok"},
		{"Grok", "grok"},
		{"xai", "grok"},
		{"claude", "claude"},
		{"anthropic", "claude"},
	}
	for _, tc := range cases {
		got := (SummariserConfig{Provider: tc.in}).EffectiveProvider()
		if got != tc.want {
			t.Errorf("EffectiveProvider(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateSummariser(t *testing.T) {
	if err := (Config{Summariser: SummariserConfig{Provider: "claude"}}).validateSummariser(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{}).validateSummariser(); err != nil {
		t.Fatal(err)
	}
	if err := (Config{Summariser: SummariserConfig{Provider: "openai"}}).validateSummariser(); err == nil {
		t.Fatal("expected reject unknown provider")
	}
}
