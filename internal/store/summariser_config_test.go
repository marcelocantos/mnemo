// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

func TestNormalizeSummariserProvider(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"auto", ""},
		{"grok", "grok"},
		{"Grok", "grok"},
		{"xai", "grok"},
		{"claude", "claude"},
		{"anthropic", "claude"},
	}
	for _, tc := range cases {
		got := normalizeSummariserProvider(tc.in)
		if got != tc.want {
			t.Errorf("normalize(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateSummariserRejectsUnknown(t *testing.T) {
	if err := (Config{Summariser: SummariserConfig{Provider: "openai"}}).validateSummariser(); err == nil {
		t.Fatal("expected reject unknown provider")
	}
	if err := (Config{Summariser: SummariserConfig{Provider: "claude"}}).validateSummariser(); err != nil {
		t.Fatal(err)
	}
}
