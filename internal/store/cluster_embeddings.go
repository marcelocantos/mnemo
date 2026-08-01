// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"
)

// EmbeddingProvider produces dense vectors for corpus documents
// (docs/design/vault-clustering.md § Engine A). Provider-agnostic from
// day one; Voyage is the only concrete provider shipped.
type EmbeddingProvider interface {
	Name() string
	Model() string
	ModelVersion() string
	Dimensions() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// embeddingProviderFactory constructs a provider for the given params,
// reading credentials from the environment. Returns (provider, available,
// warnNote): available=false with an empty note means "not opted in"
// (the common case → no egress); available=false with a note means
// "opted in but unusable" (missing key / unknown provider → fall back to
// heuristic and surface the note). Injectable so tests exercise the
// engine without a network. THE EGRESS GATE: the default never
// constructs a provider unless engine=="embeddings".
var embeddingProviderFactory = defaultEmbeddingProviderFactory

func defaultEmbeddingProviderFactory(p ClusterParams) (EmbeddingProvider, bool, string) {
	if p.Engine != clusterEngineEmbeddings {
		return nil, false, "" // not opted in — no provider, no outbound call
	}
	switch p.EmbeddingProvider {
	case "", defaultEmbeddingProvider:
		key := os.Getenv("VOYAGE_API_KEY")
		if key == "" {
			return nil, false, "engine is \"embeddings\" but VOYAGE_API_KEY is unset; using the heuristic engine for this pass"
		}
		model := p.EmbeddingModel
		if model == "" {
			model = defaultEmbeddingModel
		}
		return newVoyageProvider(key, model, p.EmbeddingModelVer), true, ""
	default:
		return nil, false, "unknown embedding_provider \"" + p.EmbeddingProvider + "\"; using the heuristic engine for this pass"
	}
}

// buildVectors returns per-document vectors, the engine actually used,
// and any degradation warnings. The embeddings path runs only when the
// provider factory yields an available provider (opt-in + key); on any
// failure it falls back to TF-IDF so a pass never blocks on egress.
func (s *Store) buildVectors(docs []ClusterCorpusDoc, p ClusterParams) ([]map[string]float64, string, []string) {
	var warns []string
	if p.Engine == clusterEngineEmbeddings {
		prov, ok, note := embeddingProviderFactory(p)
		if note != "" {
			warns = append(warns, note)
		}
		if ok {
			dense, err := s.embedCorpus(docs, prov, p.ForceReembed)
			if err == nil {
				return dense, clusterEngineEmbeddings, warns
			}
			warns = append(warns, "embedding provider error ("+err.Error()+"); using the heuristic engine for this pass")
		}
	}
	return tfidfVectors(docs), clusterEngineHeuristic, warns
}

// embedCorpus returns one L2-normalised dense vector per document, keyed
// by dimension index so it plugs into the shared cosine/union-find path.
// Vectors are cached in cluster_embeddings by
// (doc_kind, entity_id, content_hash, provider, model, model_version):
// a content or model change misses the cache and re-embeds; forceReembed
// drops the active fingerprint's rows first.
func (s *Store) embedCorpus(docs []ClusterCorpusDoc, prov EmbeddingProvider, forceReembed bool) ([]map[string]float64, error) {
	provider, model, ver := prov.Name(), prov.Model(), prov.ModelVersion()

	if forceReembed {
		if _, err := s.writeDB.Exec(
			`DELETE FROM cluster_embeddings WHERE provider = ? AND model = ? AND model_version = ?`,
			provider, model, ver); err != nil {
			return nil, err
		}
	}

	hashes := make([]string, len(docs))
	out := make([]map[string]float64, len(docs))
	var missIdx []int
	var missText []string
	for i, d := range docs {
		h := embeddingContentHash(d.Text)
		hashes[i] = h
		if !forceReembed {
			if vec, ok := s.readEmbedding(d.Kind, d.EntityID, h, provider, model, ver); ok {
				out[i] = denseToMap(vec)
				continue
			}
		}
		missIdx = append(missIdx, i)
		missText = append(missText, d.Text)
	}

	// Batch the misses through the provider (128/call, Voyage's limit).
	const batch = 128
	for start := 0; start < len(missText); start += batch {
		end := start + batch
		if end > len(missText) {
			end = len(missText)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		vecs, err := prov.Embed(ctx, missText[start:end])
		cancel()
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("provider returned %d vectors for %d inputs", len(vecs), end-start)
		}
		for k, vec := range vecs {
			i := missIdx[start+k]
			out[i] = denseToMap(vec)
			s.writeEmbedding(docs[i].Kind, docs[i].EntityID, hashes[i], provider, model, ver, vec)
		}
	}
	return out, nil
}

func (s *Store) readEmbedding(kind, entity, hash, provider, model, ver string) ([]float32, bool) {
	var blob []byte
	var dims int
	err := s.readDB.QueryRow(`
		SELECT vector, dims FROM cluster_embeddings
		WHERE doc_kind=? AND entity_id=? AND content_hash=? AND provider=? AND model=? AND model_version=?`,
		kind, entity, hash, provider, model, ver).Scan(&blob, &dims)
	if err != nil || len(blob) != dims*4 {
		return nil, false // absent, or a torn row (length sanity per design)
	}
	return decodeFloat32LE(blob, dims), true
}

func (s *Store) writeEmbedding(kind, entity, hash, provider, model, ver string, vec []float32) {
	_, err := s.writeDB.Exec(`
		INSERT INTO cluster_embeddings
			(doc_kind, entity_id, content_hash, provider, model, model_version, dims, vector, computed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(doc_kind, entity_id, content_hash, provider, model, model_version)
		DO UPDATE SET dims=excluded.dims, vector=excluded.vector, computed_at=excluded.computed_at`,
		kind, entity, hash, provider, model, ver, len(vec), encodeFloat32LE(vec),
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		// Cache write failure is non-fatal: the vector is already in hand
		// for this pass; the next pass simply re-embeds.
		return
	}
}

func embeddingContentHash(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

// denseToMap encodes a dense vector as a dimension-indexed sparse map and
// L2-normalises it, so it feeds the shared cosine path (which treats
// missing keys as zero and, for unit vectors, returns true cosine).
func denseToMap(vec []float32) map[string]float64 {
	m := make(map[string]float64, len(vec))
	for i, v := range vec {
		if v != 0 {
			m[strconv.Itoa(i)] = float64(v)
		}
	}
	normalize(m)
	return m
}

// encodeFloat32LE packs float32s little-endian (fixed regardless of host
// arch, per design, so a backup restored on another arch reads correctly).
func encodeFloat32LE(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decodeFloat32LE(buf []byte, dims int) []float32 {
	out := make([]float32, dims)
	for i := 0; i < dims; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out
}

// --- Voyage AI provider (real, gated) ---------------------------------

type voyageProvider struct {
	key, model, version string
	dims                int
	client              *http.Client
}

func newVoyageProvider(key, model, version string) *voyageProvider {
	return &voyageProvider{
		key: key, model: model, version: version, dims: 1024,
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

func (v *voyageProvider) Name() string         { return "voyage" }
func (v *voyageProvider) Model() string        { return v.model }
func (v *voyageProvider) ModelVersion() string { return v.version }
func (v *voyageProvider) Dimensions() int      { return v.dims }

// Embed calls Voyage's embeddings endpoint. Only ever invoked from
// embedCorpus, which only runs behind the engine=="embeddings" +
// key-present gate — so this is the single outbound surface.
func (v *voyageProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"input": texts, "model": v.model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.voyageai.com/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+v.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voyage: status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		out[i] = d.Embedding
	}
	return out, nil
}
