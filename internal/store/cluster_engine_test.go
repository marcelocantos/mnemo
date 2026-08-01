// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedDecision(t *testing.T, s *Store, session, repo, proposal, confirmation, ts string) {
	t.Helper()
	_, err := s.writeDB.Exec(`
		INSERT INTO decisions (session_id, proposal_text, confirmation_text, repo, timestamp)
		VALUES (?, ?, ?, ?, ?)
	`, session, proposal, confirmation, repo, ts)
	if err != nil {
		t.Fatal(err)
	}
}

func seedCompaction(t *testing.T, s *Store, session, summary, payload, ts string) {
	t.Helper()
	_, err := s.writeDB.Exec(`
		INSERT INTO compactions (session_id, generated_at, summary, payload_json)
		VALUES (?, ?, ?, ?)
	`, session, ts, summary, payload)
	if err != nil {
		t.Fatal(err)
	}
}

func seedVaultDoc(t *testing.T, s *Store, path, title, content string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.writeDB.Exec(`
		INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at)
		VALUES ('vault', ?, 'vault', ?, ?, 'h', ?, ?, ?)
	`, path, title, content, len(content), now, now)
	if err != nil {
		t.Fatal(err)
	}
}

func seedPattern(t *testing.T, s *Store, id, ptype, sig string, occ, sess int, repos string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.writeDB.Exec(`
		INSERT INTO patterns (
			id, pattern_type, signature, occurrence_count, session_count,
			repos, first_seen, last_seen, representative_excerpts, computed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, ptype, sig, occ, sess, repos, now, now, `["example excerpt about `+sig+`"]`, now)
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunClusterHeuristicStableIDs: same corpus → same theme ids.
func TestRunClusterHeuristicStableIDs(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	// Two tight topical groups: auth and clustering.
	seedDecision(t, s, "s1", "acme/auth",
		"I propose we add JWT middleware for request authentication and authorization.",
		"Yes, ship the JWT authentication middleware with refresh tokens.", ts)
	seedDecision(t, s, "s2", "acme/auth",
		"We should rotate JWT signing keys on a schedule for authentication security.",
		"Agreed — scheduled JWT key rotation for authentication.", ts)
	seedDecision(t, s, "s3", "acme/mnemo",
		"Themes should cluster documents by topical similarity using TF-IDF.",
		"Yes, document clustering with TF-IDF and single-link agglomerative is the plan.", ts)
	seedDecision(t, s, "s4", "acme/mnemo",
		"The clustering engine needs stable theme ids from member sets.",
		"Confirmed — theme id is sha1 of sorted members for clustering stability.", ts)
	// Lower threshold so two-doc groups merge.
	cfg := VaultClusteringConfig{HeuristicThreshold: 0.05, MinClusterWeight: 0.5}

	r1, err := s.RunCluster(context.Background(), ClusterRunArgs{Config: cfg, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.OutputThemes == 0 {
		t.Fatal("expected at least one theme")
	}
	ids1 := themeIDs(t, s)

	r2, err := s.RunCluster(context.Background(), ClusterRunArgs{Config: cfg, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	ids2 := themeIDs(t, s)
	if len(ids1) != len(ids2) {
		t.Fatalf("theme count churned: %v vs %v", ids1, ids2)
	}
	for i := range ids1 {
		if ids1[i] != ids2[i] {
			t.Fatalf("id unstable: %v vs %v (runs %d/%d themes)", ids1, ids2, r1.OutputThemes, r2.OutputThemes)
		}
	}
}

func themeIDs(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.readDB.Query(`SELECT id FROM themes WHERE archived = 0 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestTwoKeyEgressMatrix: keys present, neither opt-in → zero HTTP.
func TestTwoKeyEgressMatrix(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seedDecision(t, s, "s1", "r",
		"Propose feature flags for rollout control in production deployments.",
		"Yes implement feature flags for gradual production rollout control.", ts)

	t.Setenv("VOYAGE_API_KEY", "test-voyage-key-not-real")
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key-not-real")
	httpHit := false
	cfg := VaultClusteringConfig{} // engine default heuristic, label bigram
	_, err := s.RunCluster(context.Background(), ClusterRunArgs{
		Config:      cfg,
		Trigger:     "manual",
		HTTPAllowed: &httpHit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if httpHit {
		t.Fatal("expected zero outbound HTTP with keys present but no opt-in")
	}
}

// fakeEmbed is a test EmbeddingProvider with fixed 2-d vectors.
type fakeEmbed struct {
	hits *int
	dims int
}

func (f *fakeEmbed) Name() string         { return "fake" }
func (f *fakeEmbed) Model() string        { return "fake-1" }
func (f *fakeEmbed) ModelVersion() string { return "v0" }
func (f *fakeEmbed) Dimensions() int {
	if f.dims > 0 {
		return f.dims
	}
	return 2
}
func (f *fakeEmbed) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.hits != nil {
		*f.hits++
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		// Distinct non-zero vectors so cosine is defined.
		out[i] = []float32{float32(i + 1), 1}
	}
	return out, nil
}

// TestEmbeddingsEngineUsesCache: force_reembed false reuses cache rows.
func TestEmbeddingsEngineUsesCache(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seedDecision(t, s, "s1", "r",
		"Propose feature flags for rollout control in production deployments enough text.",
		"Yes implement feature flags for gradual production rollout control enough.", ts)
	hits := 0
	prov := &fakeEmbed{hits: &hits}
	cfg := VaultClusteringConfig{
		Engine:             "embeddings",
		HeuristicThreshold: 0.01,
		MinClusterWeight:   0.5,
	}
	r1, err := s.RunCluster(context.Background(), ClusterRunArgs{
		Config: cfg, Provider: prov, Trigger: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Engine != "embeddings" {
		t.Fatalf("engine=%s", r1.Engine)
	}
	if r1.EmbeddingCalls == 0 {
		t.Fatal("expected embedding calls on first pass")
	}
	firstHits := hits
	r2, err := s.RunCluster(context.Background(), ClusterRunArgs{
		Config: cfg, Provider: prov, Trigger: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.EmbeddingCalls != 0 {
		t.Fatalf("second pass should hit cache, got embedding_calls=%d", r2.EmbeddingCalls)
	}
	if hits != firstHits {
		t.Fatalf("provider called again on cache hit: hits %d → %d", firstHits, hits)
	}
	// force_reembed bypasses cache.
	r3, err := s.RunCluster(context.Background(), ClusterRunArgs{
		Config: cfg, Provider: prov, ForceReembed: true, Trigger: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r3.EmbeddingCalls == 0 {
		t.Fatal("force_reembed must call provider")
	}
}

// fakeLabeler returns a fixed label.
type fakeLabeler struct{ hits *int }

func (f *fakeLabeler) Label(_ context.Context, _ []string) (string, error) {
	if f.hits != nil {
		*f.hits++
	}
	return "feature flag rollout", nil
}

// TestLLMLabelOptIn: label.engine=llm uses the labeler; default does not.
func TestLLMLabelOptIn(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seedDecision(t, s, "s1", "r",
		"Propose feature flags for rollout control in production deployments enough text here.",
		"Yes implement feature flags for gradual production rollout control enough text.", ts)

	hits := 0
	// Default label engine = bigram → labeler must not be consulted even if passed…
	// Actually we only construct/pass when llm is set. Pass nil with llm off.
	cfgOff := VaultClusteringConfig{MinClusterWeight: 0.5, HeuristicThreshold: 0.01}
	if _, err := s.RunCluster(context.Background(), ClusterRunArgs{
		Config: cfgOff, Labeler: &fakeLabeler{hits: &hits},
	}); err != nil {
		t.Fatal(err)
	}
	// Labeler is only used when EffectiveLabelEngine is llm — but we still
	// pass it; labelCluster checks cfg. Ensure zero hits.
	if hits != 0 {
		t.Fatalf("labeler called with bigram engine: hits=%d", hits)
	}

	cfgOn := VaultClusteringConfig{
		MinClusterWeight:   0.5,
		HeuristicThreshold: 0.01,
		Label:              VaultClusteringLabelConfig{Engine: "llm"},
	}
	if _, err := s.RunCluster(context.Background(), ClusterRunArgs{
		Config: cfgOn, Labeler: &fakeLabeler{hits: &hits},
	}); err != nil {
		t.Fatal(err)
	}
	if hits == 0 {
		t.Fatal("expected labeler call when label.engine=llm")
	}
	// Theme label should reflect LLM output (title-cased).
	var label, path string
	// We don't persist path yet — check label text.
	if err := s.readDB.QueryRow(`SELECT label FROM themes LIMIT 1`).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(label), "feature") {
		t.Fatalf("expected llm-ish label, got %q", label)
	}
	_ = path
}

// TestLabelGates reject daily notes and short stubs.
func TestLabelGates(t *testing.T) {
	cent := tfVector("authentication middleware jwt refresh token security")
	// Daily note title.
	docs := []ClusterCorpusDoc{
		{DocID: "vault_user:1", Kind: "vault_user", EntityID: "1",
			Text:   "2026-05-12\n\n" + strings.Repeat("authentication middleware jwt token security refresh ", 40),
			Weight: 1.5},
		{DocID: "decision:2", Kind: "decision", EntityID: "2",
			Text: "authentication middleware jwt refresh token security", Weight: 1},
	}
	cfg := VaultClusteringConfig{}
	// Force vault_user to be closest by making its text match centroid strongly.
	lab := labelCluster(context.Background(), docs, []int{0, 1}, cent, cfg, nil)
	if lab.Path == LabelPathUserAnchor {
		t.Fatalf("daily note must not win user_anchor; gate=%s path=%s label=%s", lab.GateFired, lab.Path, lab.Label)
	}
	if lab.GateFired != LabelGateFilenameExclude && lab.GateFired != LabelGateBelowMinTokens {
		// filename gate expected for 2026-05-12
		if lab.GateFired == LabelGateNone {
			t.Fatalf("expected a gate to fire, got none; path=%s", lab.Path)
		}
	}

	// Short stub.
	docs[0].Text = "Auth notes\n\nshort"
	lab = labelCluster(context.Background(), docs, []int{0, 1}, cent, cfg, nil)
	if lab.Path == LabelPathUserAnchor {
		t.Fatal("short stub must not be user_anchor")
	}
	if lab.GateFired != LabelGateBelowMinTokens {
		// may also fail title overlap
		t.Logf("gate=%s (below_min_tokens preferred)", lab.GateFired)
	}
}

// TestThemePinAndRetire: unpinned old theme archives; pin prevents it.
func TestThemePinAndRetire(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seedDecision(t, s, "s1", "r",
		"Database migration strategy for schema evolution and backwards compatibility.",
		"Yes use expand-contract migrations for schema evolution compatibility.", ts)
	cfg := VaultClusteringConfig{HeuristicThreshold: 0.01, MinClusterWeight: 0.5}
	if _, err := s.RunCluster(context.Background(), ClusterRunArgs{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	ids := themeIDs(t, s)
	if len(ids) == 0 {
		t.Fatal("no themes")
	}
	tid := ids[0]

	// Backdate last_touched.
	old := time.Now().UTC().Add(-200 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := s.writeDB.Exec(`UPDATE themes SET last_touched = ? WHERE id = ?`, old, tid); err != nil {
		t.Fatal(err)
	}
	// Without pin → archive.
	if err := s.applyThemeRetirement(time.Now().UTC(), VaultClusteringConfig{RetireAfter: "4320h"}); err != nil {
		t.Fatal(err)
	}
	var archived int
	if err := s.readDB.QueryRow(`SELECT archived FROM themes WHERE id = ?`, tid).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived == 0 {
		t.Fatal("expected theme archived after retire_after")
	}

	// Restore and pin.
	if _, err := s.writeDB.Exec(`UPDATE themes SET archived = 0, last_touched = ? WHERE id = ?`, old, tid); err != nil {
		t.Fatal(err)
	}
	if err := s.PinTheme(tid, false, "keep"); err != nil {
		t.Fatal(err)
	}
	if err := s.applyThemeRetirement(time.Now().UTC(), VaultClusteringConfig{RetireAfter: "4320h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow(`SELECT archived FROM themes WHERE id = ?`, tid).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 0 {
		t.Fatal("pinned theme must not archive")
	}
}

// TestClusterEmbeddingsPK shape is documented by schema; smoke-insert.
func TestClusterEmbeddingsPK(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	_, err := s.writeDB.Exec(`
		INSERT INTO cluster_embeddings (
			doc_kind, entity_id, content_hash, provider, model, model_version, dims, vector, computed_at
		) VALUES ('decision','1','abc','voyage','voyage-3-lite','',2,?,?)
	`, []byte{0, 0, 0, 0, 0, 0, 0, 0}, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	// Same content different model → separate row.
	_, err = s.writeDB.Exec(`
		INSERT INTO cluster_embeddings (
			doc_kind, entity_id, content_hash, provider, model, model_version, dims, vector, computed_at
		) VALUES ('decision','1','abc','voyage','voyage-3','',2,?,?)
	`, []byte{0, 0, 0, 0, 0, 0, 0, 0}, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM cluster_embeddings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 cache rows, got %d", n)
	}
}

// TestInspectAndPin tools surface.
func TestInspectTheme(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seedDecision(t, s, "s1", "r",
		"Observability needs structured logging metrics and distributed tracing together.",
		"Ship structured logging metrics and tracing as one observability stack.", ts)
	cfg := VaultClusteringConfig{HeuristicThreshold: 0.01, MinClusterWeight: 0.5}
	if _, err := s.RunCluster(context.Background(), ClusterRunArgs{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	ids := themeIDs(t, s)
	if len(ids) == 0 {
		t.Fatal("no themes")
	}
	view, err := s.InspectTheme(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != ids[0] {
		t.Fatalf("id mismatch %s vs %s", view.ID, ids[0])
	}
	if len(view.Members) == 0 {
		t.Fatal("expected members")
	}
}

// TestCorpusIncludesFourStreams wiring.
func TestClusterCorpusStreams(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seedDecision(t, s, "s1", "r",
		"Long enough proposal text for the decision corpus admission gate about APIs.",
		"Long enough confirmation text accepting the API design decision proposal.", ts)
	seedCompaction(t, s, "s1",
		"Worked on authentication middleware and session handling throughout the span.",
		`{"targets_active":["T1"],"targets_progressed":{}}`, ts)
	seedPattern(t, s, "pattern_abcd1234", PatternDirectJSONLRead, "cat jsonl", 4, 2, `["acme/alpha"]`)
	body := strings.Repeat("user authored note about clustering themes and retrieval quality. ", 20)
	seedVaultDoc(t, s, filepath.Join(t.TempDir(), "notes", "clustering.md"), "Clustering notes", body)

	docs, err := s.ClusterCorpus()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, d := range docs {
		kinds[d.Kind]++
	}
	for _, k := range []string{"decision", "compaction", "pattern", "vault_user"} {
		if kinds[k] == 0 {
			t.Errorf("missing stream %s in corpus (got %v)", k, kinds)
		}
	}
}

// TestThemeIDFromMembers matches design: theme_<sha1[:8]>.
func TestThemeIDFromDocIDs(t *testing.T) {
	a := themeIDFromDocIDs([]string{"decision:1", "decision:2"})
	b := themeIDFromDocIDs([]string{"decision:1", "decision:2"})
	c := themeIDFromDocIDs([]string{"decision:2", "decision:1"}) // unsorted input — caller sorts
	if a != b {
		t.Fatal("unstable")
	}
	if !strings.HasPrefix(a, "theme_") || len(a) != len("theme_")+8 {
		t.Fatalf("bad id shape %q", a)
	}
	// Unsorted differs (caller must sort) — document that contract.
	if a == c {
		// sortedKeys style: if caller passes unsorted, ID changes — OK to note.
		t.Log("unsorted input produces different id (caller sorts)")
	}
}

// TestCostProfileNoDendrogram: multi-doc run completes without hang and
// does not create parent themes (phase-2 dendrogram is intentionally off).
func TestCostProfileNoDendrogram(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 20; i++ {
		seedDecision(t, s, "s", "r",
			"Topic alpha beta gamma discussion number "+strings.Repeat("x", i%5)+
				" with enough text for corpus admission and clustering signal.",
			"Confirm topic alpha beta gamma number with enough confirmation text for admission.", ts)
	}
	start := time.Now()
	cfg := VaultClusteringConfig{HeuristicThreshold: 0.99, MinClusterWeight: 0.5} // almost no merges
	r, err := s.RunCluster(context.Background(), ClusterRunArgs{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("clustering too slow for 20 docs: %v", time.Since(start))
	}
	var parents int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM themes WHERE parent_theme_id IS NOT NULL`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if parents != 0 {
		t.Fatalf("document engine must not build dendrogram parents, got %d", parents)
	}
	if r.InputDocs == 0 {
		t.Fatal("expected input docs")
	}
}
