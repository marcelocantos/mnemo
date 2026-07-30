// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// patternToolUse builds an assistant entry carrying a single tool_use
// content block, which is the shape every pattern detector matches on.
func patternToolUse(sessionID, uuid, cwd, ts, toolName string, input map[string]any) map[string]any {
	return map[string]any{
		"type":      "assistant",
		"uuid":      uuid,
		"sessionId": sessionID,
		"timestamp": ts,
		"cwd":       cwd,
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "tool_use", "id": "toolu_" + uuid, "name": toolName, "input": input},
			},
		},
	}
}

// recentTS returns an RFC3339 stamp inside the 90-day mining window.
func recentTS(hoursAgo int) string {
	return time.Now().UTC().Add(-time.Duration(hoursAgo) * time.Hour).Format(time.RFC3339)
}

// findPattern returns the pattern of the given type, or fails.
func findPattern(t *testing.T, got []PatternCandidate, patternType string) PatternCandidate {
	t.Helper()
	for _, p := range got {
		if p.PatternType == patternType {
			return p
		}
	}
	types := make([]string, len(got))
	for i, p := range got {
		types[i] = p.PatternType
	}
	t.Fatalf("no %s pattern; got types %v", patternType, types)
	return PatternCandidate{}
}

// storeWithJSONLReads seeds two sessions in different repos that read
// transcript JSONL directly — three reads in one session, one in the
// other, so occurrence_count (4) and session_count (2) differ.
func storeWithJSONLReads(t *testing.T) *Store {
	t.Helper()
	projectDir := t.TempDir()
	const (
		cwdA = "/Users/t/work/github.com/acme/alpha"
		cwdB = "/Users/t/work/github.com/acme/beta"
	)
	writeJSONL(t, projectDir, "projA", "sess-alpha", []map[string]any{
		patternToolUse("sess-alpha", "a1", cwdA, recentTS(72), "Bash",
			map[string]any{"command": "cat ~/.claude/projects/foo/one.jsonl"}),
		patternToolUse("sess-alpha", "a2", cwdA, recentTS(71), "Bash",
			map[string]any{"command": "head -5 ~/.claude/projects/foo/two.jsonl"}),
		patternToolUse("sess-alpha", "a3", cwdA, recentTS(70), "Bash",
			map[string]any{"command": "tail ~/.claude/projects/foo/three.jsonl"}),
	})
	writeJSONL(t, projectDir, "projB", "sess-beta", []map[string]any{
		patternToolUse("sess-beta", "b1", cwdB, recentTS(24), "Bash",
			map[string]any{"command": "wc -l ~/.claude/projects/bar/four.jsonl"}),
	})

	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRefreshPatternsPersistsCounts is the core 🎯T64.7 assertion: a
// mining pass lands rows in `patterns`, and occurrence_count and
// session_count are the two different numbers the design calls for
// rather than one number reported twice.
//
// The pre-🎯T64.7 miner reported len(sessions) as "occurrences", so a
// session that read six transcripts looked like six sessions. Both the
// emission gate and the clustering weight depend on telling those apart.
func TestRefreshPatternsPersistsCounts(t *testing.T) {
	s := storeWithJSONLReads(t)

	n, err := s.RefreshPatterns(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("RefreshPatterns wrote no rows")
	}

	got, err := s.ListPatterns(PatternQuery{})
	if err != nil {
		t.Fatal(err)
	}
	p := findPattern(t, got, PatternDirectJSONLRead)

	if p.Occurrences != 4 {
		t.Errorf("Occurrences = %d, want 4", p.Occurrences)
	}
	if p.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", p.SessionCount)
	}
	if len(p.Sessions) != 2 {
		t.Errorf("Sessions = %v, want 2 entries", p.Sessions)
	}
	if len(p.Repos) != 2 {
		t.Errorf("Repos = %v, want acme/alpha + acme/beta", p.Repos)
	}
	if p.ID != patternID(PatternDirectJSONLRead, sigDirectJSONLRead) {
		t.Errorf("ID = %q, not derived from (type, signature)", p.ID)
	}
	if p.FirstSeen == "" || p.LastSeen == "" || p.FirstSeen >= p.LastSeen {
		t.Errorf("FirstSeen/LastSeen = %q/%q, want an ordered span", p.FirstSeen, p.LastSeen)
	}
	if p.Evidence == "" {
		t.Error("Evidence is empty; want a representative excerpt")
	}
	if p.Description == "" || p.Suggestion == "" {
		t.Errorf("Description/Suggestion derived empty: %q / %q", p.Description, p.Suggestion)
	}
	if !p.Emittable() {
		t.Error("4 occurrences across 2 sessions should clear the emission gate")
	}
}

