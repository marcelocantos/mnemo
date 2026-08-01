// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

type fakeLabeller struct {
	label string
	calls int
}

func (f *fakeLabeller) Label(_ context.Context, excerpts []string) (string, error) {
	f.calls++
	return f.label, nil
}

func TestNormalizeLLMLabel(t *testing.T) {
	cases := map[string]string{
		"  auth middleware redesign. ":      "Auth Middleware Redesign",
		"one two three four five six seven": "One Two Three Four Five Six",
		"\"quoted label\"":                  "Quoted Label",
	}
	for in, want := range cases {
		if got := normalizeLLMLabel(in); got != want {
			t.Errorf("normalizeLLMLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplyLLMLabelsReplacesBigramOnly: an LLM label overrides a bigram
// label but never a user-anchored one.
func TestApplyLLMLabelsReplacesBigramOnly(t *testing.T) {
	prev := labelProviderFactory
	fake := &fakeLabeller{label: "auth middleware redesign"}
	labelProviderFactory = func(p ClusterParams) (LabelProvider, bool, string) {
		if p.LabelEngine != "llm" {
			return nil, false, ""
		}
		return fake, true, ""
	}
	t.Cleanup(func() { labelProviderFactory = prev })

	s := newTestStore(t, t.TempDir())
	clusters := []themeCluster{
		{members: []int{0}, label: "Old Bigram", slug: "old-bigram", labelSource: labelSourceBigram, simTo: map[int]float64{0: 1}},
		{members: []int{1}, label: "My Note", slug: "my-note", labelSource: labelSourceVaultUser, simTo: map[int]float64{1: 1}},
	}
	docs := []ClusterCorpusDoc{
		{Kind: "decision", EntityID: "1", Text: "some auth text"},
		{Kind: "vault_user", EntityID: "/v/My Note.md", Text: "user body"},
	}

	p := DefaultClusterParams()
	p.LabelEngine = "llm"
	s.applyLLMLabels(clusters, docs, p)

	if clusters[0].label != "Auth Middleware Redesign" || clusters[0].labelSource != labelSourceLLM {
		t.Errorf("bigram theme not relabelled by LLM: %+v", clusters[0])
	}
	if clusters[1].label != "My Note" || clusters[1].labelSource != labelSourceVaultUser {
		t.Errorf("user-anchored label was overridden: %+v", clusters[1])
	}
}

// TestApplyLLMLabelsNoOpWhenOff: with the default bigram config, no
// labeller is constructed and labels are untouched.
func TestApplyLLMLabelsNoOpWhenOff(t *testing.T) {
	prev := labelProviderFactory
	constructed := false
	labelProviderFactory = func(p ClusterParams) (LabelProvider, bool, string) {
		if p.LabelEngine == "llm" {
			constructed = true
			return &fakeLabeller{label: "x"}, true, ""
		}
		return nil, false, ""
	}
	t.Cleanup(func() { labelProviderFactory = prev })

	s := newTestStore(t, t.TempDir())
	clusters := []themeCluster{{members: []int{0}, label: "Keep Me", labelSource: labelSourceBigram, simTo: map[int]float64{0: 1}}}
	docs := []ClusterCorpusDoc{{Kind: "decision", EntityID: "1", Text: "text"}}

	s.applyLLMLabels(clusters, docs, DefaultClusterParams()) // bigram
	if constructed {
		t.Error("label provider constructed when label.engine is bigram")
	}
	if clusters[0].label != "Keep Me" {
		t.Errorf("label changed with LLM off: %q", clusters[0].label)
	}
}
