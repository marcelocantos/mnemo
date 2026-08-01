// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
)

func TestResolvedClusterParamsDefaults(t *testing.T) {
	p := Config{}.ResolvedClusterParams(nil)
	if p.Engine != "heuristic" || p.Threshold != defaultHeuristicThreshold {
		t.Errorf("default engine/threshold wrong: %+v", p)
	}
	if p.MinClusterWeight != DefaultMinClusterWeight || p.MaxThemes != defaultMaxThemes {
		t.Errorf("default weight/max wrong: %+v", p)
	}
	if p.RecomputeInterval != DefaultClusterInterval || p.RetireAfter != defaultRetireAfter {
		t.Errorf("default durations wrong: %+v", p)
	}
	if p.LabelEngine != "bigram" || p.LabelUserMinTokens != defaultLabelUserMinTokens {
		t.Errorf("default label wrong: %+v", p)
	}
}

func TestResolvedClusterParamsOverrides(t *testing.T) {
	cfg := Config{VaultClustering: VaultClusteringConfig{
		Engine:             "embeddings",
		EmbeddingThreshold: 0.6,
		MinClusterWeight:   5,
		RecomputeInterval:  "12h",
		MaxThemes:          50,
		RetireAfter:        "1000h",
		Label:              VaultClusteringLabelConfig{Engine: "llm", UserMinTokens: 300},
	}}
	p := cfg.ResolvedClusterParams(nil)
	if p.Engine != "embeddings" || p.Threshold != 0.6 {
		t.Errorf("embeddings override wrong: %+v", p)
	}
	if p.MinClusterWeight != 5 || p.MaxThemes != 50 {
		t.Errorf("weight/max override wrong: %+v", p)
	}
	if p.RecomputeInterval != 12*time.Hour || p.RetireAfter != 1000*time.Hour {
		t.Errorf("duration override wrong: %+v", p)
	}
	if p.LabelEngine != "llm" || p.LabelUserMinTokens != 300 {
		t.Errorf("label override wrong: %+v", p)
	}
}

func TestResolvedClusterParamsWarnsAndDefaults(t *testing.T) {
	cfg := Config{VaultClustering: VaultClusteringConfig{
		Engine:             "bogus",
		HeuristicThreshold: 5,     // out of [0,1]
		MinClusterWeight:   -1,    // non-positive
		RecomputeInterval:  "10s", // below the 60s floor
		Label:              VaultClusteringLabelConfig{Engine: "wat"},
	}}
	var warn []string
	p := cfg.ResolvedClusterParams(&warn)

	// Bad values fall back to defaults.
	if p.Engine != "heuristic" || p.Threshold != defaultHeuristicThreshold {
		t.Errorf("bad engine/threshold not defaulted: %+v", p)
	}
	if p.MinClusterWeight != DefaultMinClusterWeight {
		t.Errorf("bad weight not defaulted: %+v", p)
	}
	if p.RecomputeInterval != minRecomputeInterval {
		t.Errorf("sub-floor interval not clamped: %v", p.RecomputeInterval)
	}
	if p.LabelEngine != "bigram" {
		t.Errorf("bad label engine not defaulted: %+v", p)
	}
	// Each of the four bad values produced a warning.
	if len(warn) < 4 {
		t.Errorf("want >=4 warnings, got %d: %v", len(warn), warn)
	}
}
