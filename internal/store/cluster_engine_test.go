// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func corpusDoc(id, kind, repo, text string, weight float64) ClusterCorpusDoc {
	return ClusterCorpusDoc{
		DocID: kind + ":" + id, Kind: kind, EntityID: id,
		Repo: repo, Text: text, Weight: weight,
	}
}

// TestClusterDocsSeparatesGroups: two topically distinct groups form two
// components; unrelated vocab does not merge them.
func TestClusterDocsSeparatesGroups(t *testing.T) {
	docs := []ClusterCorpusDoc{
		corpusDoc("1", "decision", "mnemo", "database schema migration sqlite column additive", 1.0),
		corpusDoc("2", "decision", "mnemo", "schema migration sqlite additive append only column", 1.0),
		corpusDoc("3", "compaction", "mnemo", "react frontend component rendering hooks state props", 0.8),
		corpusDoc("4", "compaction", "mnemo", "frontend react rendering component props hooks memo", 0.8),
	}
	clusters := clusterDocs(docs, HeuristicThreshold)
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters, got %d: %+v", len(clusters), clusters)
	}
	// Each cluster has exactly 2 members, and members share a topic.
	for _, c := range clusters {
		if len(c.members) != 2 {
			t.Errorf("cluster has %d members, want 2: %+v", len(c.members), c)
		}
	}
}

// TestClusterDocsSingleton: an unrelated document stays its own theme.
func TestClusterDocsSingleton(t *testing.T) {
	docs := []ClusterCorpusDoc{
		corpusDoc("1", "decision", "r", "database schema migration sqlite", 1.0),
		corpusDoc("2", "decision", "r", "database schema migration sqlite", 1.0),
		corpusDoc("3", "pattern", "r", "kubernetes ingress load balancer networking", 1.2),
	}
	clusters := clusterDocs(docs, HeuristicThreshold)
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters (a pair + a singleton), got %d", len(clusters))
	}
	var sizes []int
	for _, c := range clusters {
		sizes = append(sizes, len(c.members))
	}
	sort.Ints(sizes)
	if sizes[0] != 1 || sizes[1] != 2 {
		t.Errorf("want sizes [1 2], got %v", sizes)
	}
}

func TestClusterClusterFields(t *testing.T) {
	docs := []ClusterCorpusDoc{
		corpusDoc("1", "decision", "mnemo", "schema migration schema migration sqlite", 1.0),
		corpusDoc("2", "decision", "bullseye", "schema migration schema migration column", 1.0),
	}
	clusters := clusterDocs(docs, HeuristicThreshold)
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(clusters))
	}
	c := clusters[0]
	if c.weight != 2.0 {
		t.Errorf("weight = %v, want 2.0 (sum of member weights)", c.weight)
	}
	if strings.Join(c.repos, ",") != "bullseye,mnemo" {
		t.Errorf("repos = %v, want [bullseye mnemo]", c.repos)
	}
	if c.slug == "" || c.label == "theme" {
		t.Errorf("expected a real bigram label/slug, got label=%q slug=%q", c.label, c.slug)
	}
}

func TestRecomputeThemesWritesAndIsStable(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedClusterCorpus(t, s)

	run, err := s.RecomputeThemes("", "manual", DefaultClusterParams())
	if err != nil {
		t.Fatal(err)
	}
	if run.InputDocs == 0 || run.OutputThemes == 0 {
		t.Fatalf("empty run: %+v", run)
	}

	themeCount := countScalar(t, s, "SELECT COUNT(*) FROM themes")
	memberCount := countScalar(t, s, "SELECT COUNT(*) FROM theme_members")
	runCount := countScalar(t, s, "SELECT COUNT(*) FROM cluster_runs")
	if themeCount == 0 || memberCount == 0 {
		t.Fatalf("no themes/members written: themes=%d members=%d", themeCount, memberCount)
	}
	if runCount != 1 {
		t.Fatalf("want 1 cluster_runs row, got %d", runCount)
	}

	// A second pass over the unchanged corpus is idempotent: same theme
	// count, same ids (ids derive from the sorted member set).
	idsBefore := themeIDs(t, s)
	if _, err := s.RecomputeThemes("", "manual", DefaultClusterParams()); err != nil {
		t.Fatal(err)
	}
	idsAfter := themeIDs(t, s)
	if strings.Join(idsBefore, ",") != strings.Join(idsAfter, ",") {
		t.Errorf("theme ids churned across identical passes:\n before %v\n after  %v", idsBefore, idsAfter)
	}
	if got := countScalar(t, s, "SELECT COUNT(*) FROM cluster_runs"); got != 2 {
		t.Errorf("want 2 cluster_runs rows after 2 passes, got %d", got)
	}
}

