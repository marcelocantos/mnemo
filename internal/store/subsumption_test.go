package store

import (
	"context"
	"testing"
	"time"
)

// TestSubsumptionCoversRemovedTools verifies, shape by shape, that every
// corpus whose dedicated tool was removed in 🎯T143 or retired in 🎯T144
// is reachable through unified search. This is the criterion that
// distinguishes subsumption from deletion.
func TestSubsumptionCoversRemovedTools(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	seed := map[string]string{
		"doc":      `INSERT INTO docs (repo, file_path, kind, title, content, content_hash, size, mtime, indexed_at) VALUES ('o/r','a.md','md','T','quokka in a doc','h',1,'2026-01-01','2026-01-01')`,
		"target":   `INSERT INTO targets (repo, file_path, target_id, name, status, weight, description, raw_text) VALUES ('o/r','t.md','T1','quokka target','identified',1,'quokka in a target','r')`,
		"memory":   `INSERT INTO memories (project, file_path, name, description, memory_type, content, updated_at) VALUES ('p','m.md','n','d','user','quokka in a memory','2026-01-01')`,
		"commit":   `INSERT INTO git_commits (repo, commit_hash, author_name, author_email, commit_date, subject, body) VALUES ('o/r','h1','a','a@b','2026-01-01','quokka in a commit','')`,
		"pr":       `INSERT INTO github_prs (repo, pr_number, title, body, state, author, created_at, updated_at, url) VALUES ('o/r',1,'quokka in a pr','b','open','a','2026-01-01','2026-01-01','u')`,
		"plan":     `INSERT INTO plans (repo, file_path, phase, content, updated_at) VALUES ('o/r','p.md','ph','quokka in a plan','2026-01-01')`,
		"config":   `INSERT INTO claude_configs (repo, file_path, content, updated_at) VALUES ('o/r','CLAUDE.md','quokka in a config','2026-01-01')`,
		"skill":    `INSERT INTO skills (file_path, name, description, content, updated_at) VALUES ('s.md','n','d','quokka in a skill','2026-01-01')`,
		"audit":    `INSERT INTO audit_entries (repo, file_path, date, skill, version, summary, raw_text) VALUES ('o/r','a.md','2026-01-01','release','v1','quokka in an audit','r')`,
		"decision": `INSERT INTO decisions (session_id, proposal_msg_id, confirmation_msg_id, proposal_text, confirmation_text, repo, timestamp) VALUES ('s',1,2,'quokka in a decision','ok','o/r','2026-01-01')`,
		"segment":  `INSERT INTO topic_segments (session_id, from_msg_id, to_msg_id, level, method, confidence, sealed, label, summary, repo) VALUES ('s',1,9,0,'llm',0.9,1,'quokka span','quokka in a segment','o/r')`,
	}
	for kind, q := range seed {
		if _, err := s.writeDB.Exec(q); err != nil {
			t.Fatalf("seed %s: %v", kind, err)
		}
	}
	for kind := range seed {
		res, err := s.UnifiedSearchOpts("quokka", UnifiedOpts{
			Kinds: []string{kind}, Limit: 10, SessionType: "all", SubstantiveOnly: true,
		}, time.Now())
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		if len(res.Hits) == 0 {
			t.Errorf("corpus %q returned nothing; its removed tool's capability is LOST, "+
				"not subsumed", kind)
			continue
		}
		if res.Hits[0].Kind != kind {
			t.Errorf("corpus %q returned a %q hit", kind, res.Hits[0].Kind)
		}
	}
	_ = context.Background()
}
