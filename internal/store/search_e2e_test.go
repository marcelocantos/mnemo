// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// End-to-end smoke tests for unified search (🎯T144).
//
// The unit tests exercise one corpus or two. These seed EVERY registered
// corpus, calibrate them all, and run realistic multi-word queries
// through the whole path — which is where this target's real defects
// have surfaced. Three so far, none catchable by a single-corpus test:
//
//   - segment hits rendered blank, because topic_segments has a TEXT id
//     and hydration keyed on `id` rather than the FTS rowid. Eleven
//     corpora have `id INTEGER PRIMARY KEY` (where the two coincide), so
//     every per-corpus test passed.
//   - calibration was inert, because probes were single terms while real
//     queries are multi-word ORs that score far higher.
//   - calibration never ran at all in the daemon, because the reconciler
//     was registered behind a stream that never finishes.
//
// The lesson each time: correctness per corpus does not imply
// correctness across corpora. These tests are the across.

// seedAllCorpora fills every registered corpus with graded matches for a
// shared vocabulary, so one query can legitimately hit all of them.
func seedAllCorpora(t *testing.T, s *Store) {
	t.Helper()
	// Four graded strengths, so the ranking has something to
	// discriminate BETWEEN. A fixture with two levels can only ever
	// produce two score bands, and a test asserting more would be
	// asserting against the fixture rather than the ranking.
	phrase := func(i int) string {
		switch i % 4 {
		case 0:
			return "watcher watcher watcher watcher descriptor descriptor exhaustion exhaustion under load"
		case 1:
			return "watcher watcher descriptor exhaustion under load"
		case 2:
			return "watcher descriptor noted in passing"
		default:
			return "unrelated throttle compaction reconciler prose"
		}
	}
	pad := func(i int) string {
		return strings.Repeat("surrounding context prose. ", 1+i%5)
	}

	for i := 0; i < 60; i++ {
		p, q := phrase(i), pad(i)
		mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("s%d", i), p+" "+q)
		mustExec(t, s, `INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at)
			VALUES ('o/r', ?, 'md', ?, ?, ?, 10, '2026-01-01', '2026-01-01')`,
			fmt.Sprintf("d%d.md", i), fmt.Sprintf("doc %d", i), p+" "+q, fmt.Sprintf("h%d", i))
		mustExec(t, s, `INSERT INTO git_commits (repo, commit_hash, author_name, author_email, commit_date, subject, body)
			VALUES ('o/r', ?, 'a', 'a@b', '2026-01-01T00:00:00Z', ?, ?)`,
			fmt.Sprintf("c%d", i), p, q)
		mustExec(t, s, `INSERT INTO github_prs (repo, pr_number, title, body, state, author, created_at, updated_at, url)
			VALUES ('o/r', ?, ?, ?, 'open', 'a', '2026-01-01', '2026-01-01', 'u')`,
			i, p, q)
		mustExec(t, s, `INSERT INTO targets (repo, file_path, target_id, name, status, weight, description, raw_text)
			VALUES ('o/r', 't.md', ?, ?, 'identified', 1, ?, ?)`,
			fmt.Sprintf("T%d", i), p, p+" "+q, q)
		mustExec(t, s, `INSERT INTO memories (project, file_path, name, description, memory_type, content, updated_at)
			VALUES ('p', ?, ?, 'd', 'user', ?, '2026-01-01')`,
			fmt.Sprintf("m%d.md", i), fmt.Sprintf("memory %d", i), p+" "+q)
		mustExec(t, s, `INSERT INTO decisions (session_id, proposal_msg_id, confirmation_msg_id, proposal_text, confirmation_text, repo, timestamp)
			VALUES (?, ?, ?, ?, 'agreed', 'o/r', '2026-01-01')`,
			fmt.Sprintf("s%d", i), i, i+1, p+" "+q)
		mustExec(t, s, `INSERT INTO plans (repo, file_path, phase, content, updated_at)
			VALUES ('o/r', ?, 'ph', ?, '2026-01-01')`, fmt.Sprintf("p%d.md", i), p+" "+q)
		mustExec(t, s, `INSERT INTO claude_configs (repo, file_path, content, updated_at)
			VALUES ('o/r', ?, ?, '2026-01-01')`, fmt.Sprintf("C%d.md", i), p+" "+q)
		mustExec(t, s, `INSERT INTO skills (file_path, name, description, content, updated_at)
			VALUES (?, ?, 'd', ?, '2026-01-01')`,
			fmt.Sprintf("k%d.md", i), fmt.Sprintf("skill %d", i), p+" "+q)
		mustExec(t, s, `INSERT INTO audit_entries (repo, file_path, date, skill, version, summary, raw_text)
			VALUES ('o/r', ?, '2026-01-01', 'release', 'v1', ?, ?)`,
			fmt.Sprintf("audit%d.md", i), p, q)
		// topic_segments carries a TEXT id — the corpus whose hydration
		// bug only a cross-corpus test could find.
		mustExec(t, s, `INSERT INTO topic_segments
			(id, session_id, from_msg_id, to_msg_id, level, method, confidence, sealed, label, summary, repo)
			VALUES (?, ?, ?, ?, 0, 'llm', 0.9, 1, ?, ?, 'o/r')`,
			fmt.Sprintf("seg_%08x", i), fmt.Sprintf("s%d", i), i*10, i*10+9,
			fmt.Sprintf("span %d", i), p+" "+q)
	}
}

