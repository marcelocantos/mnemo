// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package segment implements Tier-1 structural topic segmentation
// (🎯T64.10): idle gaps, user-turn cadence, and multi-scale nesting
// with seal-on-lookahead. Zero egress; no vectors.
package segment

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Default seal: require this many substantive messages after a
// candidate boundary before the preceding span is sealed.
const DefaultSealLookahead = 3

// Default idle gap that counts as a strong topic switch.
const DefaultIdleGap = 30 * time.Minute

// MinSpanMsgs is the minimum substantive messages in a sealed span.
const MinSpanMsgs = 2

// Message is the minimal projection the structural segmenter needs.
type Message struct {
	ID        int
	Role      string
	Text      string
	Timestamp string // RFC3339 or SQLite datetime
	IsNoise   bool
}

// Span is a candidate or sealed topic interval over message IDs.
type Span struct {
	FromMsgID  int
	ToMsgID    int
	Level      int
	ParentIdx  int // index into result slice; -1 if root
	Confidence float64
	Sealed     bool
	Label      string
	Summary    string
	Method     string
	FirstTS    string
	LastTS     string
}

// Config controls structural segmentation.
type Config struct {
	SealLookahead int
	IdleGap       time.Duration
	MinSpanMsgs   int
}

// DefaultConfig returns Tier-1 defaults (zero egress).
func DefaultConfig() Config {
	return Config{
		SealLookahead: DefaultSealLookahead,
		IdleGap:       DefaultIdleGap,
		MinSpanMsgs:   MinSpanMsgs,
	}
}

// SegmentID returns seg_<sha1(session|from|to|method|level)[:12]>.
func SegmentID(sessionID string, from, to, level int, method string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d|%s|%d", sessionID, from, to, method, level)))
	return "seg_" + hex.EncodeToString(sum[:])[:12]
}

// Structural runs Tier-1 segmentation over substantive messages.
// Returns nested spans (level 0 fine, level 1 coarse) with ParentIdx
// wired. Spans near the tail remain unsealed when lookahead is short.
func Structural(msgs []Message, cfg Config) []Span {
	if cfg.SealLookahead <= 0 {
		cfg.SealLookahead = DefaultSealLookahead
	}
	if cfg.IdleGap <= 0 {
		cfg.IdleGap = DefaultIdleGap
	}
	if cfg.MinSpanMsgs <= 0 {
		cfg.MinSpanMsgs = MinSpanMsgs
	}

	var sub []Message
	for _, m := range msgs {
		if m.IsNoise {
			continue
		}
		// Treat empty pure-noise roles carefully: keep user/assistant.
		if m.Role != "user" && m.Role != "assistant" && m.Role != "" {
			// still allow tool-less substantive rows with text
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
		}
		sub = append(sub, m)
	}
	if len(sub) < cfg.MinSpanMsgs {
		return nil
	}

	// Boundary scores between sub[i] and sub[i+1] (score for cut AFTER i).
	boundaries := make([]float64, len(sub)-1)
	for i := 0; i < len(sub)-1; i++ {
		boundaries[i] = boundaryScore(sub[i], sub[i+1], cfg.IdleGap)
	}

	// Fine cuts: strong boundaries (>= 0.6).
	fineCuts := cutIndices(boundaries, 0.6)
	// Coarse cuts: very strong (>= 0.85) — multi-scale hierarchy.
	coarseCuts := cutIndices(boundaries, 0.85)

	fine := spansFromCuts(sub, fineCuts, 0, cfg)
	coarse := spansFromCuts(sub, coarseCuts, 1, cfg)

	// Wire parent: each fine span's parent is the coarse span that contains it.
	for i := range fine {
		fine[i].ParentIdx = -1
		for j := range coarse {
			if fine[i].FromMsgID >= coarse[j].FromMsgID && fine[i].ToMsgID <= coarse[j].ToMsgID {
				// ParentIdx is index in combined result: coarse first, then fine.
				fine[i].ParentIdx = j
				break
			}
		}
	}
	for j := range coarse {
		coarse[j].ParentIdx = -1
	}

	out := make([]Span, 0, len(coarse)+len(fine))
	out = append(out, coarse...)
	// Re-map ParentIdx for fine after coarse prefix.
	nCoarse := len(coarse)
	for i := range fine {
		if fine[i].ParentIdx >= 0 {
			// already index into coarse
			_ = nCoarse
		}
		out = append(out, fine[i])
	}
	return out
}

func boundaryScore(a, b Message, idleGap time.Duration) float64 {
	score := 0.0
	// Idle gap.
	if ta, ok1 := parseTS(a.Timestamp); ok1 {
		if tb, ok2 := parseTS(b.Timestamp); ok2 && tb.After(ta) {
			if tb.Sub(ta) >= idleGap {
				score += 0.7
			} else if tb.Sub(ta) >= idleGap/3 {
				score += 0.35
			}
		}
	}
	// User turn after assistant run.
	if a.Role == "assistant" && b.Role == "user" {
		score += 0.25
		// Stronger if user text looks like a new imperative.
		if looksLikeNewTopic(b.Text) {
			score += 0.2
		}
	}
	if score > 1 {
		score = 1
	}
	return score
}

