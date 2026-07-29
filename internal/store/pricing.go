// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Pricing model calls from a fetched rate card (🎯T135).
//
// The rate card is the only part of cost accounting that cannot be derived
// from data already in hand, and it is the part that goes stale: models are
// added, renamed and repriced. Embedding it guarantees every model released
// after the binary is wrong.
//
// What this replaces was worse than stale. It prefix-matched, so every
// claude-opus-4-* took opus-4's rate — but claude-opus-4-5 actually prices
// at $5/M input against opus-4's $15/M, a 3x overcharge. And it fell back
// to Sonnet's rates for anything unrecognised, which is how 27 billion
// Codex tokens came to be priced as Anthropic's: not an approximation, an
// invention.
//
// So: exact model match or nothing. An unpriced model is reported as
// unpriced, never as free and never as something else's price.

// TokenCounts is one call's billable quantities.
//
// Cache writes are split by TTL because the tiers price differently, and
// the longer tier is the majority of real volume. CacheWrite5m and
// CacheWrite1h are the split; CacheWriteFlat is the aggregate a record may
// report instead, used only when the split is absent.
type TokenCounts struct {
	Input          int64
	Output         int64
	CacheRead      int64
	CacheWrite5m   int64
	CacheWrite1h   int64
	CacheWriteFlat int64
}

// contextSize approximates the prompt size for long-context tier
// selection. Providers price above a per-model threshold on the size of
// the request, which is everything sent: fresh input plus whatever was
// served from or written to cache.
func (t TokenCounts) contextSize() int64 {
	w := t.CacheWrite5m + t.CacheWrite1h
	if w == 0 {
		w = t.CacheWriteFlat
	}
	return t.Input + t.CacheRead + w
}

