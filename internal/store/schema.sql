-- mnemo schema — single source of truth, applied at startup via sqlift.
--
-- This file was generated from the live DB to match SQLite's canonical
-- stored form byte-for-byte. Any edit must preserve the exact whitespace
-- for CREATE TRIGGER / CREATE VIEW / CREATE VIRTUAL TABLE statements,
-- since sqlift's Trigger/View/VirtualTable equality is sensitive to the
-- stored sql/args text. Tables and indexes are formatted for readability
-- (their raw_sql is excluded from equality).
--
-- sqlift owns idempotency, so no IF NOT EXISTS / IF EXISTS clauses.
-- Any change here is diffed against the live DB and applied under
-- ApplyOptions{} (AllowNone) — only pure additive changes are allowed.
-- See CLAUDE.md § Schema policy for the deprecation pattern.

-- Tables

CREATE TABLE audit_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL,
			date TEXT NOT NULL DEFAULT '',
			skill TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			raw_text TEXT NOT NULL,
			UNIQUE(file_path, date, skill)
		);

CREATE TABLE ci_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			run_id INTEGER NOT NULL UNIQUE,
			workflow TEXT NOT NULL,
			branch TEXT,
			commit_sha TEXT,
			status TEXT NOT NULL,
			conclusion TEXT,
			started_at TEXT,
			completed_at TEXT,
			log_summary TEXT,
			url TEXT,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

CREATE TABLE claude_configs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL DEFAULT '',
			file_path TEXT NOT NULL UNIQUE,
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

CREATE TABLE claude_md_reviews (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			reviewed_at TEXT NOT NULL,
			-- commit_id: repo's git HEAD at review time. Stored for
			-- forensic correlation; NOT consumed by the staleness
			-- trigger logic (entry-count-since-review is the cheap
			-- signal). 🎯T41.
			commit_id TEXT NOT NULL DEFAULT '',
			-- summary: the CLAUDE.md first-paragraph as it stood at
			-- review time (the thing the LLM was asked to assess).
			summary TEXT NOT NULL,
			-- verdict: LLM's assessment. One of: "current",
			-- "stale", "rewritten" (where "rewritten" means the
			-- LLM proposed a CLAUDE.md rewrite, not just a summary
			-- update).
			verdict TEXT NOT NULL,
			-- proposed_summary: LLM's replacement summary text when
			-- verdict in (stale, rewritten). NULL otherwise.
			proposed_summary TEXT,
			-- proposed_claude_md: LLM's CLAUDE.md rewrite when
			-- verdict = rewritten. NULL otherwise. Surfaced for
			-- human review; never auto-applied.
			proposed_claude_md TEXT,
			UNIQUE(repo, reviewed_at)
		);

CREATE TABLE compactions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			connection_id TEXT,
			generated_at TEXT NOT NULL DEFAULT (datetime('now')),
			model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			entry_id_from INTEGER NOT NULL DEFAULT 0,
			entry_id_to INTEGER NOT NULL DEFAULT 0,
			payload_json TEXT NOT NULL DEFAULT '{}',
			summary TEXT NOT NULL DEFAULT ''
		);

-- 🎯T77 durable per-session compaction-failure quarantine. The
-- watcher's in-memory backoff was lost on every daemon restart, so a
-- permanently un-summarisable session (e.g. one that EINVALs at exec,
-- or whose content makes the summariser reply conversationally forever)
-- failed afresh every boot and dominated the failed:compacted ratio.
-- fail_count accrues across restarts; the candidate query excludes a
-- session whose count is at/over the threshold and whose last failure
-- is within the cooldown. A clean tick deletes the row.
CREATE TABLE compactor_quarantine (
			session_id TEXT PRIMARY KEY,
			fail_count INTEGER NOT NULL DEFAULT 0,
			last_failed_at TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT ''
		);

CREATE TABLE connection_sessions (
			connection_id TEXT NOT NULL,
			session_id   TEXT NOT NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			PRIMARY KEY (connection_id, session_id)
		);

CREATE TABLE daemon_connections (
			connection_id TEXT PRIMARY KEY,
			pid INTEGER NOT NULL,
			accepted_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			closed_at TEXT
		);

CREATE TABLE decisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			proposal_msg_id INTEGER REFERENCES messages(id),
			confirmation_msg_id INTEGER REFERENCES messages(id),
			proposal_text TEXT NOT NULL,
			confirmation_text TEXT NOT NULL,
			repo TEXT NOT NULL DEFAULT '',
			timestamp TEXT NOT NULL
		);

CREATE TABLE doc_tree_refs (
			doc_id INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
			tree_id INTEGER NOT NULL REFERENCES trees_of_interest(id) ON DELETE CASCADE,
			PRIMARY KEY (doc_id, tree_id)
		);

CREATE TABLE docs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL DEFAULT 'md',
			title TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			mtime TEXT NOT NULL DEFAULT '',
			indexed_at TEXT NOT NULL,
			taxonomy TEXT NOT NULL DEFAULT '',
			doc_date TEXT NOT NULL DEFAULT '',
			doc_status TEXT NOT NULL DEFAULT '',
			doc_target TEXT NOT NULL DEFAULT '',
			doc_source TEXT NOT NULL DEFAULT ''
		);

