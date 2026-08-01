// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strconv"
	"strings"
	"time"
)

// decisionMinAgeSinceFirstSeen is the "≥ 1 week since first observation"
// clause of the high-signal filter: a decision must have survived a week
// before it earns a place in the clustering corpus. A decision reversed
// within days is churn, not a durable topic.
const decisionMinAgeSinceFirstSeen = 7 * 24 * time.Hour

// decisionMinRationaleWords is the "≥ 1-paragraph rationale" clause:
// a bare "yes" / "ok" confirmation with no reasoning is not a decision
// worth clustering. Measured over the combined proposal + confirmation
// text.
const decisionMinRationaleWords = 20

// decisionCandidate is the raw material the high-signal predicate judges:
// the two texts, when the decision was first observed, and the reference
// "now" (injected so the predicate is deterministic under test).
type decisionCandidate struct {
	ProposalText     string
	ConfirmationText string
	FirstSeen        time.Time
	Now              time.Time
}

// decisionIsHighSignal decides whether a decision earns a place in the
// clustering corpus (docs/design/vault-clustering.md § Inputs 1). The
// design names three clauses; each guards a distinct false positive:
//
//   - confirmed, not just proposed — a proposal the user never accepted
//     is not a decision;
//   - ≥ 1-paragraph rationale — a terse "yes" carries no topical signal;
//   - ≥ 1 week since first observation — a decision reversed within days
//     is churn, not a durable theme.
//
// The same filter is reused by the Slice 9 lessons extractor, so its
// false-positive behaviour is worth getting right.
func decisionIsHighSignal(c decisionCandidate) bool {
	// Confirmed: a non-empty confirmation is what distinguishes an
	// accepted decision from an open proposal.
	if strings.TrimSpace(c.ConfirmationText) == "" {
		return false
	}
	// ≥ 1-paragraph rationale across both texts combined.
	combined := c.ProposalText + " " + c.ConfirmationText
	if len(strings.Fields(combined)) < decisionMinRationaleWords {
		return false
	}
	// ≥ 1 week since first observation. A zero FirstSeen (unparseable
	// timestamp) fails closed rather than admitting undated rows.
	if c.FirstSeen.IsZero() {
		return false
	}
	return c.Now.Sub(c.FirstSeen) >= decisionMinAgeSinceFirstSeen
}

// DecisionCorpusDocs returns the `decision` stream of the clustering
// corpus (docs/design/vault-clustering.md § Inputs 1): high-signal
// confirmed decisions, each weighted DecisionStreamWeight.
//
// The decisions table has no summary column, so the doc text is the
// proposal + confirmation concatenation trimmed to a representative
// window (decisionSummaryTokenCap). Admission is governed by
// decisionIsHighSignal.
func (s *Store) DecisionCorpusDocs() ([]ClusterCorpusDoc, error) {
	rows, err := s.readDB.Query(`
		SELECT id, COALESCE(proposal_text, ''), COALESCE(confirmation_text, ''),
		       COALESCE(repo, ''), COALESCE(timestamp, '')
		FROM decisions
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	var out []ClusterCorpusDoc
	for rows.Next() {
		var (
			id                           int64
			proposal, confirmation, repo string
			timestamp                    string
		)
		if err := rows.Scan(&id, &proposal, &confirmation, &repo, &timestamp); err != nil {
			return nil, err
		}

		firstSeen := parseDecisionTimestamp(timestamp)
		if !decisionIsHighSignal(decisionCandidate{
			ProposalText:     proposal,
			ConfirmationText: confirmation,
			FirstSeen:        firstSeen,
			Now:              now,
		}) {
			continue
		}

		text := firstNWords(
			strings.TrimSpace(proposal+"\n"+confirmation), decisionSummaryTokenCap)
		if text == "" {
			continue
		}

		out = append(out, ClusterCorpusDoc{
			DocID:    "decision:" + strconv.FormatInt(id, 10),
			Kind:     "decision",
			EntityID: strconv.FormatInt(id, 10),
			Repo:     repo,
			Text:     text,
			TS:       timestamp,
			Weight:   DecisionStreamWeight,
		})
	}
	return out, rows.Err()
}

// parseDecisionTimestamp tolerates the timestamp spellings the decisions
// table has carried over time (RFC3339, RFC3339 with nanos, and the
// SQLite datetime('now') form). An unparseable value yields the zero
// time, which decisionIsHighSignal fails closed on.
func parseDecisionTimestamp(ts string) time.Time {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