func looksLikeNewTopic(text string) bool {
	t := strings.TrimSpace(strings.ToLower(text))
	if t == "" {
		return false
	}
	prefixes := []string{
		"now ", "next ", "also ", "another ", "switch ", "instead ",
		"let's ", "lets ", "ok ", "okay ", "new ", "implement ", "fix ",
		"add ", "write ", "create ", "start ",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func cutIndices(scores []float64, threshold float64) []int {
	var cuts []int
	for i, s := range scores {
		if s >= threshold {
			cuts = append(cuts, i) // cut after sub[i]
		}
	}
	return cuts
}

func spansFromCuts(sub []Message, cuts []int, level int, cfg Config) []Span {
	if len(sub) == 0 {
		return nil
	}
	// Build ranges: start..cut inclusive of end index in sub.
	type rng struct{ lo, hi int }
	var ranges []rng
	start := 0
	for _, c := range cuts {
		if c < start {
			continue
		}
		if c-start+1 >= cfg.MinSpanMsgs {
			ranges = append(ranges, rng{start, c})
		}
		start = c + 1
	}
	if len(sub)-start >= cfg.MinSpanMsgs {
		ranges = append(ranges, rng{start, len(sub) - 1})
	} else if start < len(sub) && len(ranges) > 0 {
		// Absorb short tail into previous span.
		ranges[len(ranges)-1].hi = len(sub) - 1
	} else if start < len(sub) {
		ranges = append(ranges, rng{start, len(sub) - 1})
	}

	var out []Span
	for _, r := range ranges {
		// Seal if enough trailing messages exist after hi.
		trailing := len(sub) - 1 - r.hi
		sealed := trailing >= cfg.SealLookahead
		// Confidence: mean boundary strength at edges, or 0.5 interior.
		conf := 0.5
		if r.lo > 0 && r.lo-1 < len(sub)-1 {
			// boundary before start
			conf = 0.6
		}
		if sealed {
			conf += 0.2
		}
		if conf > 1 {
			conf = 1
		}
		label, summary := heuristicLabel(sub[r.lo : r.hi+1])
		out = append(out, Span{
			FromMsgID:  sub[r.lo].ID,
			ToMsgID:    sub[r.hi].ID,
			Level:      level,
			ParentIdx:  -1,
			Confidence: conf,
			Sealed:     sealed,
			Label:      label,
			Summary:    summary,
			Method:     "structural",
			FirstTS:    sub[r.lo].Timestamp,
			LastTS:     sub[r.hi].Timestamp,
		})
	}
	return out
}

func heuristicLabel(msgs []Message) (label, summary string) {
	// Prefer first user message text.
	var texts []string
	for _, m := range msgs {
		t := strings.TrimSpace(m.Text)
		if t == "" {
			continue
		}
		if m.Role == "user" && label == "" {
			label = firstWords(t, 8)
		}
		texts = append(texts, t)
	}
	if label == "" && len(texts) > 0 {
		label = firstWords(texts[0], 8)
	}
	if label == "" {
		label = "topic"
	}
	// Summary: first ~200 runes of concatenated text.
	joined := strings.Join(texts, " ")
	summary = firstWords(joined, 40)
	return label, summary
}

func firstWords(s string, n int) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return ""
	}
	if len(fields) > n {
		fields = fields[:n]
	}
	out := strings.Join(fields, " ")
	if len(out) > 120 {
		out = out[:117] + "..."
	}
	return out
}

func parseTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Pk computes the Pk boundary metric (Beeferman et al.) between gold
// and hyp boundary sets over a sequence of length n (message count).
// lower is better; 0 is perfect.
func Pk(n int, goldCuts, hypCuts []int, window int) float64 {
	if n < 2 {
		return 0
	}
	if window <= 0 {
		window = max(1, n/10)
	}
	gold := cutSet(goldCuts)
	hyp := cutSet(hypCuts)
	var disagree, total float64
	for i := 0; i+window < n; i++ {
		j := i + window
		// Same segment in ref? (no gold cut strictly between i and j)
		sameGold := !hasCutBetween(gold, i, j)
		sameHyp := !hasCutBetween(hyp, i, j)
		if sameGold != sameHyp {
			disagree++
		}
		total++
	}
	if total == 0 {
		return 0
	}
	return disagree / total
}

// WindowDiff is a simple windowed boundary mismatch rate (Pevzner &
// Hearst style). lower is better.
func WindowDiff(n int, goldCuts, hypCuts []int, window int) float64 {
	if n < 2 {
		return 0
	}
	if window <= 0 {
		window = max(1, n/10)
	}
	gold := cutSet(goldCuts)
	hyp := cutSet(hypCuts)
	var errSum, total float64
	for i := 0; i+window < n; i++ {
		g := countCutsBetween(gold, i, i+window)
		h := countCutsBetween(hyp, i, i+window)
		if g != h {
			errSum++
		}
		total++
	}
	if total == 0 {
		return 0
	}
	return errSum / total
}

func cutSet(cuts []int) map[int]bool {
	m := make(map[int]bool, len(cuts))
	for _, c := range cuts {
		m[c] = true
	}
	return m
}

func hasCutBetween(cuts map[int]bool, i, j int) bool {
	for k := i; k < j; k++ {
		if cuts[k] {
			return true
		}
	}
	return false
}

func countCutsBetween(cuts map[int]bool, i, j int) int {
	n := 0
	for k := i; k < j; k++ {
		if cuts[k] {
			n++
		}
	}
	return n
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
