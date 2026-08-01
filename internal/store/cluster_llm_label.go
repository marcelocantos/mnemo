// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// LabelProvider generates a short human label for a cluster from its
// representative excerpts (docs/design/vault-clustering.md § Cluster
// labelling, step 2). The Anthropic Haiku labeller is the only concrete
// implementation.
type LabelProvider interface {
	Label(ctx context.Context, excerpts []string) (string, error)
}

// labelProviderFactory mirrors embeddingProviderFactory's contract and
// egress discipline: it never constructs a labeller unless
// label.engine=="llm". Injectable for tests.
var labelProviderFactory = defaultLabelProviderFactory

func defaultLabelProviderFactory(p ClusterParams) (LabelProvider, bool, string) {
	if p.LabelEngine != labelEngineLLM {
		return nil, false, "" // not opted in — no labeller, no outbound call
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, false, "label.engine is \"llm\" but ANTHROPIC_API_KEY is unset; using bigram labels for this pass"
	}
	return newHaikuLabeller(key), true, ""
}

// applyLLMLabels replaces bigram labels with Haiku-generated ones where a
// labeller is available. User-anchored labels are never overridden — a
// human title outranks a model guess. A no-op when not opted in or on any
// provider error (the bigram label stays).
func (s *Store) applyLLMLabels(clusters []themeCluster, docs []ClusterCorpusDoc, p ClusterParams) {
	prov, ok, note := labelProviderFactory(p)
	if note != "" {
		slog.Warn("vault_clustering: " + note)
	}
	if !ok {
		return
	}
	for i := range clusters {
		if clusters[i].labelSource != labelSourceBigram {
			continue // keep user-anchored labels
		}
		excerpts := representativeExcerpts(&clusters[i], docs, 5)
		if len(excerpts) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		label, err := prov.Label(ctx, excerpts)
		cancel()
		label = normalizeLLMLabel(label)
		if err != nil || label == "" {
			continue // degrade to the bigram label already in place
		}
		clusters[i].label = label
		clusters[i].labelSource = labelSourceLLM
		clusters[i].labelNote = ""
		clusters[i].slug = slugify(label)
	}
}

// representativeExcerpts returns up to n member texts ordered by centroid
// closeness — the cluster's most typical documents.
func representativeExcerpts(tc *themeCluster, docs []ClusterCorpusDoc, n int) []string {
	members := append([]int(nil), tc.members...)
	sort.Slice(members, func(a, b int) bool {
		return tc.simTo[members[a]] > tc.simTo[members[b]]
	})
	var out []string
	for _, mi := range members {
		if len(out) >= n {
			break
		}
		if t := strings.TrimSpace(docs[mi].Text); t != "" {
			out = append(out, firstNWords(t, 80))
		}
	}
	return out
}

// normalizeLLMLabel enforces the design's label shape: ≤ 6 words,
// lowercase, no surrounding punctuation.
func normalizeLLMLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, ".,:;\"'`")
	fields := strings.Fields(s)
	if len(fields) > 6 {
		fields = fields[:6]
	}
	return titleCase(strings.Join(fields, " "))
}

// --- Anthropic Haiku labeller (real, gated) ---------------------------

type haikuLabeller struct {
	key    string
	client *http.Client
}

func newHaikuLabeller(key string) *haikuLabeller {
	return &haikuLabeller{key: key, client: &http.Client{Timeout: 30 * time.Second}}
}

// Label calls the Anthropic Messages API. Only ever invoked from
// applyLLMLabels behind the label.engine=="llm" + key-present gate.
func (h *haikuLabeller) Label(ctx context.Context, excerpts []string) (string, error) {
	prompt := "These are excerpts grouped by topical similarity. Label the topic in ≤6 words, all lowercase, no punctuation:\n\n" +
		strings.Join(excerpts, "\n---\n")
	body, err := json.Marshal(map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 32,
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", h.key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anthropic: status %d", resp.StatusCode)
	}
	var parsed struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if len(parsed.Content) == 0 {
		return "", fmt.Errorf("anthropic: empty content")
	}
	return parsed.Content[0].Text, nil
}
