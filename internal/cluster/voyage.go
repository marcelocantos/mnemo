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
)

const (
	voyageDefaultURL   = "https://api.voyageai.com/v1/embeddings"
	voyageDefaultModel = "voyage-3-lite"
	// voyage-3-lite embedding dimension.
	voyage3LiteDims = 512
	// Batch size under Voyage's 1000-input list limit; keep small for
	// predictable request sizes.
	voyageBatchSize = 64
)

// VoyageArgs configures a Voyage EmbeddingProvider.
type VoyageArgs struct {
	// APIKey. Empty → VOYAGE_API_KEY then VOYAGEAI_API_KEY from the env.
	APIKey string
	// Model defaults to voyage-3-lite.
	Model string
	// ModelVersion pins the cache-key version tag (may be empty).
	ModelVersion string
	// BaseURL overrides the API endpoint (tests).
	BaseURL string
	// HTTPClient overrides the client (tests). Nil → 30s timeout client.
	HTTPClient *http.Client
	// Dimensions override (tests / future models). Zero → model default.
	Dimensions int
	// OnRequest is invoked immediately before each HTTP POST (tests use
	// this to observe egress without inspecting the network).
	OnRequest func()
}

// Voyage is the concrete EmbeddingProvider for Voyage AI.
type Voyage struct {
	apiKey       string
	model        string
	modelVersion string
	baseURL      string
	client       *http.Client
	dims         int
	onRequest    func()
}

// NewVoyage builds a Voyage provider. Returns an error when no API key
// is available — callers should fall back to the heuristic engine.
func NewVoyage(args *VoyageArgs) (*Voyage, error) {
	if args == nil {
		args = &VoyageArgs{}
	}
	key := strings.TrimSpace(args.APIKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("VOYAGE_API_KEY"))
	}
	if key == "" {
		key = strings.TrimSpace(os.Getenv("VOYAGEAI_API_KEY"))
	}
	if key == "" {
		return nil, fmt.Errorf("voyage: no API key (set VOYAGE_API_KEY or pass APIKey)")
	}
	model := args.Model
	if model == "" {
		model = voyageDefaultModel
	}
	url := args.BaseURL
	if url == "" {
		url = voyageDefaultURL
	}
	client := args.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	dims := args.Dimensions
	if dims <= 0 {
		dims = voyage3LiteDims
	}
	return &Voyage{
		apiKey:       key,
		model:        model,
		modelVersion: args.ModelVersion,
		baseURL:      url,
		client:       client,
		dims:         dims,
		onRequest:    args.OnRequest,
	}, nil
}

func (v *Voyage) Name() string         { return "voyage" }
func (v *Voyage) Model() string        { return v.model }
func (v *Voyage) ModelVersion() string { return v.modelVersion }
func (v *Voyage) Dimensions() int      { return v.dims }

type voyageReq struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type voyageResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embed batches texts and POSTs to the Voyage embeddings endpoint.
func (v *Voyage) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += voyageBatchSize {
		end := start + voyageBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]
		vecs, err := v.embedBatch(ctx, batch)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("voyage: got %d vectors for %d inputs", len(vecs), len(batch))
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

func (v *Voyage) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(voyageReq{Input: texts, Model: v.model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.apiKey)

	if v.onRequest != nil {
		v.onRequest()
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage: http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("voyage: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("voyage: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed voyageResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("voyage: decode: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("voyage: api error: %s", parsed.Error.Message)
	}
	// Voyage may return data unordered by index — place by Index.
	out := make([][]float32, len(texts))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("voyage: out-of-range index %d", d.Index)
		}
		if len(d.Embedding) != v.dims {
			// Accept provider-reported dims when they differ from our
			// default (model swap); still require non-empty.
			if len(d.Embedding) == 0 {
				return nil, fmt.Errorf("voyage: empty embedding at index %d", d.Index)
			}
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("voyage: missing embedding for index %d", i)
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