// ModelRate is one model's per-token prices, including the above-threshold
// variants. Zero means "not separately priced", in which case the base
// rate applies at any context size.
type ModelRate struct {
	Input        float64 `json:"input_cost_per_token"`
	Output       float64 `json:"output_cost_per_token"`
	CacheRead    float64 `json:"cache_read_input_token_cost"`
	CacheWrite5m float64 `json:"cache_creation_input_token_cost"`
	CacheWrite1h float64 `json:"cache_creation_input_token_cost_above_1hr"`

	InputAbove        float64 `json:"input_cost_per_token_above_200k_tokens"`
	OutputAbove       float64 `json:"output_cost_per_token_above_200k_tokens"`
	CacheReadAbove    float64 `json:"cache_read_input_token_cost_above_200k_tokens"`
	CacheWrite5mAbove float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`
	CacheWrite1hAbove float64 `json:"cache_creation_input_token_cost_above_1hr_above_200k_tokens"`

	// ContextThreshold is where the above-rates take over. Zero disables
	// the long-context tier for this model.
	ContextThreshold int64 `json:"max_input_tokens"`
}

// pick returns the rate for a class, preferring the above-threshold
// variant when the context crosses the threshold and such a rate exists.
func pick(base, above float64, over bool) float64 {
	if over && above > 0 {
		return above
	}
	return base
}

// Cost prices ONE CALL. Never pass aggregated counts.
//
// The long-context tier is a property of a single request: a prompt over
// the threshold reprices, and one under it does not. Summing a day and
// asking whether the total crosses 200k answers a different question and
// gets it wrong in the expensive direction — a day of 5.9 million
// cache-read tokens is hundreds of ordinary requests, not one enormous
// one, and pricing it as the latter doubles the bill.
//
// Aggregate by summing per-record costs, never by pricing summed counts.
//
// The bool reports whether every quantity present was actually priceable;
// false means part of this call has no rate and the figure is incomplete.
func (r ModelRate) Cost(t TokenCounts) (float64, bool) {
	over := r.ContextThreshold > 0 && t.contextSize() > r.ContextThreshold

	w5, w1 := t.CacheWrite5m, t.CacheWrite1h
	if w5 == 0 && w1 == 0 && t.CacheWriteFlat > 0 {
		// No TTL split recorded. Charge the shorter tier: it is the
		// cheaper of the two, so an unknown split under-states rather
		// than inflates, and under-stating is the honest direction when
		// the data cannot say.
		w5 = t.CacheWriteFlat
	}

	usd := float64(t.Input)*pick(r.Input, r.InputAbove, over) +
		float64(t.Output)*pick(r.Output, r.OutputAbove, over) +
		float64(t.CacheRead)*pick(r.CacheRead, r.CacheReadAbove, over) +
		float64(w5)*pick(r.CacheWrite5m, r.CacheWrite5mAbove, over) +
		float64(w1)*pick(r.CacheWrite1h, r.CacheWrite1hAbove, over)

	// A rate card entry that prices nothing is not a price.
	complete := r.Input > 0 || r.Output > 0 || r.CacheRead > 0 || r.CacheWrite5m > 0
	return usd, complete
}

// RateCard is a dated snapshot of per-model prices.
//
// Dated deliberately: a cached card IS the record of what prices were in
// force, which is what makes contemporaneous pricing possible. Repricing
// old usage with a newer card is an error in the opposite direction from
// staleness — a later artifact projected backwards.
type RateCard struct {
	FetchedAt time.Time            `json:"fetched_at"`
	Source    string               `json:"source"`
	Rates     map[string]ModelRate `json:"rates"`
}

// Rate looks up a model by its EXACT identifier. No prefix matching and no
// fallback: two identifiers differing only by a date suffix may price
// differently, and guessing produced the 3x error this replaced.
func (c *RateCard) Rate(model string) (ModelRate, bool) {
	if c == nil || model == "" {
		return ModelRate{}, false
	}
	r, ok := c.Rates[model]
	return r, ok
}

// LongContextThreshold is the request size above which every current
// model reprices. Used to bucket records in SQL so per-request tiering
// survives aggregation; a model whose own threshold differs is priced
// without the tier rather than with the wrong one.
const LongContextThreshold = 200000

// PricingSourceURL is the community-maintained rate file. One request,
// thousands of models, carrying the TTL and long-context variants.
const PricingSourceURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// pricingCacheFile is where the fetched card is kept, under the mnemo home.
const pricingCacheFile = "pricing.json"

var (
	pricingMu     sync.Mutex
	pricingCached *RateCard
)

// LoadRateCard returns the cached rate card, reading it from disk on first
// use. It never fetches: fetching is an outbound call and happens only
// through RefreshRateCard, behind an explicit opt-in.
//
// Returns nil when no card has been fetched, which callers must treat as
// "nothing is priceable" rather than as an excuse to guess.
func LoadRateCard() *RateCard {
	pricingMu.Lock()
	defer pricingMu.Unlock()
	if pricingCached != nil {
		return pricingCached
	}
	home, err := EffectiveHome()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, pricingCacheFile))
	if err != nil {
		return nil
	}
	var c RateCard
	if json.Unmarshal(data, &c) != nil || len(c.Rates) == 0 {
		return nil
	}
	pricingCached = &c
	return pricingCached
}

// SetRateCard installs a rate card process-wide, returning a function that
// restores the previous one.
//
// The seam for anything that obtains rates by a route other than
// RefreshRateCard: a card shipped alongside a deployment, one supplied by
// a host application, or a fixed card in a test that must not depend on a
// fetched file. Restoring rather than clearing matters because pricing is
// global state and a caller that installs one has no way to know whether
// something else already had.
func SetRateCard(c *RateCard) (restore func()) {
	pricingMu.Lock()
	prev := pricingCached
	pricingCached = c
	pricingMu.Unlock()
	return func() {
		pricingMu.Lock()
		pricingCached = prev
		pricingMu.Unlock()
	}
}

// RefreshRateCard fetches the rate card and caches it with today's date.
//
// The only outbound call in cost accounting, and gated by the caller: a
// user who has not asked for pricing must not have their machine reach the
// network because they ran a usage report.
func RefreshRateCard(ctx interface{ Done() <-chan struct{} }) (*RateCard, error) {
	req, err := http.NewRequest(http.MethodGet, PricingSourceURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch rate card: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch rate card: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var raw map[string]ModelRate
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse rate card: %w", err)
	}
	delete(raw, "sample_spec")

	card := &RateCard{FetchedAt: time.Now().UTC(), Source: PricingSourceURL, Rates: raw}
	home, err := EffectiveHome()
	if err == nil {
		if out, err := json.Marshal(card); err == nil {
			_ = os.WriteFile(filepath.Join(home, pricingCacheFile), out, 0o644)
		}
	}
	pricingMu.Lock()
	pricingCached = card
	pricingMu.Unlock()
	return card, nil
}

// priceBucket prices a group of records that share a model and a
// long-context tier (🎯T135).
//
// Aggregation is safe here precisely because the bucket is tier-uniform:
// the SQL groups on each record's own context size, so summing within a
// bucket cannot smuggle an ordinary request into the expensive tier.
//
// Returns priced=false when the model has no rate. Callers must surface
// that rather than fall back: the previous implementation substituted
// Sonnet's rates for anything it did not recognise, which is not an
// approximation but an invention, and it priced an entire foreign
// provider's corpus.
func priceBucket(card *RateCard, model string, overThreshold bool, t TokenCounts) (float64, bool) {
	rate, ok := card.Rate(model)
	if !ok {
		return 0, false
	}
	// The bucket's tier is already decided; neutralise the per-call
	// threshold so summed counts cannot re-derive it and get it wrong.
	rate.ContextThreshold = 0
	if overThreshold {
		if rate.InputAbove > 0 {
			rate.Input = rate.InputAbove
		}
		if rate.OutputAbove > 0 {
			rate.Output = rate.OutputAbove
		}
		if rate.CacheReadAbove > 0 {
			rate.CacheRead = rate.CacheReadAbove
		}
		if rate.CacheWrite5mAbove > 0 {
			rate.CacheWrite5m = rate.CacheWrite5mAbove
		}
		if rate.CacheWrite1hAbove > 0 {
			rate.CacheWrite1h = rate.CacheWrite1hAbove
		}
	}
	return rate.Cost(t)
}

// EstimateCost prices a group of records at a model's rates, returning
// priced=false when the model has no entry in the rate card.
//
// The single pricing entry point. There were previously two — one here and
// a hand-copied "mirroring the rates in store.go" table in the HTTP layer —
// and they had already drifted apart: the copy was missing models the
// original had. Two implementations of a price list is one more than can be
// kept correct.
func EstimateCost(model string, t TokenCounts) (float64, bool) {
	over := t.contextSize() > LongContextThreshold
	return priceBucket(LoadRateCard(), model, over, t)
}
