// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"log/slog"
	"time"
)

// VaultClusteringConfig is the user-facing configuration for the theme
// clustering engine (docs/design/vault-clustering.md § User-facing
// controls). Every field is optional; a zero value resolves to the
// documented default via ResolvedClusterParams. Out-of-range values fall
// back to the default with a warning rather than blocking a pass.
type VaultClusteringConfig struct {
	Engine             string  `json:"engine,omitempty"`              // "heuristic" (default) | "embeddings"
	HeuristicThreshold float64 `json:"heuristic_threshold,omitempty"` // TF-IDF cosine cut, default 0.35
	EmbeddingThreshold float64 `json:"embedding_threshold,omitempty"` // dense cosine cut, default 0.55
	MinClusterWeight   float64 `json:"min_cluster_weight,omitempty"`  // render floor, default 3
	RecomputeInterval  string  `json:"recompute_interval,omitempty"`  // Go duration, default "24h"
	MaxThemes          int     `json:"max_themes,omitempty"`          // render cap, default 200
	RetireAfter        string  `json:"retire_after,omitempty"`        // Go duration, default "4320h"

	EmbeddingProvider     string `json:"embedding_provider,omitempty"`      // default "voyage"
	EmbeddingModel        string `json:"embedding_model,omitempty"`         // default "voyage-3-lite"
	EmbeddingModelVersion string `json:"embedding_model_version,omitempty"` // default ""

	Label VaultClusteringLabelConfig `json:"label,omitempty"`
}

// VaultClusteringLabelConfig configures the labelling chain. The LLM
// path is opt-in per the T63 egress posture (engine "llm"); default
// "bigram" runs fully offline.
type VaultClusteringLabelConfig struct {
	Engine              string   `json:"engine,omitempty"`                // "bigram" (default) | "llm"
	UserMinTokens       int      `json:"user_min_tokens,omitempty"`       // anchor eligibility, default 200
	UserFilenameExclude []string `json:"user_filename_exclude,omitempty"` // extends the default daily-note set
}

// Clustering defaults (docs/design/vault-clustering.md § config keys).
const (
	clusterEngineHeuristic  = "heuristic"
	clusterEngineEmbeddings = "embeddings"
	labelEngineBigram       = "bigram"
	labelEngineLLM          = "llm"

	defaultHeuristicThreshold = HeuristicThreshold // 0.35
	defaultEmbeddingThreshold = 0.55
	defaultMaxThemes          = 200
	defaultRetireAfter        = 4320 * time.Hour // 180 days
	defaultEmbeddingProvider  = "voyage"
	defaultEmbeddingModel     = "voyage-3-lite"
	defaultLabelUserMinTokens = 200
	minRecomputeInterval      = 60 * time.Second
)

// ClusterParams is the fully-resolved, validated parameter set a
// clustering pass runs with. Produced by Config.ResolvedClusterParams so
// the engine never sees a raw config value.
type ClusterParams struct {
	Engine             string
	Threshold          float64 // the active engine's cut (heuristic or embedding)
	MinClusterWeight   float64
	RecomputeInterval  time.Duration
	MaxThemes          int
	RetireAfter        time.Duration
	EmbeddingProvider  string
	EmbeddingModel     string
	EmbeddingModelVer  string
	LabelEngine        string
	LabelUserMinTokens int
	LabelFilenameExtra []string
}

// DefaultClusterParams is the parameter set used when no config is
// present (the tool path when config is unavailable, and tests).
func DefaultClusterParams() ClusterParams {
	return ClusterParams{
		Engine:             clusterEngineHeuristic,
		Threshold:          defaultHeuristicThreshold,
		MinClusterWeight:   DefaultMinClusterWeight,
		RecomputeInterval:  DefaultClusterInterval,
		MaxThemes:          defaultMaxThemes,
		RetireAfter:        defaultRetireAfter,
		EmbeddingProvider:  defaultEmbeddingProvider,
		EmbeddingModel:     defaultEmbeddingModel,
		LabelEngine:        labelEngineBigram,
		LabelUserMinTokens: defaultLabelUserMinTokens,
	}
}

