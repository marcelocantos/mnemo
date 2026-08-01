// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
)

const (
	anthropicDefaultURL   = "https://api.anthropic.com/v1/messages"
	anthropicDefaultModel = "claude-haiku-4-5-20251001"
	anthropicAPIVersion   = "2023-06-01"
)

// AnthropicLabelArgs configures the LLM labelling path.
type AnthropicLabelArgs struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	OnRequest  func()
}

// AnthropicLabeler implements Labeler via the Anthropic Messages API.
// Only constructed when vault_clustering.label.engine = "llm".
type AnthropicLabeler struct {
	apiKey    string
	model     string
	baseURL   string
	client    *http.Client
	onRequest func()
}

// NewAnthropicLabeler returns a labeler or an error when no key is set.
func NewAnthropicLabeler(args *AnthropicLabelArgs) (*AnthropicLabeler, error) {
	if args == nil {
		args = &AnthropicLabelArgs{}
	}
	key := strings.TrimSpace(args.APIKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	}
	if key == "" {
		return nil, fmt.Errorf("anthropic labeler: no API key")
	}
	model := args.Model
	if model == "" {
		model = anthropicDefaultModel
	}
	url := args.BaseURL
	if url == "" {
		url = anthropicDefaultURL
	}
	client := args.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &AnthropicLabeler{
		apiKey:    key,
		model:     model,
		baseURL:   url,
		client:    client,
		onRequest: args.OnRequest,
	}, nil
}

type anthReq struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []anthMessage `json:"messages"`
}

type anthMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Label asks Haiku for a ≤6-word topic label.
func (a *AnthropicLabeler) Label(ctx context.Context, excerpts []string) (string, error) {
	if len(excerpts) == 0 {
		return "", fmt.Errorf("no excerpts")
	}
	// Cap to top-5 as the design specifies.
	if len(excerpts) > 5 {
		excerpts = excerpts[:5]
	}
	var b strings.Builder
	b.WriteString("These are excerpts grouped by topical similarity. Label the topic in ≤6 words, all lowercase, no punctuation:\n\n")
	for i, e := range excerpts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, truncate(strings.TrimSpace(e), 400))
	}
	body, err := json.Marshal(anthReq{
		Model:     a.model,
		MaxTokens: 32,
		Messages:  []anthMessage{{Role: "user", Content: b.String()}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	if a.onRequest != nil {
		a.onRequest()
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed anthResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("anthropic: %s", parsed.Error.Message)
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text = c.Text
			break
		}
	}
	return sanitizeLLMLabel(text), nil
}

// sanitizeLLMLabel lowercases, strips punctuation, caps at 6 words.
func sanitizeLLMLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Drop surrounding quotes the model sometimes emits.
	s = strings.Trim(s, `"'`)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	fields := strings.Fields(b.String())
	if len(fields) > 6 {
		fields = fields[:6]
	}
	return strings.Join(fields, " ")
}
