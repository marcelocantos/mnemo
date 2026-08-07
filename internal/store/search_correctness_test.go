// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Correctness tests for 🎯T144 unified search.
//
// The ranking tests prove hits are ORDERED sensibly. These prove they
// are the RIGHT hits, carrying the RIGHT content — a distinction worth
// keeping, because a hydration bug that pairs corpus A's ids with
// corpus B's rows produces a result set that is well-ordered, fully
// populated, and entirely wrong.

// TestHitsResolveToTheirOwnRows is the hydration oracle. Each corpus is
// seeded with a uniquely identifiable body, so a hit whose content came
// from the wrong table or the wrong row is detectable rather than
// merely plausible.
func TestHitsResolveToTheirOwnRows(t *testing.T) {
	s := newTestStore(t, t.TempDir())

	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('sess-A', 'p', 'user', 'zebrafish appears in a message', '2026-01-01T00:00:00Z', 'user', 0, 'text')`)
	mustExec(t, s, `INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at)
		VALUES ('o/r', 'docs/z.md', 'md', 'Zebrafish doc', 'zebrafish appears in a doc', 'h', 10, '2026-01-01', '2026-01-01')`)
	mustExec(t, s, `INSERT INTO git_commits (repo, commit_hash, author_name, author_email, commit_date, subject, body)
		VALUES ('o/r', 'deadbeef', 'a', 'a@b', '2026-01-01T00:00:00Z', 'zebrafish appears in a commit', '')`)

	res, err := s.UnifiedSearch("zebrafish", []string{"message", "doc", "commit"}, 10, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("got %d hits, want 3 (one per corpus)", len(res.Hits))
	}

	want := map[string]string{
		"message": "message",
		"doc":     "doc",
		"commit":  "commit",
	}
	for _, h := range res.Hits {
		marker, ok := want[h.Kind]
		if !ok {
			t.Errorf("unexpected kind %q", h.Kind)
			continue
		}
		combined := h.Title + " " + h.Body
		if !strings.Contains(combined, marker) {
			t.Errorf("%s hit carries %q, which does not identify a %s row — "+
				"hydration paired an id with the wrong table or row",
				h.Kind, strings.TrimSpace(combined), h.Kind)
		}
		delete(want, h.Kind)
	}
	if len(want) > 0 {
		t.Errorf("corpora produced no hit: %v", want)
	}
}

// TestKindsFilterRestrictsCorpora: asking for one corpus must search
// exactly that corpus. A filter that is accepted and ignored is worse
// than one that is rejected.
func TestKindsFilterRestrictsCorpora(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCorpora(t, s)

	res, err := s.UnifiedSearch("watcher", []string{"doc"}, 10, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Corpora) != 1 || res.Corpora[0] != "doc" {
		t.Fatalf("searched %v, want exactly [doc]", res.Corpora)
	}
	for _, h := range res.Hits {
		if h.Kind != "doc" {
			t.Errorf("kinds=[doc] returned a %s hit", h.Kind)
		}
	}
	if len(res.Hits) == 0 {
		t.Error("expected the doc corpus to match")
	}
}

// TestDefaultCorpusSetExcludesNonDefault pins that the default is a
// curated set rather than every registered corpus — the bound on
// per-search cost.
func TestDefaultCorpusSetExcludesNonDefault(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	res, err := s.UnifiedSearch("anything", nil, 10, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	searched := map[string]bool{}
	for _, c := range res.Corpora {
		searched[c] = true
	}
	if searched["plan"] || searched["config"] || searched["skill"] || searched["audit"] {
		t.Errorf("default search included an out-of-default corpus: %v", res.Corpora)
	}
	if !searched["message"] {
		t.Error("default search must include messages")
	}
	// But they must still be reachable explicitly.
	res, err = s.UnifiedSearch("anything", []string{"plan"}, 10, time.Now())
	if err != nil {
		t.Fatalf("explicit non-default corpus: %v", err)
	}
	if len(res.Corpora) != 1 || res.Corpora[0] != "plan" {
		t.Errorf("explicit kinds=[plan] searched %v", res.Corpora)
	}
}

// TestLimitIsRespected: the caller's limit bounds the merged set, not
// each corpus independently.
func TestLimitIsRespected(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for i := 0; i < 40; i++ {
		mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'p', 'user', 'recurring term in every document', '2026-01-01T00:00:00Z', 'user', 0, 'text')`,
			fmt.Sprintf("s%d", i))
	}
	for _, limit := range []int{1, 5, 25} {
		res, err := s.UnifiedSearch("recurring", nil, limit, time.Now())
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(res.Hits) > limit {
			t.Errorf("limit %d returned %d hits", limit, len(res.Hits))
		}
	}
}