CREATE TABLE entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			type TEXT NOT NULL,
			timestamp TEXT,
			raw BLOB,
			-- Virtual columns for high-query entry-level fields.
			uuid TEXT GENERATED ALWAYS AS (COALESCE(raw->>'$.uuid', raw->>'$.messageId')),
			model TEXT GENERATED ALWAYS AS (raw->>'$.message.model'),
			stop_reason TEXT GENERATED ALWAYS AS (raw->>'$.message.stop_reason'),
			input_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.input_tokens')),
			output_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.output_tokens')),
			cache_read_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_read_input_tokens')),
			cache_creation_tokens INTEGER GENERATED ALWAYS AS (json_extract(raw, '$.message.usage.cache_creation_input_tokens')),
			agent_id TEXT GENERATED ALWAYS AS (raw->>'$.agentId'),
			version TEXT GENERATED ALWAYS AS (raw->>'$.version'),
			slug TEXT GENERATED ALWAYS AS (raw->>'$.slug'),
			is_sidechain INTEGER GENERATED ALWAYS AS (CASE WHEN json_extract(raw, '$.isSidechain') THEN 1 ELSE 0 END),
			data_type TEXT GENERATED ALWAYS AS (raw->>'$.data.type'),
			data_command TEXT GENERATED ALWAYS AS (raw->>'$.data.command'),
			data_hook_event TEXT GENERATED ALWAYS AS (raw->>'$.data.hookEvent'),
			top_tool_use_id TEXT GENERATED ALWAYS AS (raw->>'$.toolUseID'),
			parent_tool_use_id TEXT GENERATED ALWAYS AS (raw->>'$.parentToolUseID')
		);

CREATE TABLE git_commits (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			commit_hash TEXT NOT NULL,
			author_name TEXT NOT NULL,
			author_email TEXT NOT NULL,
			commit_date TEXT NOT NULL,
			subject TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			UNIQUE(repo, commit_hash)
		);

CREATE TABLE github_issues (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			author TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			url TEXT NOT NULL,
			labels TEXT NOT NULL DEFAULT '[]',
			UNIQUE(repo, issue_number)
		);

CREATE TABLE github_prs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL,
			author TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			merged_at TEXT,
			url TEXT NOT NULL,
			UNIQUE(repo, pr_number)
		);

CREATE TABLE image_descriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			UNIQUE(image_id)
		);

CREATE TABLE image_embeddings (
			image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			dim INTEGER NOT NULL,
			vector BLOB NOT NULL,
			error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

CREATE TABLE image_occurrences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			image_id INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
			entry_id INTEGER REFERENCES entries(id),
			message_id INTEGER REFERENCES messages(id),
			session_id TEXT NOT NULL,
			source_type TEXT NOT NULL CHECK(source_type IN ('inline','path')),
			occurred_at TEXT NOT NULL,
			UNIQUE(image_id, entry_id, message_id, source_type)
		);

CREATE TABLE image_ocr (
			image_id INTEGER PRIMARY KEY REFERENCES images(id) ON DELETE CASCADE,
			text TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL,
			confidence REAL,
			error TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

CREATE TABLE images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content_hash TEXT UNIQUE NOT NULL,
			bytes BLOB NOT NULL,
			original_path TEXT,
			mime_type TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			pixel_format TEXT NOT NULL,
			byte_size INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

CREATE TABLE ingest_state (
			path TEXT PRIMARY KEY,
			offset INTEGER NOT NULL,
			-- 🎯T68.6 fingerprint cursor: the file's size+mtime at the
			-- moment the offset was recorded. SourceDrift compares these
			-- against current stat to detect same-size in-place rewrites
			-- (offset>=size but mtime moved). Nullable so old rows fall
			-- back to size-only detection until next ingest re-stamps
			-- them.
			recorded_size INTEGER,
			recorded_mtime TEXT
		);

CREATE TABLE ingest_status (
			stream TEXT PRIMARY KEY,
			last_backfill TEXT NOT NULL,
			files_indexed INTEGER NOT NULL,
			files_on_disk INTEGER NOT NULL
		);

-- Per-repo reconcile cursor for the external mirror streams
-- (ci/github/commits) — 🎯T68.5. A (repo, stream) row records when
-- that repo's stream last reconciled, so the mirror reconciler can be
-- divergence-driven (reconcile a repo whose row is missing or stale)
-- rather than boot-once or fixed-poll. Additive, append-only.
CREATE TABLE mirror_status (
			repo TEXT NOT NULL,
			stream TEXT NOT NULL,
			last_reconciled_at TEXT NOT NULL,
			PRIMARY KEY (repo, stream)
		);

-- Forward output manifest for the vault exporter (🎯T68.6). Each note
-- the exporter writes UPSERTs a row keyed by note_path; orphan
-- detection is then exact set-difference (manifest rows whose file is
-- gone; *.md files under the vault root with no manifest row) instead
-- of lossy slug reverse-mapping. content_hash lets the GC verify the
-- on-disk note is still the artifact we wrote before removing it.
-- Vault-relative paths.
CREATE TABLE vault_outputs (
			note_path TEXT PRIMARY KEY,
			entity_kind TEXT NOT NULL,
			entity_id TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			written_at TEXT NOT NULL
		);
CREATE INDEX idx_vault_outputs_entity ON vault_outputs(entity_kind, entity_id);

CREATE TABLE memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project TEXT NOT NULL,
			file_path TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			memory_type TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_id INTEGER REFERENCES entries(id),
			session_id TEXT NOT NULL,
			project TEXT NOT NULL,
			role TEXT NOT NULL,
			text TEXT NOT NULL,
			timestamp TEXT,
			type TEXT,
			is_noise INTEGER NOT NULL DEFAULT 0,
			content_type TEXT NOT NULL DEFAULT 'text',
			tool_name TEXT,
			tool_use_id TEXT,
			tool_input BLOB,
			is_error INTEGER NOT NULL DEFAULT 0,
			-- Computed columns: commonly queried fields from tool_input.
			tool_file_path TEXT GENERATED ALWAYS AS (tool_input->>'file_path'),
			tool_command TEXT GENERATED ALWAYS AS (tool_input->>'command'),
			tool_pattern TEXT GENERATED ALWAYS AS (tool_input->>'pattern'),
			tool_description TEXT GENERATED ALWAYS AS (tool_input->>'description'),
			tool_skill TEXT GENERATED ALWAYS AS (tool_input->>'skill'),
			tool_old_string TEXT GENERATED ALWAYS AS (tool_input->>'old_string'),
			tool_new_string TEXT GENERATED ALWAYS AS (tool_input->>'new_string'),
			tool_content TEXT GENERATED ALWAYS AS (tool_input->>'content'),
			tool_query TEXT GENERATED ALWAYS AS (tool_input->>'query'),
			tool_url TEXT GENERATED ALWAYS AS (tool_input->>'url'),
			tool_name_param TEXT GENERATED ALWAYS AS (tool_input->>'name'),
			tool_prompt TEXT GENERATED ALWAYS AS (tool_input->>'prompt'),
			tool_subject TEXT GENERATED ALWAYS AS (tool_input->>'subject'),
			tool_status TEXT GENERATED ALWAYS AS (tool_input->>'status'),
			tool_task_id TEXT GENERATED ALWAYS AS (COALESCE(tool_input->>'task_id', tool_input->>'taskId'))
		);

