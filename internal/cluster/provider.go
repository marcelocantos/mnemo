// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package cluster holds pure clustering helpers that do not touch the
// SQLite store: embedding providers and dense-vector packing (🎯T64.8).
package cluster

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
)

// EmbeddingProvider is the hosted-vectorisation surface for Engine A.
// Concrete providers live beside this interface; the store engine never
// speaks HTTP itself.
type EmbeddingProvider interface {
	// Name returned in cluster_embeddings.provider.
	Name() string
	// Model returned in cluster_embeddings.model.
	Model() string
	// ModelVersion returned in cluster_embeddings.model_version.
	// Empty string if the provider does not version models.
	ModelVersion() string
	// Dimensions is the expected vector length (sanity-checked on read).
	Dimensions() int
	// Embed produces dense float32 vectors for each input string.
	// Length of the returned slice must equal len(texts).
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// PackFloat32LE encodes v as little-endian IEEE 754 float32s (design:
// arch-independent, length = dims * 4).
func PackFloat32LE(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// UnpackFloat32LE decodes a little-endian float32 blob. Rejects length
// mismatches against dims.
func UnpackFloat32LE(b []byte, dims int) ([]float32, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("dims must be positive")
	}
	if len(b) != dims*4 {
		return nil, fmt.Errorf("vector length %d != dims*%d", len(b), dims*4)
	}
	out := make([]float32, dims)
	for i := 0; i < dims; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// DenseToSparse maps a dense vector onto a map keyed by dimension index
// strings so the existing cosine() over map[string]float64 can run
// unchanged for both engines.
func DenseToSparse(v []float32) map[string]float64 {
	out := make(map[string]float64, len(v))
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		for i := range v {
			out[fmt.Sprintf("d%d", i)] = 0
		}
		return out
	}
	for i, x := range v {
		out[fmt.Sprintf("d%d", i)] = float64(x) / norm
	}
	return out
}

// Labeler produces a short cluster label from representative excerpts.
// Used only when vault_clustering.label.engine = "llm".
type Labeler interface {
	// Label returns ≤6-word lowercase topic label for the excerpts.
	Label(ctx context.Context, excerpts []string) (string, error)
}