// ResolvedClusterParams validates the vault_clustering config and returns
// the effective parameters, warning-and-defaulting on any invalid value
// so a typo degrades one setting rather than blocking clustering. warn
// collects human-readable notes for mnemo_vault_status.warnings[]; pass
// nil to discard them.
func (c Config) ResolvedClusterParams(warn *[]string) ClusterParams {
	p := DefaultClusterParams()
	vc := c.VaultClustering

	note := func(s string) {
		slog.Warn("vault_clustering: " + s)
		if warn != nil {
			*warn = append(*warn, s)
		}
	}

	switch vc.Engine {
	case "", clusterEngineHeuristic:
		p.Engine = clusterEngineHeuristic
		p.Threshold = defaultHeuristicThreshold
	case clusterEngineEmbeddings:
		p.Engine = clusterEngineEmbeddings
		p.Threshold = defaultEmbeddingThreshold
	default:
		note("engine \"" + vc.Engine + "\" is not one of heuristic|embeddings; using heuristic")
	}

	// Threshold override for the active engine.
	if vc.Engine == clusterEngineEmbeddings {
		if inUnit(vc.EmbeddingThreshold) {
			p.Threshold = vc.EmbeddingThreshold
		} else if vc.EmbeddingThreshold != 0 {
			note("embedding_threshold out of [0,1]; using default")
		}
	} else {
		if inUnit(vc.HeuristicThreshold) {
			p.Threshold = vc.HeuristicThreshold
		} else if vc.HeuristicThreshold != 0 {
			note("heuristic_threshold out of [0,1]; using default")
		}
	}

	if vc.MinClusterWeight > 0 {
		p.MinClusterWeight = vc.MinClusterWeight
	} else if vc.MinClusterWeight != 0 {
		note("min_cluster_weight must be positive; using default")
	}

	if vc.MaxThemes > 0 {
		p.MaxThemes = vc.MaxThemes
	} else if vc.MaxThemes != 0 {
		note("max_themes must be positive; using default")
	}

	p.RecomputeInterval = resolveClusterDuration(
		vc.RecomputeInterval, DefaultClusterInterval, minRecomputeInterval, "recompute_interval", note)
	p.RetireAfter = resolveClusterDuration(
		vc.RetireAfter, defaultRetireAfter, 0, "retire_after", note)

	if vc.EmbeddingProvider != "" {
		p.EmbeddingProvider = vc.EmbeddingProvider
	}
	if vc.EmbeddingModel != "" {
		p.EmbeddingModel = vc.EmbeddingModel
	}
	p.EmbeddingModelVer = vc.EmbeddingModelVersion

	switch vc.Label.Engine {
	case "", labelEngineBigram:
		p.LabelEngine = labelEngineBigram
	case labelEngineLLM:
		p.LabelEngine = labelEngineLLM
	default:
		note("label.engine \"" + vc.Label.Engine + "\" is not one of bigram|llm; using bigram")
	}
	if vc.Label.UserMinTokens > 0 {
		p.LabelUserMinTokens = vc.Label.UserMinTokens
	}
	p.LabelFilenameExtra = vc.Label.UserFilenameExclude

	return p
}

func inUnit(f float64) bool { return f > 0 && f <= 1 }

// resolveClusterDuration parses a Go duration, clamping up to min (when
// min > 0) and falling back to def on a parse error or non-positive
// value, noting each correction.
func resolveClusterDuration(raw string, def, min time.Duration, key string, note func(string)) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		note(key + " \"" + raw + "\" is not a valid positive duration; using default")
		return def
	}
	if min > 0 && d < min {
		note(key + " below the " + min.String() + " floor; clamping up")
		return min
	}
	return d
}
