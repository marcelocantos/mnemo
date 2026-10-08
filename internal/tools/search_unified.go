// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marcelocantos/mnemo/internal/store"
)

// unifiedSearch renders a cross-corpus search (🎯T144).
//
// The output is deliberately readable rather than raw JSON: every hit
// names the corpus it came from and how it was ranked, so an agent can
// see that a doc and a commit were interleaved on comparable evidence
// rather than on raw BM25 scores that mean different things per index.
func (h *callHandler) unifiedSearch(query, kindsArg string, limit int,
	sessionType, repoFilter string, contextBefore, contextAfter int,
	substantiveOnly bool,
) (string, bool, error) {
	var kinds []string
	for _, k := range strings.Split(kindsArg, ",") {
		if k = strings.TrimSpace(k); k != "" {
			kinds = append(kinds, k)
		}
	}
	// "all" is a convenience for the full registry rather than the
	// default set — spelled out so the cost is a choice.
	if len(kinds) == 1 && kinds[0] == "all" {
		kinds = store.AllCorpusKinds()
	}

	res, err := h.mem.UnifiedSearchOpts(query, store.UnifiedOpts{
		Kinds:           kinds,
		Limit:           limit,
		SessionType:     sessionType,
		Repo:            repoFilter,
		ContextBefore:   contextBefore,
		ContextAfter:    contextAfter,
		SubstantiveOnly: substantiveOnly,
	}, time.Now())
	if err != nil {
		return fmt.Sprintf("search failed: %v", err), true, nil
	}
	if len(res.Hits) == 0 {
		return fmt.Sprintf("No results found across %s. Try different terms — "+
			"the content may use different vocabulary than expected.",
			strings.Join(res.Corpora, ", ")), false, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d hits across %s\n", len(res.Hits), strings.Join(res.Corpora, ", "))
	// Naming the ranking scale is not decoration. Fused hits are ranked
	// quality-blind — a corpus's #1 counts as a #1 however poor it is — so
	// a reader comparing them against calibrated ones is comparing two
	// different claims.
	//
	// This reports res.Ranking rather than inferring the scale from
	// Degraded being non-empty: fusion also triggers on evidence that is
	// fresh but too thin to compare across corpora, and that case leaves
	// Degraded empty. Inferring it silently mislabelled those results as
	// calibrated.
	if res.Ranking == "rank_fusion" {
		var parts []string
		for corpus, why := range res.Degraded {
			parts = append(parts, fmt.Sprintf("%s (%s)", corpus, why))
		}
		// Sorted so repeated identical searches render identically; map
		// iteration order would otherwise reshuffle this line per call.
		sort.Strings(parts)
		if len(parts) > 0 {
			fmt.Fprintf(&b, "Ranked by fusion rather than calibration: %s\n",
				strings.Join(parts, "; "))
		} else {
			fmt.Fprintln(&b, "Ranked by fusion rather than calibration: "+
				"a corpus in scope has too little sampled evidence to compare.")
		}
	}
	b.WriteString("\n")

	for _, hit := range res.Hits {
		fmt.Fprintf(&b, "[%s] %s", hit.Kind, hit.Title)
		if hit.Meta != "" {
			fmt.Fprintf(&b, "  — %s", hit.Meta)
		}
		if hit.TS != "" {
			fmt.Fprintf(&b, "  %s", hit.TS)
		}
		fmt.Fprintf(&b, "  (%s %.2f)\n", hit.Ranking, hit.Score)

		if hit.SegmentLabel != "" {
			fmt.Fprintf(&b, "  span: %s", hit.SegmentLabel)
			if hit.SegmentSummary != "" {
				fmt.Fprintf(&b, " — %s", hit.SegmentSummary)
			}
			b.WriteString("\n")
		}
		if hit.Body != "" {
			body := hit.Body
			// A tool output is the full body of a persisted result — up to
			// 4 MiB — and the match is usually well past its head, so show
			// a window around the first query term rather than the whole
			// thing or its first lines (🎯T192).
			if hit.Kind == "tool_output" {
				body = excerpt(body, query, toolOutputExcerptRunes)
			}
			fmt.Fprintf(&b, "  %s\n", body)
		}
		// Message hits keep their surrounding context, which is the
		// affordance the pre-🎯T144 tool was most used for.
		if hit.Message != nil {
			for _, c := range hit.Message.Before {
				fmt.Fprintf(&b, "    - %s: %s\n", c.Role, truncate(c.Text, 160))
			}
			for _, c := range hit.Message.After {
				fmt.Fprintf(&b, "    + %s: %s\n", c.Role, truncate(c.Text, 160))
			}
		}
		b.WriteString("\n")
	}
	return b.String(), false, nil
}

// truncate bounds a context line.
func truncate(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// toolOutputExcerptRunes bounds the excerpt shown for a tool_output hit.
const toolOutputExcerptRunes = 400

// excerpt returns a window of about max runes from body centred on the
// first occurrence of any term in query, or the head of body when no
// term occurs. FTS5 syntax (quotes, operators, column filters, *) is
// stripped before matching, and newlines are flattened so the excerpt
// stays one block in the rendered result.
func excerpt(body, query string, max int) string {
	flat := strings.Join(strings.Fields(body), " ")
	runes := []rune(flat)
	if len(runes) <= max {
		return flat
	}
	lower := strings.ToLower(flat)
	at := -1
	for _, raw := range strings.Fields(query) {
		term := strings.ToLower(strings.Trim(raw, `"'*():{}`))
		switch term {
		case "", "and", "or", "not", "near":
			continue
		}
		if i := strings.Index(lower, term); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := 0
	if at > 0 {
		// Byte offset to rune offset, then centre the window.
		start = utf8.RuneCountInString(flat[:at]) - max/2
		if start < 0 {
			start = 0
		}
	}
	end := start + max
	if end > len(runes) {
		end = len(runes)
		start = end - max
		if start < 0 {
			start = 0
		}
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}
