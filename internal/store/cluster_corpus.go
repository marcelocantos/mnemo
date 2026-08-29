// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// Stream weights for the clustering corpus (docs/design/vault-clustering.md).
const (
	DecisionStreamWeight   = 1.0
	CompactionStreamWeight = 0.8
	// PatternStreamWeight is defined in patterns.go (1.2).
	VaultUserStreamWeight = 1.5

	// VaultUserCorpusMinTokens is the admission floor for vault_user
	// documents (belong to a cluster). Label-anchor eligibility uses
	// vault_clustering.label.user_min_tokens (default 200).
	VaultUserCorpusMinTokens = 100
)

// ClusterCorpus assembles all four clustering streams into one corpus
// for a pass (🎯T64.8). Patterns already exist (🎯T64.7); the other three
// land here with the engine.
func (s *Store) ClusterCorpus() ([]ClusterCorpusDoc, error) {
	var out []ClusterCorpusDoc

	decisions, err := s.decisionCorpusDocs()
	if err != nil {
		return nil, fmt.Errorf("decision corpus: %w", err)
	}
	out = append(out, decisions...)

	compactions, err := s.compactionCorpusDocs()
	if err != nil {
		return nil, fmt.Errorf("compaction corpus: %w", err)
	}
	out = append(out, compactions...)

	patterns, err := s.PatternCorpusDocs()
	if err != nil {
		return nil, fmt.Errorf("pattern corpus: %w", err)
	}
	out = append(out, patterns...)

	vault, err := s.vaultUserCorpusDocs()
	if err != nil {
		return nil, fmt.Errorf("vault_user corpus: %w", err)
	}
	out = append(out, vault...)

	return out, nil
}

// decisionCorpusDocs: confirmed decisions with non-trivial rationale.
// Weight 1.0. Prefer a stored summary shape if payload grows one later;
// today we use proposal+confirmation trimmed to ~500 tokens.
func (s *Store) decisionCorpusDocs() ([]ClusterCorpusDoc, error) {
	rows, err := s.readDB.Query(`
		SELECT id, COALESCE(repo,''), COALESCE(timestamp,''),
		       proposal_text, confirmation_text
		FROM decisions
		WHERE length(trim(confirmation_text)) > 0
		  AND length(trim(proposal_text)) + length(trim(confirmation_text)) >= 80
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClusterCorpusDoc
	for rows.Next() {
		var id int64
		var repo, ts, proposal, confirmation string
		if err := rows.Scan(&id, &repo, &ts, &proposal, &confirmation); err != nil {
			return nil, err
		}
		text := trimToTokens(strings.TrimSpace(proposal+"\n\n"+confirmation), 500)
		if text == "" {
			continue
		}
		eid := fmt.Sprintf("%d", id)
		out = append(out, ClusterCorpusDoc{
			DocID:    "decision:" + eid,
			Kind:     "decision",
			EntityID: eid,
			Repo:     repo,
			Text:     text,
			TS:       ts,
			Weight:   DecisionStreamWeight,
		})
	}
	return out, rows.Err()
}

// compactionCorpusDocs: spans with targets_active or targets_progressed
// in payload_json. Weight 0.8.
func (s *Store) compactionCorpusDocs() ([]ClusterCorpusDoc, error) {
	rows, err := s.readDB.Query(`
		SELECT id, session_id, COALESCE(generated_at,''), COALESCE(summary,''), COALESCE(payload_json,'{}')
		FROM compactions
		WHERE length(trim(summary)) > 0
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClusterCorpusDoc
	for rows.Next() {
		var id int64
		var sessionID, ts, summary, payload string
		if err := rows.Scan(&id, &sessionID, &ts, &summary, &payload); err != nil {
			return nil, err
		}
		if !compactionHasTargetSignal(payload) {
			continue
		}
		text := trimToTokens(strings.TrimSpace(summary), 500)
		if text == "" {
			continue
		}
		// Compactions have no direct repo column; leave empty — members
		// still contribute text signal. Repo enrichment can come later
		// from session_meta if needed.
		eid := fmt.Sprintf("%d", id)
		out = append(out, ClusterCorpusDoc{
			DocID:    "compaction:" + eid,
			Kind:     "compaction",
			EntityID: eid,
			Repo:     "",
			Text:     text,
			TS:       ts,
			Weight:   CompactionStreamWeight,
		})
	}
	return out, rows.Err()
}

func compactionHasTargetSignal(payloadJSON string) bool {
	var p struct {
		TargetsActive     []string          `json:"targets_active"`
		TargetsProgressed map[string]string `json:"targets_progressed"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return false
	}
	if len(p.TargetsActive) > 0 {
		return true
	}
	return len(p.TargetsProgressed) > 0
}

// vaultUserCorpusDocs: docs.kind='vault' outside _mnemo/, ≥100 tokens.
// Weight 1.5. Only present when vault annotations have been ingested
// under full/includes scope.
func (s *Store) vaultUserCorpusDocs() ([]ClusterCorpusDoc, error) {
	rows, err := s.readDB.Query(`
		SELECT id, COALESCE(repo,''), file_path, COALESCE(title,''), mnemo_text(content, content_z), COALESCE(mtime,''), COALESCE(indexed_at,'')
		FROM docs
		WHERE kind = 'vault'
		ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClusterCorpusDoc
	for rows.Next() {
		var id int64
		var repo, path, title, content, mtime, indexedAt string
		if err := rows.Scan(&id, &repo, &path, &title, &content, &mtime, &indexedAt); err != nil {
			return nil, err
		}
		if isUnderMnemoWing(path) {
			continue
		}
		body := stripFrontmatterAndFences(content)
		if countTokensApprox(body) < VaultUserCorpusMinTokens {
			continue
		}
		text := strings.TrimSpace(title + "\n\n" + body)
		text = trimToTokens(text, 800)
		ts := mtime
		if ts == "" {
			ts = indexedAt
		}
		eid := fmt.Sprintf("%d", id)
		out = append(out, ClusterCorpusDoc{
			DocID:    "vault_user:" + eid,
			Kind:     "vault_user",
			EntityID: eid,
			Repo:     repo,
			Text:     text,
			TS:       ts,
			Weight:   VaultUserStreamWeight,
		})
	}
	return out, rows.Err()
}

func isUnderMnemoWing(path string) bool {
	// Paths are absolute or vault-relative; normalise separators.
	p := filepath.ToSlash(path)
	return strings.Contains(p, "/_mnemo/") || strings.HasPrefix(p, "_mnemo/")
}

// stripFrontmatterAndFences removes YAML frontmatter and fenced code
// blocks so token counts reflect prose.
func stripFrontmatterAndFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "---") {
		if i := strings.Index(s[3:], "\n---"); i >= 0 {
			rest := s[3+i+4:]
			if strings.HasPrefix(rest, "\n") {
				rest = rest[1:]
			}
			s = rest
		}
	}
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// countTokensApprox counts whitespace-separated tokens after lowercasing.
func countTokensApprox(s string) int {
	n := 0
	in := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			in = false
			continue
		}
		if !in {
			n++
			in = true
		}
	}
	return n
}

// trimToTokens keeps roughly the first max tokens of s.
func trimToTokens(s string, max int) string {
	if max <= 0 {
		return s
	}
	fields := strings.Fields(s)
	if len(fields) <= max {
		return s
	}
	return strings.Join(fields[:max], " ")
}