func calibrateAll(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	rec := calibrationReconcilerStream{s}
	if _, err := rec.Reconcile(context.Background(), now); err != nil {
		t.Fatalf("calibration pass: %v", err)
	}
}

// TestE2EEveryCorpusContributes is the headline smoke test: with every
// corpus holding genuine matches, a single query must return hits from
// all of them.
func TestE2EEveryCorpusContributes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedAllCorpora(t, s)
	now := time.Now()
	calibrateAll(t, s, now)

	all := AllCorpusKinds()
	res, err := s.UnifiedSearchOpts("watcher descriptor exhaustion",
		UnifiedOpts{Kinds: all, Limit: 200, SessionType: "all", SubstantiveOnly: true}, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	seen := map[string]int{}
	for _, h := range res.Hits {
		seen[h.Kind]++
	}
	var missing []string
	for _, kind := range all {
		if seen[kind] == 0 {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		t.Errorf("corpora seeded with matching content returned nothing: %s\n"+
			"(got %v). A corpus that cannot contribute is a corpus whose "+
			"registry entry is wrong.", strings.Join(missing, ", "), seen)
	}
	t.Logf("contributions across %d corpora: %v", len(all), seen)
}

// TestE2EEveryHitHydrates is the oracle that would have caught the
// segment blank-hit bug: every returned hit must carry displayable
// content, whatever corpus it came from.
func TestE2EEveryHitHydrates(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedAllCorpora(t, s)
	now := time.Now()
	calibrateAll(t, s, now)

	res, err := s.UnifiedSearchOpts("watcher descriptor exhaustion",
		UnifiedOpts{Kinds: AllCorpusKinds(), Limit: 150, SessionType: "all", SubstantiveOnly: true}, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	blank := map[string]int{}
	for _, h := range res.Hits {
		if strings.TrimSpace(h.Title) == "" && strings.TrimSpace(h.Body) == "" {
			blank[h.Kind]++
		}
	}
	if len(blank) > 0 {
		t.Errorf("hits rendered with no content, by corpus: %v\n"+
			"Hydration resolves the FTS rowid against the source table; a "+
			"corpus whose id column is not its rowid returns nothing and the "+
			"hit renders blank.", blank)
	}
}

// TestE2ERankingIsReasonable checks the ordering makes sense rather than
// merely existing: strong matches must outrank weak ones, and no single
// corpus may own the head.
func TestE2ERankingIsReasonable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedAllCorpora(t, s)
	now := time.Now()
	calibrateAll(t, s, now)

	res, err := s.UnifiedSearchOpts("watcher descriptor exhaustion",
		UnifiedOpts{Kinds: AllCorpusKinds(), Limit: 40, SessionType: "all", SubstantiveOnly: true}, now)
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) < 20 {
		t.Fatalf("expected a substantial result set, got %d", len(res.Hits))
	}

	// 1. Scores must be ordered and in range.
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i].Score > res.Hits[i-1].Score {
			t.Fatalf("results are not ordered: position %d scored %.4f above "+
				"position %d's %.4f", i, res.Hits[i].Score, i-1, res.Hits[i-1].Score)
		}
	}
	for _, h := range res.Hits {
		if h.Score < -0.01 || h.Score > 1.01 {
			t.Errorf("%s hit scored %.4f, outside [0,1]", h.Kind, h.Score)
		}
	}

	// 2. Ranking must actually discriminate — not one value repeated.
	distinct := map[float64]bool{}
	for _, h := range res.Hits {
		distinct[float64(int(h.Score*50))/50] = true
	}
	if len(distinct) < 3 {
		t.Errorf("only %d distinct score bands across %d hits (%v); ranking is "+
			"not discriminating, which is what an inert calibration looks like",
			len(distinct), len(res.Hits), distinct)
	}

	// 3. Strong matches must beat weak ones.
	strongInHead := 0
	for _, h := range res.Hits[:10] {
		if strings.Count(strings.ToLower(h.Title+" "+h.Body), "watcher") >= 2 {
			strongInHead++
		}
	}
	if strongInHead < 5 {
		t.Errorf("only %d of the top 10 hits are strong matches; ranking is not "+
			"putting the best content first", strongInHead)
	}

	// 4. The merged result must not be one corpus wearing a hat.
	//
	// Measured over the top 20 rather than the top 10, and as a count of
	// DISTINCT corpora rather than a per-corpus cap. Every corpus in this
	// fixture holds identical text, so which one leads is decided by
	// small differences in distribution shape — near the top of a
	// distribution, tiny magnitude differences become large quantile
	// differences. Asserting even interleaving at the very head would be
	// asserting something the fixture cannot support; asserting that the
	// result set is genuinely multi-corpus is the real property.
	head := map[string]int{}
	for _, h := range res.Hits[:20] {
		head[h.Kind]++
	}
	if len(head) < 3 {
		t.Errorf("only %d distinct corpora in the top 20 (%v); the merged result "+
			"is dominated by one source", len(head), head)
	}
	t.Logf("top-20 composition: %v; %d distinct score bands", head, len(distinct))
}

