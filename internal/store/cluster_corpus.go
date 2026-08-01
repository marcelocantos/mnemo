// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Stream weights for the clustering corpus
// (docs/design/vault-clustering.md § Inputs). The pattern stream's
// weight (1.2) lives beside PatternCorpusDocs in patterns.go; the
// other three are defined here alongside the streams that use them.
//
// Weights encode relative trust in the source: user-authored notes are
// the strongest signal of human-meaningful structure, decisions sit
// closest to source material, and compaction summaries are LLM-distilled
// and slightly less reliable.
const (
	// DecisionStreamWeight is the baseline reference (1.0).
	DecisionStreamWeight = 1.0
	// CompactionStreamWeight is below baseline: summaries are
	// LLM-distilled, one step removed from source material.
	CompactionStreamWeight = 0.8
	// VaultUserStreamWeight is the highest: user-authored content
	// anchors and labels clusters rather than competing with
	// auto-extracted text.
	VaultUserStreamWeight = 1.5
)

// Corpus admission thresholds (docs/design/vault-clustering.md § Inputs).
const (
	// decisionSummaryTokenCap trims the proposal+confirmation fallback
	// text to a representative window. The decisions table has no
	// summary column, so this fallback is the only path.
	decisionSummaryTokenCap = 500

	// vaultUserMinTokens is the *corpus-admission* gate for user notes
	// (≥ 100 tokens of body, excluding frontmatter). Distinct from the
	// label-anchor gate (≥ 200 tokens, § Cluster labelling) which lands
	// with the engine in a later phase: a shorter note carries enough
	// signal to *belong* to a cluster but not to *name* it.
	vaultUserMinTokens = 100
)

