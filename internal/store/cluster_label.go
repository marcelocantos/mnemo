// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/marcelocantos/mnemo/internal/cluster"
)

// LabelPath names which step of the labelling chain produced a label.
type LabelPath string

const (
	LabelPathUserAnchor LabelPath = "user_anchor"
	LabelPathLLM        LabelPath = "llm"
	LabelPathBigram     LabelPath = "bigram"
	LabelPathToken      LabelPath = "token"
)

// LabelGate is why a vault_user anchor was rejected (or empty if accepted).
type LabelGate string

const (
	LabelGateNone            LabelGate = ""
	LabelGateNotClosest      LabelGate = "not_centroid_closest"
	LabelGateBelowMinTokens  LabelGate = "below_min_tokens"
	LabelGateFilenameExclude LabelGate = "filename_pattern"
	LabelGateTitleNoOverlap  LabelGate = "title_content_no_overlap"
	LabelGateNoVaultUser     LabelGate = "no_vault_user_member"
)

// ThemeLabel is the outcome of labelling one cluster.
type ThemeLabel struct {
	Label      string
	Slug       string
	Path       LabelPath
	GateFired  LabelGate // set when a vault_user candidate was considered and rejected
	CentroidTx string
}

// defaultDailyNoteExclude matches daily-note / scratch filenames.
var defaultDailyNoteExclude = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\d{4}-\d{2}-\d{2}(\.md)?$`),
	regexp.MustCompile(`(?i)^\d{4}_\d{2}_\d{2}(\.md)?$`),
	regexp.MustCompile(`(?i)^\d{4}\.\d{2}\.\d{2}(\.md)?$`),
	regexp.MustCompile(`(?i)^(daily|journal|journals|inbox|scratch|todo|untitled)(\.md)?$`),
}

var dailyPathParts = map[string]bool{
	"daily": true, "journals": true, "journal": true,
	"inbox": true, "scratch": true,
}

// labelCluster applies the user-anchor → optional LLM → bigram chain.
// labeler is non-nil only when label.engine=llm and a provider was
// constructed; otherwise the LLM step is skipped with zero egress.
func labelCluster(ctx context.Context, docs []ClusterCorpusDoc, members []int, cent map[string]float64, cfg VaultClusteringConfig, labeler cluster.Labeler) ThemeLabel {
	// Centroid text ≈ top terms for inspect/gate overlap.
	centroidTx := topTerms(cent, 12)
	minTok := cfg.EffectiveUserMinTokens()

	// --- User anchor path -------------------------------------------------
	vaultIdxs := make([]int, 0)
	for _, mi := range members {
		if docs[mi].Kind == "vault_user" {
			vaultIdxs = append(vaultIdxs, mi)
		}
	}
	var gate LabelGate = LabelGateNoVaultUser
	if len(vaultIdxs) > 0 {
		// Centroid-closest vault_user.
		bestI, bestSim := -1, -1.0
		for _, mi := range vaultIdxs {
			sim := cosine(tfVector(docs[mi].Text), cent)
			if sim > bestSim {
				bestSim = sim
				bestI = mi
			}
		}
		gate = LabelGateNone
		if bestI < 0 {
			gate = LabelGateNotClosest
		} else {
			d := docs[bestI]
			title, body := splitTitleBody(d.Text)
			bodyToks := countTokensApprox(stripFrontmatterAndFences(body))
			switch {
			case bodyToks < minTok:
				gate = LabelGateBelowMinTokens
			case filenameExcluded(title, d.EntityID, cfg.Label.UserFilenameExclude):
				// Prefer path from first line / title; entity_id alone is
				// numeric so also check title-as-filename.
				gate = LabelGateFilenameExclude
			case !titleContentOverlap(title, cent):
				gate = LabelGateTitleNoOverlap
			default:
				label := cleanLabel(title)
				if label == "" {
					label = cleanLabel(topTerms(cent, 4))
				}
				return ThemeLabel{
					Label:      label,
					Slug:       slugify(label),
					Path:       LabelPathUserAnchor,
					GateFired:  LabelGateNone,
					CentroidTx: centroidTx,
				}
			}
		}
	}

	// --- LLM path (opt-in) ------------------------------------------------
	if labeler != nil && cfg.EffectiveLabelEngine() == "llm" {
		excerpts := topExcerpts(docs, members, 5)
		if lab, err := labeler.Label(ctx, excerpts); err == nil && strings.TrimSpace(lab) != "" {
			label := cleanLabel(lab)
			return ThemeLabel{
				Label:      titleCase(label),
				Slug:       slugify(label),
				Path:       LabelPathLLM,
				GateFired:  gate,
				CentroidTx: centroidTx,
			}
		}
		// Provider error → fall through to bigram (design: degrade).
	}

	// --- Bigram fallback --------------------------------------------------
	label, path := bigramLabel(docs, members)
	return ThemeLabel{
		Label:      label,
		Slug:       slugify(label),
		Path:       path,
		GateFired:  gate,
		CentroidTx: centroidTx,
	}
}

// topExcerpts picks up to n member texts for the LLM prompt, preferring
// higher-weight docs.
func topExcerpts(docs []ClusterCorpusDoc, members []int, n int) []string {
	type scored struct {
		w float64
		t string
	}
	var all []scored
	for _, mi := range members {
		w := docs[mi].Weight
		if w <= 0 {
			w = 1
		}
		all = append(all, scored{w, docs[mi].Text})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].w != all[j].w {
			return all[i].w > all[j].w
		}
		return all[i].t < all[j].t
	})
	if n > len(all) {
		n = len(all)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = all[i].t
	}
	return out
}

func splitTitleBody(text string) (title, body string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	parts := strings.SplitN(text, "\n", 2)
	title = strings.TrimSpace(parts[0])
	title = strings.TrimPrefix(title, "# ")
	title = strings.TrimSpace(title)
	if len(parts) == 2 {
		body = parts[1]
	}
	return title, body
}

func filenameExcluded(title, entityHint string, extra []string) bool {
	candidates := []string{title, filepath.Base(title)}
	for _, c := range candidates {
		base := strings.TrimSpace(c)
		if base == "" {
			continue
		}
		base = filepath.Base(base)
		for _, re := range defaultDailyNoteExclude {
			if re.MatchString(base) {
				return true
			}
		}
		for _, pat := range extra {
			if re, err := regexp.Compile("(?i)" + pat); err == nil && re.MatchString(base) {
				return true
			}
		}
		// Path-segment exclusions when title carries a path.
		for _, part := range strings.Split(filepath.ToSlash(title), "/") {
			if dailyPathParts[strings.ToLower(part)] {
				return true
			}
		}
	}
	_ = entityHint
	return false
}

func titleContentOverlap(title string, cent map[string]float64) bool {
	toks := tokenize(title)
	if len(toks) == 0 {
		return false
	}
	for _, t := range toks {
		if _, ok := cent[t]; ok {
			return true
		}
	}
	return false
}

func bigramLabel(docs []ClusterCorpusDoc, members []int) (string, LabelPath) {
	type pair struct{ a, b string }
	counts := map[pair]float64{}
	uni := map[string]float64{}
	for _, mi := range members {
		w := docs[mi].Weight
		if w <= 0 {
			w = 1
		}
		toks := tokenize(docs[mi].Text)
		for i, t := range toks {
			uni[t] += w
			if i+1 < len(toks) {
				counts[pair{t, toks[i+1]}] += w
			}
		}
	}
	if len(counts) > 0 {
		type scored struct {
			p pair
			n float64
		}
		var all []scored
		for p, n := range counts {
			all = append(all, scored{p, n})
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].n != all[j].n {
				return all[i].n > all[j].n
			}
			if all[i].p.a != all[j].p.a {
				return all[i].p.a < all[j].p.a
			}
			return all[i].p.b < all[j].p.b
		})
		lab := titleCase(all[0].p.a + " " + all[0].p.b)
		return lab, LabelPathBigram
	}
	// Single-token fallback.
	type us struct {
		t string
		n float64
	}
	var all []us
	for t, n := range uni {
		all = append(all, us{t, n})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].t < all[j].t
	})
	if len(all) == 0 {
		return "various topics", LabelPathToken
	}
	return titleCase(all[0].t), LabelPathToken
}

func topTerms(cent map[string]float64, n int) string {
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range cent {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if n > len(all) {
		n = len(all)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = all[i].k
	}
	return strings.Join(parts, " ")
}

func cleanLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "#*_`\"'")
	s = strings.Join(strings.Fields(s), " ")
	// Cap at ~6 words for theme titles.
	fields := strings.Fields(s)
	if len(fields) > 6 {
		fields = fields[:6]
	}
	return strings.Join(fields, " ")
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "theme"
	}
	if len(out) > 48 {
		out = out[:48]
		out = strings.Trim(out, "-")
	}
	return out
}

func titleCase(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(p)
		r[0] = unicode.ToUpper(r[0])
		parts[i] = string(r)
	}
	return strings.Join(parts, " ")
}