// TestNoMatchIsEmptyNotError: a query nothing matches is a normal
// outcome.
func TestNoMatchIsEmptyNotError(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCorpora(t, s)

	res, err := s.UnifiedSearch("supercalifragilisticexpialidocious", nil, 10, time.Now())
	if err != nil {
		t.Fatalf("a no-match query must not error: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("got %d hits for a term present in no corpus", len(res.Hits))
	}
}

// TestSegmentEnrichmentAttachesSpan is the 🎯T144 framing made testable:
// segmentation is a richer search signal, not a separate domain, so a
// message hit must carry the span that encloses it rather than
// requiring a second tool call.
func TestSegmentEnrichmentAttachesSpan(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('sess-1', 'p', 'user', 'the descriptor table filled up', '2026-01-01T00:00:00Z', 'user', 0, 'text')`)

	var msgID int64
	if err := s.readDB.QueryRow(`SELECT id FROM messages LIMIT 1`).Scan(&msgID); err != nil {
		t.Fatalf("read message id: %v", err)
	}
	mustExec(t, s, `INSERT INTO topic_segments
		(session_id, from_msg_id, to_msg_id, level, method, confidence, sealed, label, summary, repo)
		VALUES ('sess-1', ?, ?, 0, 'llm', 0.9, 1, 'FD exhaustion', 'the watcher ran out of descriptors', 'o/r')`,
		msgID-5, msgID+5)

	res, err := s.UnifiedSearch("descriptor", []string{"message"}, 10, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	h := res.Hits[0]
	if h.SegmentLabel != "FD exhaustion" {
		t.Errorf("segment label = %q, want %q — the enclosing span was not attached",
			h.SegmentLabel, "FD exhaustion")
	}
	if !strings.Contains(h.SegmentSummary, "descriptors") {
		t.Errorf("segment summary = %q, want the span's summary", h.SegmentSummary)
	}
}

// TestSegmentEnrichmentIsOptional: a message with no enclosing span
// must still return, unenriched. Enrichment is additive.
func TestSegmentEnrichmentIsOptional(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('sess-1', 'p', 'user', 'the descriptor table filled up', '2026-01-01T00:00:00Z', 'user', 0, 'text')`)

	res, err := s.UnifiedSearch("descriptor", []string{"message"}, 10, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("a message with no enclosing span must still be returned")
	}
	if res.Hits[0].SegmentLabel != "" {
		t.Errorf("unexpected segment label %q with no span seeded", res.Hits[0].SegmentLabel)
	}
}

// TestSmallestEnclosingSpanWins: spans are hierarchical, so enrichment
// must pick the tightest one — the broadest span is nearly always the
// least informative.
func TestSmallestEnclosingSpanWins(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	mustExec(t, s, `INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
		VALUES ('sess-1', 'p', 'user', 'the descriptor table filled up', '2026-01-01T00:00:00Z', 'user', 0, 'text')`)
	var msgID int64
	if err := s.readDB.QueryRow(`SELECT id FROM messages LIMIT 1`).Scan(&msgID); err != nil {
		t.Fatalf("read id: %v", err)
	}
	mustExec(t, s, `INSERT INTO topic_segments
		(session_id, from_msg_id, to_msg_id, level, method, confidence, sealed, label, summary, repo)
		VALUES ('sess-1', ?, ?, 0, 'llm', 0.9, 1, 'whole session', 'everything', 'o/r')`,
		msgID-100, msgID+100)
	mustExec(t, s, `INSERT INTO topic_segments
		(session_id, from_msg_id, to_msg_id, level, method, confidence, sealed, label, summary, repo)
		VALUES ('sess-1', ?, ?, 1, 'llm', 0.9, 1, 'tight span', 'specifics', 'o/r')`,
		msgID-1, msgID+1)

	res, err := s.UnifiedSearch("descriptor", []string{"message"}, 10, time.Now())
	if err != nil {
		t.Fatalf("UnifiedSearch: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("no hits")
	}
	if got := res.Hits[0].SegmentLabel; got != "tight span" {
		t.Errorf("attached span %q, want the smallest enclosing one (%q)", got, "tight span")
	}
}

// TestOneBadCorpusDoesNotFailTheSearch: robustness. Corpora are queried
// independently, so a failure in one must degrade that corpus rather
// than the whole result — the others still have answers.
func TestOneBadCorpusDoesNotFailTheSearch(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedCorpora(t, s)

	// An FTS5 syntax error affects every corpus equally, so instead
	// drop one corpus's index out from under the query.
	if _, err := s.writeDB.Exec(`DROP TABLE docs_fts`); err != nil {
		t.Skipf("cannot drop docs_fts in this build: %v", err)
	}

	res, err := s.UnifiedSearch("watcher", []string{"message", "doc", "commit"}, 10, time.Now())
	if err != nil {
		t.Fatalf("one broken corpus must not fail the whole search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("the healthy corpora returned nothing")
	}
	if res.Degraded["doc"] == "" {
		t.Error("the broken corpus must be reported in Degraded, not silently dropped")
	}
	for _, h := range res.Hits {
		if h.Kind == "doc" {
			t.Error("the broken corpus returned hits")
		}
	}
}