// CompactionCorpusDocs returns the `compaction` stream of the
// clustering corpus (docs/design/vault-clustering.md § Inputs 2):
// compaction spans where actual work was tracked, each weighted
// CompactionStreamWeight.
//
// Filtering: spans with a non-empty targets_active or targets_progressed
// in their payload — i.e. spans where a bullseye target was moved on.
// Empty spans (a single message followed by /clear) contribute noise
// without signal and are excluded.
//
// The design names a `prose_summary` column; physically the prose lives
// in the compactions.summary column (and, as a fallback, the payload's
// "summary" field). Repo is resolved from session_meta.
func (s *Store) CompactionCorpusDocs() ([]ClusterCorpusDoc, error) {
	rows, err := s.readDB.Query(`
		SELECT c.id, c.session_id, COALESCE(c.summary, ''),
		       COALESCE(c.payload_json, '{}'), COALESCE(c.generated_at, ''),
		       COALESCE(m.repo, '')
		FROM compactions c
		LEFT JOIN session_meta m ON m.session_id = c.session_id
		ORDER BY c.id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClusterCorpusDoc
	for rows.Next() {
		var (
			id                     int64
			sessionID, summary     string
			payloadJSON, generated string
			repo                   string
		)
		if err := rows.Scan(&id, &sessionID, &summary, &payloadJSON, &generated, &repo); err != nil {
			return nil, err
		}

		var payload struct {
			TargetsActive     []string          `json:"targets_active"`
			TargetsProgressed map[string]string `json:"targets_progressed"`
			Summary           string            `json:"summary"`
		}
		// A malformed payload cannot prove work was tracked, so it is
		// excluded rather than admitted on faith.
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			continue
		}
		if len(payload.TargetsActive) == 0 && len(payload.TargetsProgressed) == 0 {
			continue
		}

		text := strings.TrimSpace(summary)
		if text == "" {
			text = strings.TrimSpace(payload.Summary)
		}
		if text == "" {
			continue
		}

		out = append(out, ClusterCorpusDoc{
			DocID:    "compaction:" + strconv.FormatInt(id, 10),
			Kind:     "compaction",
			EntityID: strconv.FormatInt(id, 10),
			Repo:     repo,
			Text:     text,
			TS:       generated,
			Weight:   CompactionStreamWeight,
		})
	}
	return out, rows.Err()
}

// VaultUserCorpusDocs returns the `vault_user` stream of the clustering
// corpus (docs/design/vault-clustering.md § Inputs 4): user-authored
// vault notes with at least vaultUserMinTokens of body content
// (excluding frontmatter), each weighted VaultUserStreamWeight.
//
// vaultRoot is the resolved vault path. Notes under <vaultRoot>/_mnemo/
// are excluded: they are either mnemo-generated pages or below-fence
// annotations attributed to their parent entity, not standalone
// user documents. An empty vaultRoot returns no docs — the caller
// (the clustering engine) only invokes this stream when the indexing
// scope is "full" or "includes", so a "_mnemo_only" scope never reaches
// here.
func (s *Store) VaultUserCorpusDocs(vaultRoot string) ([]ClusterCorpusDoc, error) {
	if strings.TrimSpace(vaultRoot) == "" {
		return nil, nil
	}
	mnemoPrefix := strings.TrimRight(vaultRoot, "/") + "/_mnemo/"

	rows, err := s.readDB.Query(`
		SELECT file_path, COALESCE(repo, ''), content,
		       COALESCE(NULLIF(doc_date, ''), mtime)
		FROM docs
		WHERE kind = 'vault'
		ORDER BY file_path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClusterCorpusDoc
	for rows.Next() {
		var filePath, repo, content, ts string
		if err := rows.Scan(&filePath, &repo, &content, &ts); err != nil {
			return nil, err
		}
		if strings.HasPrefix(filePath, mnemoPrefix) {
			continue
		}
		body := stripLeadingFrontmatter(content)
		if len(strings.Fields(body)) < vaultUserMinTokens {
			continue
		}
		out = append(out, ClusterCorpusDoc{
			DocID:    "vault_user:" + filePath,
			Kind:     "vault_user",
			EntityID: filePath,
			Repo:     repo,
			Text:     strings.TrimSpace(body),
			TS:       ts,
			Weight:   VaultUserStreamWeight,
		})
	}
	return out, rows.Err()
}

// ClusterCorpus merges all four streams into a single corpus for a
// clustering pass (docs/design/vault-clustering.md § Stream merge).
// vaultRoot is passed through to VaultUserCorpusDocs; an empty value
// omits the vault_user stream (scope "_mnemo_only" or no vault).
//
// The order is stable (decision, compaction, pattern, vault_user) so a
// pass over an unchanged corpus feeds the engine identical input.
func (s *Store) ClusterCorpus(vaultRoot string) ([]ClusterCorpusDoc, error) {
	decisions, err := s.DecisionCorpusDocs()
	if err != nil {
		return nil, err
	}
	compactions, err := s.CompactionCorpusDocs()
	if err != nil {
		return nil, err
	}
	patterns, err := s.PatternCorpusDocs()
	if err != nil {
		return nil, err
	}
	vaultUser, err := s.VaultUserCorpusDocs(vaultRoot)
	if err != nil {
		return nil, err
	}

	out := make([]ClusterCorpusDoc, 0,
		len(decisions)+len(compactions)+len(patterns)+len(vaultUser))
	out = append(out, decisions...)
	out = append(out, compactions...)
	out = append(out, patterns...)
	out = append(out, vaultUser...)
	return out, nil
}

// stripLeadingFrontmatter removes a leading YAML frontmatter block
// (a "---" line, arbitrary content, a closing "---" line) so token
// counts and cluster text reflect body content only. Content without a
// leading fence is returned unchanged.
func stripLeadingFrontmatter(content string) string {
	trimmed := strings.TrimLeft(strings.TrimPrefix(content, "\uFEFF"), " \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return content
	}
	// Require the "---" to be its own line.
	rest := trimmed[len("---"):]
	if rest != "" && rest[0] != '\n' && rest[0] != '\r' {
		return content
	}
	if idx := strings.Index(rest, "\n---"); idx >= 0 {
		after := rest[idx+len("\n---"):]
		// Consume to end of the closing fence line.
		if nl := strings.IndexByte(after, '\n'); nl >= 0 {
			return after[nl+1:]
		}
		return ""
	}
	return content
}

// firstNWords returns the first n whitespace-delimited tokens of s,
// rejoined with single spaces. Used to trim fallback text to a
// representative window without a tokenizer dependency.
func firstNWords(s string, n int) string {
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}