CREATE TABLE plans (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL UNIQUE,
			phase TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

CREATE TABLE query_templates (
			id INTEGER PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			description TEXT,
			query_text TEXT NOT NULL,
			param_names TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

CREATE TABLE reconciled_costs (
			date TEXT NOT NULL PRIMARY KEY, -- UTC calendar date (YYYY-MM-DD)
			cost_usd REAL NOT NULL,          -- authoritative cost from Anthropic Admin API
			fetched_at TEXT NOT NULL         -- when this row was retrieved
		);

CREATE TABLE session_chains (
			successor_id TEXT PRIMARY KEY,
			predecessor_id TEXT NOT NULL,
			boundary TEXT NOT NULL DEFAULT 'clear',
			gap_ms INTEGER NOT NULL,
			confidence TEXT NOT NULL CHECK(confidence IN ('definitive', 'high', 'medium', 'low')),
			mechanism TEXT NOT NULL DEFAULT 'mcp_connection',
			detected_at TEXT NOT NULL DEFAULT (datetime('now'))
		);

CREATE TABLE session_meta (
			session_id TEXT PRIMARY KEY,
			repo TEXT NOT NULL DEFAULT '',
			cwd TEXT NOT NULL DEFAULT '',
			git_branch TEXT NOT NULL DEFAULT '',
			work_type TEXT NOT NULL DEFAULT '',
			topic TEXT NOT NULL DEFAULT '',
			-- 🎯T68.6 source-state convergence (Law 2): valid-time tag
			-- per session. The index retains content durably; this column
			-- tracks the *current* state of the session's source JSONL
			-- ("live" / "truncated_at=…" / "deleted_at=…"). The state
			-- reconciler — a drift sweep over SourceDrift — converges
			-- this tag toward reality without ever removing rows.
			-- Additive, defaulted: old sessions read as "live" until a
			-- drift event tags them.
			source_status TEXT NOT NULL DEFAULT 'live',
			source_state_at TEXT NOT NULL DEFAULT '',
			-- 🎯T72 recursion guard: set to 1 when this session's
			-- transcript is a claudia-spawned compactor run (detected at
			-- ingest by the CompactorMarker prefix on the first user
			-- message), 0 for genuine user/dev sessions. Replaces the
			-- over-broad excludeCWD prefix check that wrongly skipped real
			-- dev sessions sharing the mnemo repo cwd. Additive, defaulted:
			-- old rows read as 0 (eligible) until re-ingest tags a marker.
			compactor_internal INTEGER NOT NULL DEFAULT 0
		);

CREATE TABLE session_nonces (
			nonce TEXT PRIMARY KEY,
			session_id TEXT NOT NULL
		);

CREATE TABLE session_summary (
			session_id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			session_type TEXT NOT NULL DEFAULT 'interactive',
			total_msgs INTEGER NOT NULL DEFAULT 0,
			substantive_msgs INTEGER NOT NULL DEFAULT 0,
			first_msg TEXT NOT NULL DEFAULT '',
			last_msg TEXT NOT NULL DEFAULT ''
		);

CREATE TABLE skills (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

CREATE TABLE snapshot_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			entry_id INTEGER NOT NULL REFERENCES entries(id),
			session_id TEXT NOT NULL,
			file_path TEXT NOT NULL,
			backup_time TEXT
		);

CREATE TABLE targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			file_path TEXT NOT NULL,
			target_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			weight REAL NOT NULL DEFAULT 0,
			description TEXT NOT NULL DEFAULT '',
			raw_text TEXT NOT NULL,
			UNIQUE(file_path, target_id)
		);

CREATE TABLE trees_of_interest (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			root_path TEXT NOT NULL UNIQUE,
			label TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		);

-- Indexes

CREATE INDEX idx_audit_entries_date ON audit_entries(date);

CREATE INDEX idx_audit_entries_repo ON audit_entries(repo);

CREATE INDEX idx_audit_entries_skill ON audit_entries(skill);

CREATE INDEX idx_ci_runs_conclusion ON ci_runs(conclusion);

CREATE INDEX idx_ci_runs_repo ON ci_runs(repo);

CREATE INDEX idx_ci_runs_started ON ci_runs(started_at);

CREATE INDEX idx_ci_runs_status ON ci_runs(status);

CREATE INDEX idx_claude_configs_repo ON claude_configs(repo);

CREATE INDEX idx_claude_md_reviews_repo
			ON claude_md_reviews(repo, reviewed_at DESC);

CREATE INDEX idx_compactions_session_generated ON compactions(session_id, generated_at DESC);

CREATE INDEX idx_connection_sessions_connection_last ON connection_sessions(connection_id, last_seen_at DESC);

CREATE INDEX idx_connection_sessions_session ON connection_sessions(session_id);

CREATE INDEX idx_daemon_connections_open ON daemon_connections(closed_at) WHERE closed_at IS NULL;

CREATE INDEX idx_daemon_connections_pid ON daemon_connections(pid);

CREATE INDEX idx_decisions_repo ON decisions(repo);

CREATE INDEX idx_decisions_session ON decisions(session_id);

CREATE INDEX idx_decisions_timestamp ON decisions(timestamp);

CREATE INDEX idx_doc_tree_refs_tree ON doc_tree_refs(tree_id);

CREATE INDEX idx_docs_repo ON docs(repo);

CREATE INDEX idx_docs_taxonomy ON docs(taxonomy) WHERE taxonomy != '';

CREATE INDEX idx_entries_agent_id ON entries(agent_id) WHERE agent_id IS NOT NULL;

CREATE INDEX idx_entries_assistant_tokens ON entries(session_id, input_tokens, output_tokens) WHERE type = 'assistant';

-- 🎯T72 addenda-token metric: SUM(output_tokens + cache_creation_tokens)
-- over assistant entries past a cursor (entries.id > cursor_entry_id),
-- computed for every session on every compactor scan. Keyed
-- (session_id, id) with the two summed columns materialised as covering
-- payload so the per-session range sum is an index-only scan rather than
-- a json_extract over each row's raw BLOB.
CREATE INDEX idx_entries_addenda ON entries(session_id, id, output_tokens, cache_creation_tokens) WHERE type = 'assistant';

CREATE INDEX idx_entries_data_hook_event ON entries(data_hook_event) WHERE data_hook_event IS NOT NULL;

CREATE INDEX idx_entries_data_type ON entries(data_type) WHERE data_type IS NOT NULL;

CREATE INDEX idx_entries_model ON entries(model) WHERE model IS NOT NULL;

CREATE INDEX idx_entries_parent_tool_use_id ON entries(parent_tool_use_id) WHERE parent_tool_use_id IS NOT NULL;

CREATE INDEX idx_entries_project ON entries(project);

CREATE INDEX idx_entries_session ON entries(session_id);

CREATE UNIQUE INDEX idx_entries_session_uuid ON entries(session_id, uuid) WHERE uuid IS NOT NULL;

CREATE INDEX idx_entries_timestamp ON entries(timestamp);

CREATE INDEX idx_entries_top_tool_use_id ON entries(top_tool_use_id) WHERE top_tool_use_id IS NOT NULL;

CREATE INDEX idx_entries_type ON entries(type);

CREATE INDEX idx_git_commits_date ON git_commits(commit_date);

CREATE INDEX idx_git_commits_hash ON git_commits(commit_hash);

CREATE INDEX idx_git_commits_repo ON git_commits(repo);

CREATE INDEX idx_github_issues_created ON github_issues(created_at);

CREATE INDEX idx_github_issues_repo ON github_issues(repo);

CREATE INDEX idx_github_issues_state ON github_issues(state);

CREATE INDEX idx_github_issues_updated ON github_issues(updated_at);

CREATE INDEX idx_github_prs_created ON github_prs(created_at);

CREATE INDEX idx_github_prs_repo ON github_prs(repo);

CREATE INDEX idx_github_prs_state ON github_prs(state);

CREATE INDEX idx_github_prs_updated ON github_prs(updated_at);

CREATE INDEX idx_image_descriptions_image ON image_descriptions(image_id);

CREATE INDEX idx_image_embeddings_model ON image_embeddings(model);

CREATE INDEX idx_image_occurrences_image ON image_occurrences(image_id);

CREATE INDEX idx_image_occurrences_session ON image_occurrences(session_id);

CREATE INDEX idx_image_ocr_backend ON image_ocr(backend);

CREATE INDEX idx_images_content_hash ON images(content_hash);

CREATE INDEX idx_memories_project ON memories(project);

CREATE INDEX idx_memories_type ON memories(memory_type);

CREATE INDEX idx_messages_content_type ON messages(content_type);

CREATE INDEX idx_messages_entry_id ON messages(entry_id) WHERE entry_id IS NOT NULL;

CREATE INDEX idx_messages_is_error ON messages(is_error) WHERE is_error = 1;

CREATE INDEX idx_messages_project ON messages(project);

CREATE INDEX idx_messages_session ON messages(session_id);

-- Supports the compaction owed-predicate (🎯T68.1/🎯T68.3) and
-- ReadSessionAfter (🎯T68.2), which count / page substantive messages
-- per session past a messages.id cursor. Since the predicate now scans
-- every session each scan (no recency floor), this partial composite
-- turns the per-session range scan into an index lookup.
CREATE INDEX idx_messages_session_id_substantive ON messages(session_id, id) WHERE is_noise = 0;

CREATE INDEX idx_messages_tool_command ON messages(tool_command) WHERE tool_command IS NOT NULL;

CREATE INDEX idx_messages_tool_description ON messages(tool_description) WHERE tool_description IS NOT NULL;

CREATE INDEX idx_messages_tool_file_path ON messages(tool_file_path) WHERE tool_file_path IS NOT NULL;

CREATE INDEX idx_messages_tool_name ON messages(tool_name);

CREATE INDEX idx_messages_tool_new_string ON messages(tool_new_string) WHERE tool_new_string IS NOT NULL;

CREATE INDEX idx_messages_tool_old_string ON messages(tool_old_string) WHERE tool_old_string IS NOT NULL;

CREATE INDEX idx_messages_tool_pattern ON messages(tool_pattern) WHERE tool_pattern IS NOT NULL;

CREATE INDEX idx_messages_tool_skill ON messages(tool_skill) WHERE tool_skill IS NOT NULL;

CREATE INDEX idx_messages_tool_task_id ON messages(tool_task_id) WHERE tool_task_id IS NOT NULL;

CREATE INDEX idx_messages_tool_url ON messages(tool_url) WHERE tool_url IS NOT NULL;

CREATE INDEX idx_messages_tool_use_id ON messages(tool_use_id);

CREATE INDEX idx_plans_phase ON plans(phase);

CREATE INDEX idx_plans_repo ON plans(repo);

CREATE INDEX idx_session_chains_predecessor ON session_chains(predecessor_id);

CREATE INDEX idx_session_nonces_session ON session_nonces(session_id);

CREATE INDEX idx_session_summary_type ON session_summary(session_type);

CREATE INDEX idx_skills_name ON skills(name);

CREATE INDEX idx_snapshot_files_entry ON snapshot_files(entry_id);

CREATE INDEX idx_snapshot_files_path ON snapshot_files(file_path);

CREATE INDEX idx_snapshot_files_session ON snapshot_files(session_id);

CREATE INDEX idx_targets_repo ON targets(repo);

CREATE INDEX idx_targets_status ON targets(status);

CREATE INDEX idx_targets_target_id ON targets(target_id);

-- Virtual tables (FTS5)

CREATE VIRTUAL TABLE audit_entries_fts USING fts5(
			summary, raw_text, repo,
			content=audit_entries,
			content_rowid=id
		);

CREATE VIRTUAL TABLE ci_runs_fts USING fts5(
			repo, workflow, branch, log_summary, conclusion,
			content='ci_runs', content_rowid='id'
		);

CREATE VIRTUAL TABLE claude_configs_fts USING fts5(
			content, repo,
			content=claude_configs,
			content_rowid=id
		);

CREATE VIRTUAL TABLE compactions_fts USING fts5(
			summary, session_id,
			content=compactions,
			content_rowid=id
		);

CREATE VIRTUAL TABLE decisions_fts USING fts5(
			proposal_text, confirmation_text, repo,
			content=decisions,
			content_rowid=id
		);

CREATE VIRTUAL TABLE docs_fts USING fts5(
			title, content, repo, kind, taxonomy,
			content=docs,
			content_rowid=id
		);

CREATE VIRTUAL TABLE git_commits_fts USING fts5(
			subject, body, repo, author_name,
			content=git_commits, content_rowid=id
		);

CREATE VIRTUAL TABLE github_issues_fts USING fts5(
			title, body, repo, author,
			content=github_issues, content_rowid=id
		);

CREATE VIRTUAL TABLE github_prs_fts USING fts5(
			title, body, repo, author,
			content=github_prs, content_rowid=id
		);

CREATE VIRTUAL TABLE image_descriptions_fts USING fts5(
			name,
			description,
			content=image_descriptions,
			content_rowid=id
		);

CREATE VIRTUAL TABLE image_ocr_fts USING fts5(
			text,
			content=image_ocr,
			content_rowid=image_id
		);

CREATE VIRTUAL TABLE memories_fts USING fts5(
			name, description, content, project,
			content=memories,
			content_rowid=id
		);

CREATE VIRTUAL TABLE messages_fts USING fts5(
			text, role, project, session_id,
			content=messages,
			content_rowid=id
		);

CREATE VIRTUAL TABLE plans_fts USING fts5(
			content, repo, phase,
			content=plans,
			content_rowid=id
		);

CREATE VIRTUAL TABLE skills_fts USING fts5(
			name, description, content,
			content=skills,
			content_rowid=id
		);

CREATE VIRTUAL TABLE snapshot_files_fts USING fts5(
			file_path,
			content=snapshot_files,
			content_rowid=id
		);

CREATE VIRTUAL TABLE targets_fts USING fts5(
			name, description, raw_text, repo,
			content=targets,
			content_rowid=id
		);

-- Triggers

CREATE TRIGGER audit_entries_ad AFTER DELETE ON audit_entries
		BEGIN
			INSERT INTO audit_entries_fts(audit_entries_fts, rowid, summary, raw_text, repo)
			VALUES ('delete', old.id, old.summary, old.raw_text, old.repo);
		END;

CREATE TRIGGER audit_entries_ai AFTER INSERT ON audit_entries
		BEGIN
			INSERT INTO audit_entries_fts(rowid, summary, raw_text, repo)
			VALUES (new.id, new.summary, new.raw_text, new.repo);
		END;

CREATE TRIGGER audit_entries_au AFTER UPDATE ON audit_entries
		BEGIN
			INSERT INTO audit_entries_fts(audit_entries_fts, rowid, summary, raw_text, repo)
			VALUES ('delete', old.id, old.summary, old.raw_text, old.repo);
			INSERT INTO audit_entries_fts(rowid, summary, raw_text, repo)
			VALUES (new.id, new.summary, new.raw_text, new.repo);
		END;

CREATE TRIGGER ci_runs_ai AFTER INSERT ON ci_runs
		BEGIN
			INSERT INTO ci_runs_fts(rowid, repo, workflow, branch, log_summary, conclusion)
			VALUES (new.id, new.repo, new.workflow, COALESCE(new.branch, ''), COALESCE(new.log_summary, ''), COALESCE(new.conclusion, ''));
		END;

CREATE TRIGGER ci_runs_au AFTER UPDATE ON ci_runs
		BEGIN
			INSERT INTO ci_runs_fts(ci_runs_fts, rowid, repo, workflow, branch, log_summary, conclusion)
			VALUES ('delete', old.id, old.repo, old.workflow, COALESCE(old.branch, ''), COALESCE(old.log_summary, ''), COALESCE(old.conclusion, ''));
			INSERT INTO ci_runs_fts(rowid, repo, workflow, branch, log_summary, conclusion)
			VALUES (new.id, new.repo, new.workflow, COALESCE(new.branch, ''), COALESCE(new.log_summary, ''), COALESCE(new.conclusion, ''));
		END;

CREATE TRIGGER claude_configs_ad AFTER DELETE ON claude_configs
		BEGIN
			INSERT INTO claude_configs_fts(claude_configs_fts, rowid, content, repo)
			VALUES ('delete', old.id, old.content, old.repo);
		END;

CREATE TRIGGER claude_configs_ai AFTER INSERT ON claude_configs
		BEGIN
			INSERT INTO claude_configs_fts(rowid, content, repo)
			VALUES (new.id, new.content, new.repo);
		END;

CREATE TRIGGER claude_configs_au AFTER UPDATE ON claude_configs
		BEGIN
			INSERT INTO claude_configs_fts(claude_configs_fts, rowid, content, repo)
			VALUES ('delete', old.id, old.content, old.repo);
			INSERT INTO claude_configs_fts(rowid, content, repo)
			VALUES (new.id, new.content, new.repo);
		END;

CREATE TRIGGER compactions_ai AFTER INSERT ON compactions
		BEGIN
			INSERT INTO compactions_fts(rowid, summary, session_id)
			VALUES (new.id, new.summary, new.session_id);
		END;

CREATE TRIGGER decisions_ad AFTER DELETE ON decisions
		BEGIN
			INSERT INTO decisions_fts(decisions_fts, rowid, proposal_text, confirmation_text, repo)
			VALUES ('delete', old.id, old.proposal_text, old.confirmation_text, old.repo);
		END;

CREATE TRIGGER decisions_ai AFTER INSERT ON decisions
		BEGIN
			INSERT INTO decisions_fts(rowid, proposal_text, confirmation_text, repo)
			VALUES (new.id, new.proposal_text, new.confirmation_text, new.repo);
		END;

CREATE TRIGGER decisions_au AFTER UPDATE ON decisions
		BEGIN
			INSERT INTO decisions_fts(decisions_fts, rowid, proposal_text, confirmation_text, repo)
			VALUES ('delete', old.id, old.proposal_text, old.confirmation_text, old.repo);
			INSERT INTO decisions_fts(rowid, proposal_text, confirmation_text, repo)
			VALUES (new.id, new.proposal_text, new.confirmation_text, new.repo);
		END;

CREATE TRIGGER docs_ad AFTER DELETE ON docs
		BEGIN
			INSERT INTO docs_fts(docs_fts, rowid, title, content, repo, kind, taxonomy)
			VALUES ('delete', old.id, old.title, old.content, old.repo, old.kind, old.taxonomy);
		END;

CREATE TRIGGER docs_ai AFTER INSERT ON docs
		BEGIN
			INSERT INTO docs_fts(rowid, title, content, repo, kind, taxonomy)
			VALUES (new.id, new.title, new.content, new.repo, new.kind, new.taxonomy);
		END;

CREATE TRIGGER docs_au AFTER UPDATE ON docs
		BEGIN
			INSERT INTO docs_fts(docs_fts, rowid, title, content, repo, kind, taxonomy)
			VALUES ('delete', old.id, old.title, old.content, old.repo, old.kind, old.taxonomy);
			INSERT INTO docs_fts(rowid, title, content, repo, kind, taxonomy)
			VALUES (new.id, new.title, new.content, new.repo, new.kind, new.taxonomy);
		END;

CREATE TRIGGER entries_file_snapshot AFTER INSERT ON entries
		WHEN new.type = 'file-history-snapshot'
		BEGIN
			INSERT INTO snapshot_files (entry_id, session_id, file_path, backup_time)
			SELECT new.id, new.session_id, f.key, f.value->>'backupTime'
			FROM json_each(new.raw, '$.snapshot.trackedFileBackups') f
			WHERE f.key != '';

			INSERT INTO snapshot_files_fts(rowid, file_path)
			SELECT sf.id, sf.file_path
			FROM snapshot_files sf
			WHERE sf.entry_id = new.id;
		END;

CREATE TRIGGER git_commits_ad AFTER DELETE ON git_commits
		BEGIN
			INSERT INTO git_commits_fts(git_commits_fts, rowid, subject, body, repo, author_name)
			VALUES ('delete', old.id, old.subject, old.body, old.repo, old.author_name);
		END;

CREATE TRIGGER git_commits_ai AFTER INSERT ON git_commits
		BEGIN
			INSERT INTO git_commits_fts(rowid, subject, body, repo, author_name)
			VALUES (new.id, new.subject, new.body, new.repo, new.author_name);
		END;

CREATE TRIGGER git_commits_au AFTER UPDATE ON git_commits
		BEGIN
			INSERT INTO git_commits_fts(git_commits_fts, rowid, subject, body, repo, author_name)
			VALUES ('delete', old.id, old.subject, old.body, old.repo, old.author_name);
			INSERT INTO git_commits_fts(rowid, subject, body, repo, author_name)
			VALUES (new.id, new.subject, new.body, new.repo, new.author_name);
		END;

CREATE TRIGGER github_issues_ad AFTER DELETE ON github_issues
		BEGIN
			INSERT INTO github_issues_fts(github_issues_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
		END;

CREATE TRIGGER github_issues_ai AFTER INSERT ON github_issues
		BEGIN
			INSERT INTO github_issues_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;

CREATE TRIGGER github_issues_au AFTER UPDATE ON github_issues
		BEGIN
			INSERT INTO github_issues_fts(github_issues_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
			INSERT INTO github_issues_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;

CREATE TRIGGER github_prs_ad AFTER DELETE ON github_prs
		BEGIN
			INSERT INTO github_prs_fts(github_prs_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
		END;

CREATE TRIGGER github_prs_ai AFTER INSERT ON github_prs
		BEGIN
			INSERT INTO github_prs_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;

CREATE TRIGGER github_prs_au AFTER UPDATE ON github_prs
		BEGIN
			INSERT INTO github_prs_fts(github_prs_fts, rowid, title, body, repo, author)
			VALUES ('delete', old.id, old.title, old.body, old.repo, old.author);
			INSERT INTO github_prs_fts(rowid, title, body, repo, author)
			VALUES (new.id, new.title, new.body, new.repo, new.author);
		END;

CREATE TRIGGER image_descriptions_ad AFTER DELETE ON image_descriptions
		BEGIN
			INSERT INTO image_descriptions_fts(image_descriptions_fts, rowid, name, description)
			VALUES ('delete', old.id, old.name, old.description);
		END;

CREATE TRIGGER image_descriptions_ai AFTER INSERT ON image_descriptions
		BEGIN
			INSERT INTO image_descriptions_fts(rowid, name, description)
			VALUES (new.id, new.name, new.description);
		END;

CREATE TRIGGER image_descriptions_au AFTER UPDATE ON image_descriptions
		BEGIN
			INSERT INTO image_descriptions_fts(image_descriptions_fts, rowid, name, description)
			VALUES ('delete', old.id, old.name, old.description);
			INSERT INTO image_descriptions_fts(rowid, name, description)
			VALUES (new.id, new.name, new.description);
		END;

CREATE TRIGGER image_ocr_ad AFTER DELETE ON image_ocr
		BEGIN
			INSERT INTO image_ocr_fts(image_ocr_fts, rowid, text)
			VALUES ('delete', old.image_id, old.text);
		END;

CREATE TRIGGER image_ocr_ai AFTER INSERT ON image_ocr
		BEGIN
			INSERT INTO image_ocr_fts(rowid, text)
			VALUES (new.image_id, new.text);
		END;

CREATE TRIGGER image_ocr_au AFTER UPDATE ON image_ocr
		BEGIN
			INSERT INTO image_ocr_fts(image_ocr_fts, rowid, text)
			VALUES ('delete', old.image_id, old.text);
			INSERT INTO image_ocr_fts(rowid, text)
			VALUES (new.image_id, new.text);
		END;

CREATE TRIGGER memories_ad AFTER DELETE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, name, description, content, project)
			VALUES ('delete', old.id, old.name, old.description, old.content, old.project);
		END;

CREATE TRIGGER memories_ai AFTER INSERT ON memories
		BEGIN
			INSERT INTO memories_fts(rowid, name, description, content, project)
			VALUES (new.id, new.name, new.description, new.content, new.project);
		END;

CREATE TRIGGER memories_au AFTER UPDATE ON memories
		BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, name, description, content, project)
			VALUES ('delete', old.id, old.name, old.description, old.content, old.project);
			INSERT INTO memories_fts(rowid, name, description, content, project)
			VALUES (new.id, new.name, new.description, new.content, new.project);
		END;

CREATE TRIGGER messages_ai AFTER INSERT ON messages
		BEGIN
			INSERT INTO messages_fts(rowid, text, role, project, session_id)
			SELECT new.id, new.text, new.role, new.project, new.session_id
			WHERE new.is_noise = 0;

			INSERT INTO session_summary (session_id, project, session_type, total_msgs, substantive_msgs, first_msg, last_msg)
			VALUES (new.session_id, new.project,
				CASE
	WHEN new.project = 'subagents' THEN 'subagent'
	WHEN new.project LIKE '%worktrees%' THEN 'worktree'
	WHEN new.project LIKE '%-private-tmp%' THEN 'ephemeral'
	ELSE 'interactive'
END,
				1,
				CASE WHEN new.is_noise = 0 THEN 1 ELSE 0 END,
				new.timestamp, new.timestamp)
			ON CONFLICT(session_id) DO UPDATE SET
				total_msgs = total_msgs + 1,
				substantive_msgs = substantive_msgs + CASE WHEN new.is_noise = 0 THEN 1 ELSE 0 END,
				first_msg = MIN(first_msg, new.timestamp),
				last_msg = MAX(last_msg, new.timestamp);
		END;

CREATE TRIGGER plans_ad AFTER DELETE ON plans
		BEGIN
			INSERT INTO plans_fts(plans_fts, rowid, content, repo, phase)
			VALUES ('delete', old.id, old.content, old.repo, old.phase);
		END;

CREATE TRIGGER plans_ai AFTER INSERT ON plans
		BEGIN
			INSERT INTO plans_fts(rowid, content, repo, phase)
			VALUES (new.id, new.content, new.repo, new.phase);
		END;

CREATE TRIGGER plans_au AFTER UPDATE ON plans
		BEGIN
			INSERT INTO plans_fts(plans_fts, rowid, content, repo, phase)
			VALUES ('delete', old.id, old.content, old.repo, old.phase);
			INSERT INTO plans_fts(rowid, content, repo, phase)
			VALUES (new.id, new.content, new.repo, new.phase);
		END;

CREATE TRIGGER skills_ad AFTER DELETE ON skills
		BEGIN
			INSERT INTO skills_fts(skills_fts, rowid, name, description, content)
			VALUES ('delete', old.id, old.name, old.description, old.content);
		END;

CREATE TRIGGER skills_ai AFTER INSERT ON skills
		BEGIN
			INSERT INTO skills_fts(rowid, name, description, content)
			VALUES (new.id, new.name, new.description, new.content);
		END;

CREATE TRIGGER skills_au AFTER UPDATE ON skills
		BEGIN
			INSERT INTO skills_fts(skills_fts, rowid, name, description, content)
			VALUES ('delete', old.id, old.name, old.description, old.content);
			INSERT INTO skills_fts(rowid, name, description, content)
			VALUES (new.id, new.name, new.description, new.content);
		END;

CREATE TRIGGER targets_ad AFTER DELETE ON targets
		BEGIN
			INSERT INTO targets_fts(targets_fts, rowid, name, description, raw_text, repo)
			VALUES ('delete', old.id, old.name, old.description, old.raw_text, old.repo);
		END;

CREATE TRIGGER targets_ai AFTER INSERT ON targets
		BEGIN
			INSERT INTO targets_fts(rowid, name, description, raw_text, repo)
			VALUES (new.id, new.name, new.description, new.raw_text, new.repo);
		END;

CREATE TRIGGER targets_au AFTER UPDATE ON targets
		BEGIN
			INSERT INTO targets_fts(targets_fts, rowid, name, description, raw_text, repo)
			VALUES ('delete', old.id, old.name, old.description, old.raw_text, old.repo);
			INSERT INTO targets_fts(rowid, name, description, raw_text, repo)
			VALUES (new.id, new.name, new.description, new.raw_text, new.repo);
		END;

-- Views

CREATE VIEW sessions AS
		SELECT
			ss.session_id,
			ss.project,
			ss.session_type,
			COALESCE(sm.repo, '') AS repo,
			COALESCE(sm.git_branch, '') AS git_branch,
			COALESCE(sm.work_type, '') AS work_type,
			COALESCE(sm.topic, '') AS topic,
			ss.total_msgs,
			ss.substantive_msgs,
			ss.first_msg,
			ss.last_msg
		FROM session_summary ss
		LEFT JOIN session_meta sm ON sm.session_id = ss.session_id;