// TestRecomputeThemesLeavesSegmentThemes: the pass rewrites only the four
// corpus doc kinds; a dormant segment-clusterer theme survives.
func TestRecomputeThemesLeavesSegmentThemes(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seedClusterCorpus(t, s)

	if _, err := s.writeDB.Exec(
		`INSERT INTO themes (id, label, summary, weight, repos, depth, first_seen, last_touched, computed_at)
		 VALUES ('theme_segmentX', 'seg', 'seg', 1, '[]', 0, '', '', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(
		`INSERT INTO theme_members (theme_id, doc_kind, entity_id, membership_kind, similarity)
		 VALUES ('theme_segmentX', 'segment', 'seg1', 'primary', 1.0)`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecomputeThemes("", "manual", DefaultClusterParams()); err != nil {
		t.Fatal(err)
	}
	if got := countScalar(t, s,
		"SELECT COUNT(*) FROM themes WHERE id = 'theme_segmentX'"); got != 1 {
		t.Errorf("segment theme was clobbered by the heuristic pass")
	}
	if got := countScalar(t, s,
		"SELECT COUNT(*) FROM theme_members WHERE doc_kind = 'segment'"); got != 1 {
		t.Errorf("segment member was clobbered")
	}
}

// TestRecomputeThemesDerivesMemberTimestamps: a theme's last_touched
// reflects its newest member, not the pass time — otherwise retirement
// could never fire (every pass would refresh the stamp to now).
func TestRecomputeThemesDerivesMemberTimestamps(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo) VALUES ('sess', 'mnemo')`); err != nil {
		t.Fatal(err)
	}
	// Two clustering decisions dated well in the past.
	oldTS := "2026-01-01T00:00:00Z"
	longAgo := time.Now().UTC().Add(-200 * 24 * time.Hour).Format(time.RFC3339)
	txt := "schema migration must stay additive because a new sqlite column with a default keeps old binaries safe and never drops data or tightens a constraint on existing rows"
	for i, ts := range []string{longAgo, oldTS} {
		if _, err := s.writeDB.Exec(
			`INSERT INTO decisions (id, session_id, proposal_text, confirmation_text, repo, timestamp)
			 VALUES (?, 'sess', ?, 'confirmed that is the right approach for us', 'mnemo', ?)`,
			i+1, txt, ts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecomputeThemes("", "manual", DefaultClusterParams()); err != nil {
		t.Fatal(err)
	}
	var lastTouched string
	if err := s.readDB.QueryRow(
		`SELECT last_touched FROM themes ORDER BY weight DESC LIMIT 1`).Scan(&lastTouched); err != nil {
		t.Fatal(err)
	}
	// last_touched must be one of the member timestamps, not "now".
	got := parseThemeTSForTest(t, lastTouched)
	if time.Since(got) < 100*24*time.Hour {
		t.Errorf("last_touched %q looks like pass time, not the newest member", lastTouched)
	}
}

func parseThemeTSForTest(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("last_touched not RFC3339: %q", ts)
	}
	return parsed
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Schema Migration": "schema-migration",
		"  React  Hooks ":  "react-hooks",
		"C++/CLI!!":        "c-cli",
		"":                 "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- helpers -----------------------------------------------------------

func seedClusterCorpus(t *testing.T, s *Store) {
	t.Helper()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.writeDB.Exec(
		`INSERT INTO session_meta (session_id, repo) VALUES ('sess', 'mnemo')`); err != nil {
		t.Fatal(err)
	}
	decisions := []string{
		"we should do the schema migration additively because sqlite append only column keeps old binaries safe",
		"schema migration must stay additive: new sqlite column with default, never drop, append only contract",
	}
	for i, txt := range decisions {
		if _, err := s.writeDB.Exec(
			`INSERT INTO decisions (id, session_id, proposal_text, confirmation_text, repo, timestamp)
			 VALUES (?, 'sess', ?, 'confirmed that is the right approach for us to take', 'mnemo', ?)`,
			i+1, txt, old); err != nil {
			t.Fatal(err)
		}
	}
	comps := []string{
		"reworked the react frontend rendering: component hooks state props memoisation to cut re-renders",
		"frontend react component rendering pass: hooks props state and memo to avoid wasteful re-render",
	}
	for i, txt := range comps {
		if _, err := s.writeDB.Exec(
			`INSERT INTO compactions (id, session_id, summary, payload_json, generated_at)
			 VALUES (?, 'sess', ?, '{"targets_active":["T1"]}', '2026-07-01T00:00:00Z')`,
			i+1, txt); err != nil {
			t.Fatal(err)
		}
	}
}

func countScalar(t *testing.T, s *Store, q string) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func themeIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.readDB.Query("SELECT id FROM themes ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}