// TestPatternsFTSMatches proves the external-content FTS5 table and its
// triggers are wired: a row inserted through upsertPattern is findable
// by a term from its excerpts, not only by primary key.
func TestPatternsFTSMatches(t *testing.T) {
	s := storeWithJSONLReads(t)
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListPatterns(PatternQuery{Query: "direct_jsonl_read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("FTS match on pattern_type returned %d rows, want 1", len(got))
	}

	// A term that only appears in representative_excerpts, confirming
	// the third indexed column is populated too.
	got, err = s.ListPatterns(PatternQuery{Query: "projects"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Error("FTS match on an excerpt term returned nothing")
	}
}

// TestPatternRepoFilterMatchesElements checks the repo filter looks
// inside the JSON array rather than at its serialised text. A LIKE over
// the raw column would let '","' or a fragment spanning two elements
// produce a false hit.
func TestPatternRepoFilterMatchesElements(t *testing.T) {
	s := storeWithJSONLReads(t)
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListPatterns(PatternQuery{Repo: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("repo filter alpha matched nothing")
	}

	got, err = s.ListPatterns(PatternQuery{Repo: "no-such-repo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("repo filter no-such-repo matched %d rows, want 0", len(got))
	}
}

// TestPatternEmissionGateNeedsTwoSessions pins the design's gate:
// occurrence >= 3 is not sufficient. One session that repeats a query
// three times is a habit, not a corroborated pattern, and reporting it
// would put a page in the vault on the strength of a single session.
func TestPatternEmissionGateNeedsTwoSessions(t *testing.T) {
	projectDir := t.TempDir()
	const (
		cwdA  = "/Users/t/work/github.com/acme/alpha"
		cwdB  = "/Users/t/work/github.com/acme/beta"
		query = "SELECT session_id FROM messages WHERE tool_name = 'Bash' LIMIT 10"
	)
	writeJSONL(t, projectDir, "projA", "sess-solo", []map[string]any{
		patternToolUse("sess-solo", "q1", cwdA, recentTS(50), "mnemo_query", map[string]any{"query": query}),
		patternToolUse("sess-solo", "q2", cwdA, recentTS(49), "mnemo_query", map[string]any{"query": query}),
		patternToolUse("sess-solo", "q3", cwdA, recentTS(48), "mnemo_query", map[string]any{"query": query}),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}

	// The row exists — mining records what it saw — but is gated out of
	// what callers are served.
	all, err := s.ListPatterns(PatternQuery{})
	if err != nil {
		t.Fatal(err)
	}
	solo := findPattern(t, all, PatternRepeatedQuery)
	if solo.Occurrences != 3 || solo.SessionCount != 1 {
		t.Fatalf("mined %d occurrences / %d sessions, want 3 / 1", solo.Occurrences, solo.SessionCount)
	}
	if solo.Emittable() {
		t.Error("3 occurrences in 1 session must not clear the emission gate")
	}
	served, err := s.DiscoverPatterns(0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range served {
		if p.PatternType == PatternRepeatedQuery {
			t.Error("DiscoverPatterns served a single-session pattern")
		}
	}

	// A second session running the same shape once corroborates it.
	writeJSONL(t, projectDir, "projB", "sess-second", []map[string]any{
		patternToolUse("sess-second", "q4", cwdB, recentTS(10), "mnemo_query", map[string]any{"query": query}),
	})
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}
	served, err = s.DiscoverPatterns(0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := findPattern(t, served, PatternRepeatedQuery)
	if p.Occurrences != 4 || p.SessionCount != 2 {
		t.Errorf("after corroboration: %d occurrences / %d sessions, want 4 / 2",
			p.Occurrences, p.SessionCount)
	}
}

// TestRefreshPatternsIdempotent checks a second pass over an unchanged
// corpus neither duplicates rows nor moves the counts. The row id is
// derived from (type, signature) precisely so re-mining converges.
func TestRefreshPatternsIdempotent(t *testing.T) {
	s := storeWithJSONLReads(t)

	first, err := s.RefreshPatterns(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ListPatterns(PatternQuery{})
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.RefreshPatterns(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("row count moved on an unchanged corpus: %d then %d", first, second)
	}
	after, err := s.ListPatterns(PatternQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("pattern count changed: %d then %d", len(before), len(after))
	}
	for i := range before {
		if before[i].ID != after[i].ID ||
			before[i].Occurrences != after[i].Occurrences ||
			before[i].SessionCount != after[i].SessionCount ||
			before[i].FirstSeen != after[i].FirstSeen {
			t.Errorf("row %d changed across identical passes:\n before %+v\n after  %+v",
				i, before[i], after[i])
		}
	}
}

// TestPatternFirstSeenSurvivesWindow proves first_seen accumulates
// rather than tracking the mining window. The window bounds the counts;
// the row is supposed to remember when the pattern was first observed,
// which a plain overwrite would silently reset every pass.
func TestPatternFirstSeenSurvivesWindow(t *testing.T) {
	s := storeWithJSONLReads(t)
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}
	mined := findPattern(t, mustList(t, s), PatternDirectJSONLRead)

	// Simulate an older observation than any the window can still see.
	old := "2020-01-01T00:00:00Z"
	if _, err := s.writeDB.Exec(
		`UPDATE patterns SET first_seen = ? WHERE id = ?`, old, mined.ID,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}
	again := findPattern(t, mustList(t, s), PatternDirectJSONLRead)
	if again.FirstSeen != old {
		t.Errorf("FirstSeen = %q after re-mine, want the earlier %q preserved",
			again.FirstSeen, old)
	}
	if again.LastSeen != mined.LastSeen {
		t.Errorf("LastSeen = %q, want it to stay at %q", again.LastSeen, mined.LastSeen)
	}
}

func mustList(t *testing.T, s *Store) []PatternCandidate {
	t.Helper()
	got, err := s.ListPatterns(PatternQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestPatternCorpusDocs covers acceptance criterion 4: patterns are a
// clustering input stream at weight 1.2, gated by the same emission
// filter as the vault pages, with one repo-scoped doc per repo the
// pattern spans.
func TestPatternCorpusDocs(t *testing.T) {
	s := storeWithJSONLReads(t)
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}

	docs, err := s.PatternCorpusDocs()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("no corpus docs for an emittable pattern")
	}
	repos := map[string]bool{}
	for _, d := range docs {
		if d.Kind != "pattern" {
			t.Errorf("Kind = %q, want pattern", d.Kind)
		}
		if d.Weight != PatternStreamWeight {
			t.Errorf("Weight = %v, want %v", d.Weight, PatternStreamWeight)
		}
		if d.DocID != "pattern:"+d.EntityID {
			t.Errorf("DocID = %q, want pattern:<entity_id>", d.DocID)
		}
		if d.Text == "" {
			t.Error("Text is empty; the clusterer has nothing to vectorise")
		}
		repos[d.Repo] = true
	}
	if !repos["acme/alpha"] || !repos["acme/beta"] {
		t.Errorf("repo fan-out = %v, want a doc for each of acme/alpha and acme/beta", repos)
	}
	if PatternStreamWeight != 1.2 {
		t.Errorf("PatternStreamWeight = %v, but the clustering design specifies 1.2", PatternStreamWeight)
	}
}

// TestPatternCorpusDocsRespectGate checks a sub-threshold pattern
// contributes nothing to the clustering corpus. Filtering only at the
// renderer would let the clusterer see documents no page exists for,
// which is the disagreement the shared gate exists to prevent.
func TestPatternCorpusDocsRespectGate(t *testing.T) {
	projectDir := t.TempDir()
	const cwd = "/Users/t/work/github.com/acme/alpha"
	writeJSONL(t, projectDir, "projA", "sess-thin", []map[string]any{
		patternToolUse("sess-thin", "s1", cwd, recentTS(9), "mnemo_search",
			map[string]any{"query": "flaky windows test"}),
		patternToolUse("sess-thin", "s2", cwd, recentTS(8), "mnemo_search",
			map[string]any{"query": "windows flaky test"}),
		patternToolUse("sess-thin", "s3", cwd, recentTS(7), "mnemo_search",
			map[string]any{"query": "test flaky windows"}),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}

	// Word-order-insensitive normalisation groups all three into one
	// signature: 3 occurrences, 1 session.
	mined := findPattern(t, mustList(t, s), PatternRepeatedSearch)
	if mined.Occurrences != 3 || mined.SessionCount != 1 {
		t.Fatalf("mined %d/%d, want 3 occurrences / 1 session", mined.Occurrences, mined.SessionCount)
	}
	docs, err := s.PatternCorpusDocs()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Errorf("corpus admitted %d docs for a single-session pattern", len(docs))
	}
}

// TestDiscoverPatternsMinesOnFirstCall covers the upgrade and
// fresh-install path: the table is empty until the reconciler runs, and
// answering "no patterns" then would be indistinguishable from "no
// patterns exist".
func TestDiscoverPatternsMinesOnFirstCall(t *testing.T) {
	s := storeWithJSONLReads(t)

	if rows := mustList(t, s); len(rows) != 0 {
		t.Fatalf("expected an empty patterns table before any refresh, got %d rows", len(rows))
	}

	got, err := s.DiscoverPatterns(0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("DiscoverPatterns returned nothing on an uncomputed table")
	}
	if rows := mustList(t, s); len(rows) == 0 {
		t.Error("the lazy first call did not persist what it mined")
	}
	if got[0].ComputedAt == "" {
		t.Error("ComputedAt is empty; callers cannot tell how fresh the answer is")
	}
}

// TestPatternsReconcilerHonoursCadence checks the hourly gate. The
// registry dispatcher ticks every minute for every stream, so without
// the age check here the miner would run 60x its intended cadence.
func TestPatternsReconcilerHonoursCadence(t *testing.T) {
	s := storeWithJSONLReads(t)
	r := patternsReconcilerStream{s}

	if r.Name() != "patterns" {
		t.Errorf("Name = %q, want patterns", r.Name())
	}
	if r.Interval() != patternsRefreshInterval {
		t.Errorf("Interval = %v, want %v", r.Interval(), patternsRefreshInterval)
	}

	base := time.Now()
	first, err := r.Reconcile(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("first reconcile mined nothing")
	}

	// A minute later: inside the cadence, so a no-op.
	n, err := r.Reconcile(context.Background(), base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reconcile one minute later applied %d changes, want 0", n)
	}

	// Past the interval: mines again.
	n, err = r.Reconcile(context.Background(), base.Add(patternsRefreshInterval+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != first {
		t.Errorf("reconcile past the interval applied %d changes, want %d", n, first)
	}
}

// TestPatternsReconcilerRegistered guards the wiring: a reconciler that
// exists but is not in StreamReconcilers never runs, and an unwired
// oracle decays.
func TestPatternsReconcilerRegistered(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	for _, sr := range s.StreamReconcilers() {
		if sr.Name() == "patterns" {
			return
		}
	}
	t.Error("patterns stream is not registered in StreamReconcilers()")
}

// TestTranscriptGrepRequiresASearch pins the detector narrowing: a
// `cat` of a transcript file is a direct read, not a grep, and must not
// be counted under a pattern whose description says "Grep/rg commands".
// Before this, every direct JSONL read also fired transcript_grep, so
// the two patterns were near-duplicates and one of them was wrong.
func TestTranscriptGrepRequiresASearch(t *testing.T) {
	s := storeWithJSONLReads(t)
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, p := range mustList(t, s) {
		if p.PatternType == PatternTranscriptGrep {
			t.Errorf("cat/head/tail/wc reads were counted as a grep: %+v", p)
		}
	}

	// A genuine search over the transcript directory does fire it.
	projectDir := t.TempDir()
	const (
		cwdA = "/Users/t/work/github.com/acme/alpha"
		cwdB = "/Users/t/work/github.com/acme/beta"
	)
	writeJSONL(t, projectDir, "projA", "sess-g1", []map[string]any{
		patternToolUse("sess-g1", "g1", cwdA, recentTS(30), "Bash",
			map[string]any{"command": "rg FD_ACCEPT ~/.claude/projects/proj"}),
		patternToolUse("sess-g1", "g2", cwdA, recentTS(29), "Bash",
			map[string]any{"command": "LC_ALL=C grep -r foo ~/.claude/projects/proj"}),
	})
	writeJSONL(t, projectDir, "projB", "sess-g2", []map[string]any{
		patternToolUse("sess-g2", "g3", cwdB, recentTS(5), "Bash",
			map[string]any{"command": "cat /tmp/x | grep bar ~/.claude/projects/proj"}),
	})
	s2 := newTestStore(t, projectDir)
	if err := s2.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}
	g := findPattern(t, mustList(t, s2), PatternTranscriptGrep)
	if g.Occurrences != 3 || g.SessionCount != 2 {
		t.Errorf("grep pattern = %d occurrences / %d sessions, want 3 / 2",
			g.Occurrences, g.SessionCount)
	}
}

// TestIsSearchCommand covers the shapes that decide transcript_grep
// membership, including the two that a naive substring check gets wrong.
func TestIsSearchCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"grep -r foo ~/.claude/projects", true},
		{"rg foo", true},
		{"/usr/bin/grep foo", true},
		{"LC_ALL=C grep foo", true},
		{"cat x | rg foo", true},
		{"ls && ack foo", true},
		{"cat ~/.claude/projects/x/one.jsonl", false},
		{"wc -l one.jsonl", false},
		{"head -5 two.jsonl", false},
		// A flag or filename containing "rg"/"grep" is not an invocation.
		{"cat merge.jsonl", false},
		{"./configure --arg rgb", false},
		{"", false},
	} {
		if got := isSearchCommand(tc.cmd); got != tc.want {
			t.Errorf("isSearchCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestPatternSessionIdentityIsNotTruncated guards the bug that keying
// session identity on an 8-char prefix collapsed sess-pat-a and
// sess-pat-b into one session, under-counting exactly the corroboration
// the emission gate turns on.
func TestPatternSessionIdentityIsNotTruncated(t *testing.T) {
	projectDir := t.TempDir()
	const cwd = "/Users/t/work/github.com/acme/alpha"
	// Two ids sharing their first 8 characters.
	writeJSONL(t, projectDir, "projA", "collide-one", []map[string]any{
		patternToolUse("collide-one", "c1", cwd, recentTS(40), "Bash",
			map[string]any{"command": "cat ~/.claude/projects/x/a.jsonl"}),
		patternToolUse("collide-one", "c2", cwd, recentTS(39), "Bash",
			map[string]any{"command": "cat ~/.claude/projects/x/b.jsonl"}),
	})
	writeJSONL(t, projectDir, "projB", "collide-two", []map[string]any{
		patternToolUse("collide-two", "c3", cwd, recentTS(38), "Bash",
			map[string]any{"command": "cat ~/.claude/projects/x/c.jsonl"}),
	})
	s := newTestStore(t, projectDir)
	if err := s.IngestAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshPatterns(time.Now()); err != nil {
		t.Fatal(err)
	}
	p := findPattern(t, mustList(t, s), PatternDirectJSONLRead)
	if p.SessionCount != 2 {
		t.Errorf("SessionCount = %d for two ids sharing an 8-char prefix, want 2", p.SessionCount)
	}
	if !p.Emittable() {
		t.Error("prefix collision suppressed a pattern that clears the gate")
	}
}

// TestPatternNormalisation pins the two canonicalisers that define
// grouped-pattern identity. A change here silently re-partitions every
// existing row, since the id is derived from the signature.
func TestPatternNormalisation(t *testing.T) {
	sqlA := "SELECT * FROM messages WHERE session_id = 'abc' LIMIT 10"
	sqlB := "select  *  from messages where session_id = 'zzz' limit 250"
	if got, want := discoverNormalizeSQL(sqlA), discoverNormalizeSQL(sqlB); got != want {
		t.Errorf("literals and case should not split a shape:\n %q\n %q", got, want)
	}
	if discoverNormalizeSQL(sqlA) == discoverNormalizeSQL("SELECT * FROM entries LIMIT 1") {
		t.Error("different table shapes collapsed into one signature")
	}

	if got, want := discoverNormalizeSearch("Flaky Windows Test"), discoverNormalizeSearch("test flaky windows"); got != want {
		t.Errorf("word order/case should not split a search pattern: %q vs %q", got, want)
	}
	if want := "flaky test windows"; discoverNormalizeSearch("windows flaky test") != want {
		t.Errorf("normalised search = %q, want %q", discoverNormalizeSearch("windows flaky test"), want)
	}
}