// TestE2EScopedQueriesStayScoped: asking for a subset must search only
// that subset, end to end across every corpus.
func TestE2EScopedQueriesStayScoped(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedAllCorpora(t, s)
	now := time.Now()
	calibrateAll(t, s, now)

	for _, kind := range AllCorpusKinds() {
		res, err := s.UnifiedSearchOpts("watcher descriptor",
			UnifiedOpts{Kinds: []string{kind}, Limit: 10, SessionType: "all", SubstantiveOnly: true}, now)
		if err != nil {
			t.Errorf("kinds=[%s]: %v", kind, err)
			continue
		}
		if len(res.Hits) == 0 {
			t.Errorf("kinds=[%s] returned nothing despite seeded matches", kind)
			continue
		}
		for _, h := range res.Hits {
			if h.Kind != kind {
				t.Errorf("kinds=[%s] returned a %s hit", kind, h.Kind)
			}
		}
	}
}

// TestE2ECalibrationCoversAllCorpora: after one reconcile pass, every
// corpus with enough content must be carrying real evidence rather than
// sitting silently at the neutral prior.
func TestE2ECalibrationCoversAllCorpora(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedAllCorpora(t, s)
	now := time.Now()
	calibrateAll(t, s, now)

	cals, err := s.LoadCalibrations()
	if err != nil {
		t.Fatalf("LoadCalibrations: %v", err)
	}
	var uncalibrated []string
	for _, kind := range AllCorpusKinds() {
		c := cals[kind]
		if c == nil || c.Evidence() == 0 {
			uncalibrated = append(uncalibrated, kind)
			continue
		}
		if c.Quantiles[0] > c.Quantiles[len(c.Quantiles)-1] {
			t.Errorf("%s: quantile boundaries are not ascending", kind)
		}
	}
	if len(uncalibrated) > 0 {
		t.Errorf("corpora with 60 seeded documents produced no usable "+
			"calibration: %s", strings.Join(uncalibrated, ", "))
	}
}
