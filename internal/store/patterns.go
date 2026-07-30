// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Pattern types recognised by the miner. Persisted verbatim in
// patterns.pattern_type, so treat these as a stable contract: a rename
// orphans every existing row rather than updating it.
const (
	PatternDirectJSONLRead = "direct_jsonl_read"
	PatternTranscriptGrep  = "transcript_grep"
	PatternRepeatedQuery   = "repeated_query"
	PatternRepeatedSearch  = "repeated_search"
)

// Signatures for the two single-shape families. The grouped families
// (repeated_query, repeated_search) derive their signature from the
// normalised input instead.
const (
	sigDirectJSONLRead = "bash:read-jsonl-under-claude-projects"
	sigTranscriptGrep  = "grep:transcript-directory"
)

// Emission gate for a persisted pattern: occurrence >= 3 across >= 2
// sessions (docs/design/vault-library-wing.md § Slice 7,
// docs/design/vault-clustering.md § Inputs 3). Both the vault renderer
// and the clustering corpus stream apply exactly this pair, so a
// pattern visible as a page is a pattern the clusterer also sees.
const (
	PatternEmitMinOccurrences = 3
	PatternEmitMinSessions    = 2
)

// PatternStreamWeight is the clustering weight for the patterns corpus
// stream (docs/design/vault-clustering.md § Inputs 3): patterns are
// explicit recurrences and the highest-signal of the four streams.
const PatternStreamWeight = 1.2

// patternsMineWindowDays bounds how far back a mining pass looks. The
// window governs the counts; first_seen accumulates across passes and
// is not clipped by it (see upsertPattern).
const patternsMineWindowDays = 90

// patternsRefreshInterval is the reconcile cadence. "Medium cadence
// (hourly)" per the design — mining is a handful of indexed scans over
// messages, cheap enough to keep fresh but not worth doing per minute.
const patternsRefreshInterval = time.Hour

// patternExcerptLimit caps how many representative excerpts a pattern
// row carries, and patternExcerptLen truncates each. The detector
// queries order by timestamp so "the first three" is the same three on
// every pass — otherwise a re-mine would reshuffle the excerpts, rewrite
// every vault page, and make the idempotence claim false.
const (
	patternExcerptLimit = 3
	patternExcerptLen   = 200
)

// patternSessionListLimit bounds the session ids a row carries.
// SessionCount stays exact; the list is a sample, and every renderer
// says "showing N of M" rather than letting a truncated list read as
// the whole set. A pattern spanning 400 sessions should not put 400
// UUIDs in a JSON column or on a vault page.
const patternSessionListLimit = 20

// PatternCandidate is a detected workaround pattern suggesting a
// missing mnemo feature.
//
// Occurrences and SessionCount are different numbers and both matter:
// one session that read six transcript files directly is 6 occurrences
// across 1 session, which the emission gate rejects for lack of
// corroboration.
type PatternCandidate struct {
	ID          string `json:"id"`
	PatternType string `json:"pattern_type"`
	// Signature is the canonicalised input that defines the pattern —
	// a fixed string for the single-shape families, the normalised SQL
	// or search text for the grouped ones. (type, signature) is the
	// identity: ID is derived from it.
	Signature   string `json:"signature"`
	Description string `json:"description"`
	Occurrences int    `json:"occurrences"`
	// SessionCount is the exact number of distinct sessions the pattern
	// was observed in. Sessions is a capped sample of those ids (full
	// ids, never prefixes — see patternSessionListLimit), so callers
	// report len(Sessions) against SessionCount rather than as it.
	SessionCount int      `json:"session_count"`
	Sessions     []string `json:"sessions"`
	Repos        []string `json:"repos"`
	// Evidence is the first representative excerpt, kept as a distinct
	// field because callers rendering a single example are the common
	// case.
	Evidence   string   `json:"evidence"`
	Excerpts   []string `json:"representative_excerpts"`
	Suggestion string   `json:"suggestion"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
	ComputedAt string   `json:"computed_at"`
}

// Emittable reports whether the pattern clears the emission gate —
// the shared filter for vault pages and the clustering corpus.
func (p PatternCandidate) Emittable() bool {
	return p.Occurrences >= PatternEmitMinOccurrences &&
		p.SessionCount >= PatternEmitMinSessions
}

// PatternQuery selects persisted patterns. The zero value returns
// every row, newest activity first.
type PatternQuery struct {
	// Query is an FTS5 match against pattern_type, signature and the
	// representative excerpts. Empty matches everything.
	Query string
	// Repo matches a substring of any repo in the pattern's repo set.
	Repo string
	// Days bounds on last_seen. Zero or negative means all time.
	Days int
	// MinOccurrences and MinSessions default to 0 (no filter) so a
	// caller must ask for the emission gate explicitly. Emittable() is
	// the canonical spelling of it.
	MinOccurrences int
	MinSessions    int
	Limit          int
}

// ClusterCorpusDoc is one document in the clustering corpus
// (docs/design/vault-clustering.md § Stream merge):
//
//	corpus(doc_id, kind, entity_id, repo, text, ts, weight)
//	kind ∈ {decision, compaction, pattern, vault_user}
//
// 🎯T64.7 lands the `pattern` stream. The other three arrive with the
// engine itself (🎯T64.8), which is what consumes this type — nothing
// clusters today.
type ClusterCorpusDoc struct {
	DocID    string  `json:"doc_id"`
	Kind     string  `json:"kind"`
	EntityID string  `json:"entity_id"`
	Repo     string  `json:"repo"`
	Text     string  `json:"text"`
	TS       string  `json:"ts"`
	Weight   float64 `json:"weight"`
}

// patternID derives the stable row id from the pattern's identity.
func patternID(patternType, signature string) string {
	sum := sha1.Sum([]byte(patternType + "\x00" + signature))
	return "pattern_" + hex.EncodeToString(sum[:])[:8]
}

// patternDescription renders the human-facing one-liner for a pattern.
// Derived rather than stored: it is a pure function of the identity
// plus the counts, and storing it would let a copy drift from the row
// that produced it.
func patternDescription(patternType, signature string, sessionCount int) string {
	switch patternType {
	case PatternDirectJSONLRead:
		return "Bash commands reading JSONL transcript files directly instead of using mnemo tools"
	case PatternTranscriptGrep:
		return "Grep/rg commands targeting transcript directories instead of using mnemo_search"
	case PatternRepeatedQuery:
		return fmt.Sprintf("The same mnemo_query shape was run across %s — candidate for a template",
			pluralizeSessions(sessionCount))
	case PatternRepeatedSearch:
		return fmt.Sprintf("Search pattern %q repeated across %s — may warrant a dedicated tool",
			signature, pluralizeSessions(sessionCount))
	}
	return patternType
}

// patternSuggestion names the action a pattern implies.
func patternSuggestion(patternType string) string {
	switch patternType {
	case PatternDirectJSONLRead:
		return "Use mnemo_search or mnemo_read_session instead of reading JSONL files directly."
	case PatternTranscriptGrep:
		return "Use mnemo_search with appropriate query terms instead of grep over transcript dirs."
	case PatternRepeatedQuery:
		return "Save this query as a template with mnemo_define for reuse."
	case PatternRepeatedSearch:
		return "Consider adding a dedicated mnemo tool for this recurring search need."
	}
	return ""
}

func pluralizeSessions(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// patternObservation is one raw hit from a mining query.
type patternObservation struct {
	sessionID string
	repo      string
	ts        string
	toolName  string
	text      string
}

// searchCommands are the shell tools that mean "search" for the purpose
// of the transcript_grep detector.
var searchCommands = map[string]struct{}{
	"grep": {}, "egrep": {}, "fgrep": {}, "zgrep": {},
	"rg": {}, "ripgrep": {}, "ag": {}, "ack": {}, "ugrep": {},
}

// isSearchCommand reports whether cmd invokes a search tool in any of
// its pipeline segments.
//
// The transcript_grep detector's SQL can only ask "does this command
// mention a transcript directory", which a `cat` or a `wc -l` satisfies
// just as well as a grep. Persisting those under a description that
// says "Grep/rg commands" would put a false claim on a vault page and
// into the clustering corpus, where patterns carry the highest weight
// of the four streams. direct_jsonl_read already covers the reads.
func isSearchCommand(cmd string) bool {
	for _, segment := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == '|' || r == ';' || r == '&' || r == '(' || r == '\n'
	}) {
		for _, word := range strings.Fields(segment) {
			// Skip leading VAR=value assignments and `env`-style
			// prefixes so `LC_ALL=C grep …` still counts.
			if strings.Contains(word, "=") && !strings.HasPrefix(word, "-") {
				continue
			}
			base := word
			if i := strings.LastIndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			if _, ok := searchCommands[base]; ok {
				return true
			}
			// Only the first real word of a segment names the command.
			break
		}
	}
	return false
}

// patternGroup accumulates observations sharing an identity.
type patternGroup struct {
	patternType string
	signature   string
	occurrences int
	sessions    map[string]struct{}
	repos       map[string]struct{}
	excerpts    []string
	seenExcerpt map[string]struct{}
	firstSeen   string
	lastSeen    string
}

func (g *patternGroup) add(o patternObservation) {
	g.occurrences++
	if o.sessionID != "" {
		// Full ids, never truncated: session_count is a distinct count,
		// and two ids sharing an 8-char prefix are two sessions. Keying
		// on a prefix collapsed them and under-counted the corroboration
		// the emission gate turns on.
		g.sessions[o.sessionID] = struct{}{}
	}
	if o.repo != "" {
		g.repos[o.repo] = struct{}{}
	}
	if o.ts != "" {
		if g.firstSeen == "" || o.ts < g.firstSeen {
			g.firstSeen = o.ts
		}
		if o.ts > g.lastSeen {
			g.lastSeen = o.ts
		}
	}
	if text := truncateExcerpt(o.text); text != "" && len(g.excerpts) < patternExcerptLimit {
		if _, dup := g.seenExcerpt[text]; !dup {
			g.seenExcerpt[text] = struct{}{}
			g.excerpts = append(g.excerpts, text)
		}
	}
}

func (g *patternGroup) candidate(computedAt string) PatternCandidate {
	c := PatternCandidate{
		ID:           patternID(g.patternType, g.signature),
		PatternType:  g.patternType,
		Signature:    g.signature,
		Occurrences:  g.occurrences,
		SessionCount: len(g.sessions),
		Sessions:     capSlice(sortedSet(g.sessions), patternSessionListLimit),
		Repos:        sortedSet(g.repos),
		Excerpts:     g.excerpts,
		FirstSeen:    g.firstSeen,
		LastSeen:     g.lastSeen,
		ComputedAt:   computedAt,
	}
	c.Description = patternDescription(c.PatternType, c.Signature, c.SessionCount)
	c.Suggestion = patternSuggestion(c.PatternType)
	if len(c.Excerpts) > 0 {
		c.Evidence = c.Excerpts[0]
	}
	return c
}

func sortedSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// capSlice bounds a list to n entries. Callers report the true count
// separately so a capped list never passes for the whole set.
func capSlice(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
}

func truncateExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > patternExcerptLen {
		return s[:patternExcerptLen] + "..."
	}
	return s
}

// minePatterns runs the four detectors over the trailing `days` window
// and returns one candidate per (type, signature), with truthful
// occurrence and session counts.
//
// Deliberately takes no repo filter: mining is a whole-corpus pass that
// records which repos each pattern spans, and filtering by repo is a
// read-side concern over the persisted rows. Filtering here would make
// the counts depend on who asked.
func (s *Store) minePatterns(days int) ([]PatternCandidate, error) {
	if days <= 0 {
		days = patternsMineWindowDays
	}
	window := fmt.Sprintf("-%d days", days)
	computedAt := time.Now().UTC().Format(time.RFC3339)

	groups := map[string]*patternGroup{}
	record := func(patternType, signature string, o patternObservation) {
		key := patternType + "\x00" + signature
		g, ok := groups[key]
		if !ok {
			g = &patternGroup{
				patternType: patternType,
				signature:   signature,
				sessions:    map[string]struct{}{},
				repos:       map[string]struct{}{},
				seenExcerpt: map[string]struct{}{},
			}
			groups[key] = g
		}
		g.add(o)
	}

	// Direct JSONL reads via Bash.
	err := s.scanPatternObservations(`
		SELECT m.session_id, COALESCE(sm.repo, ''), e.timestamp, m.tool_name, m.tool_command
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name = 'Bash'
		  AND m.tool_command IS NOT NULL
		  AND (m.tool_command LIKE '%/.claude/projects/%' OR m.tool_command LIKE '%/.claude/sessions/%')
		  AND m.tool_command LIKE '%.jsonl%'
		  AND e.timestamp >= datetime('now', ?)
		ORDER BY e.timestamp, m.id
	`, []any{window}, func(o patternObservation) {
		record(PatternDirectJSONLRead, sigDirectJSONLRead, o)
	})
	if err != nil {
		return nil, fmt.Errorf("mine direct_jsonl_read: %w", err)
	}

	// Searches over transcript directories. The SQL predicate can only
	// say "mentions a transcript path"; isSearchCommand decides whether
	// a Bash row is actually a search.
	err = s.scanPatternObservations(`
		SELECT m.session_id, COALESCE(sm.repo, ''), e.timestamp, m.tool_name,
		       COALESCE(m.tool_command, m.tool_pattern)
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name IN ('Bash', 'Grep')
		  AND (
		    m.tool_command LIKE '%/.claude/projects/%'
		    OR m.tool_command LIKE '%/.claude/sessions/%'
		    OR m.tool_pattern LIKE '%/.claude/projects/%'
		    OR m.tool_pattern LIKE '%/.claude/sessions/%'
		  )
		  AND e.timestamp >= datetime('now', ?)
		ORDER BY e.timestamp, m.id
	`, []any{window}, func(o patternObservation) {
		// The Grep tool is a search by definition; a Bash command has to
		// prove it (see isSearchCommand).
		if o.toolName == "Bash" && !isSearchCommand(o.text) {
			return
		}
		record(PatternTranscriptGrep, sigTranscriptGrep, o)
	})
	if err != nil {
		return nil, fmt.Errorf("mine transcript_grep: %w", err)
	}

	// Repeated mnemo_query shapes.
	err = s.scanPatternObservations(`
		SELECT m.session_id, COALESCE(sm.repo, ''), e.timestamp, m.tool_name, m.tool_query
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name = 'mnemo_query'
		  AND m.tool_query IS NOT NULL
		  AND e.timestamp >= datetime('now', ?)
		ORDER BY e.timestamp, m.id
	`, []any{window}, func(o patternObservation) {
		record(PatternRepeatedQuery, discoverNormalizeSQL(o.text), o)
	})
	if err != nil {
		return nil, fmt.Errorf("mine repeated_query: %w", err)
	}

	// Repeated mnemo_search terms.
	err = s.scanPatternObservations(`
		SELECT m.session_id, COALESCE(sm.repo, ''), e.timestamp, m.tool_name, m.tool_query
		FROM messages m
		JOIN entries e ON e.id = m.entry_id
		LEFT JOIN session_meta sm ON sm.session_id = m.session_id
		WHERE m.content_type = 'tool_use'
		  AND m.tool_name = 'mnemo_search'
		  AND m.tool_query IS NOT NULL
		  AND e.timestamp >= datetime('now', ?)
		ORDER BY e.timestamp, m.id
	`, []any{window}, func(o patternObservation) {
		record(PatternRepeatedSearch, discoverNormalizeSearch(o.text), o)
	})
	if err != nil {
		return nil, fmt.Errorf("mine repeated_search: %w", err)
	}

	out := make([]PatternCandidate, 0, len(groups))
	for _, key := range sortedGroupKeys(groups) {
		g := groups[key]
		if g.signature == "" {
			continue
		}
		out = append(out, g.candidate(computedAt))
	}
	return out, nil
}

// sortedGroupKeys orders the group map so a mining pass emits
// candidates deterministically. (segment_cluster.go's sortedKeys covers
// map[string]struct{}; Go has no overloading.)
func sortedGroupKeys(m map[string]*patternGroup) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scanPatternObservations runs one detector query and feeds every row
// to sink. A row that fails to scan is skipped rather than aborting the
// pass: a single malformed tool_input should not cost the whole mine.
func (s *Store) scanPatternObservations(query string, args []any, sink func(patternObservation)) error {
	rows, err := s.readDB.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var o patternObservation
		var text sql.NullString
		if err := rows.Scan(&o.sessionID, &o.repo, &o.ts, &o.toolName, &text); err != nil {
			continue
		}
		o.text = text.String
		sink(o)
	}
	return rows.Err()
}

// RefreshPatterns mines the trailing window and upserts every
// discovered pattern, returning the number of rows written.
//
// Idempotent: re-running with an unchanged corpus rewrites the same
// values. Rows for patterns that have aged out of the window are left
// standing — the table is a record of what has been observed, and
// deleting a pattern because nobody triggered it this quarter would
// lose the first_seen the row exists to accumulate. Callers that care
// about recency filter on last_seen.
func (s *Store) RefreshPatterns(now time.Time) (int, error) {
	candidates, err := s.minePatterns(patternsMineWindowDays)
	if err != nil {
		return 0, err
	}
	stamp := now.UTC().Format(time.RFC3339)
	written := 0
	for _, c := range candidates {
		c.ComputedAt = stamp
		if err := s.upsertPattern(c); err != nil {
			return written, fmt.Errorf("upsert %s: %w", c.ID, err)
		}
		written++
	}
	return written, nil
}

// upsertPattern writes one pattern row. Counts are replaced (they
// describe the current window) while first_seen only ever moves
// earlier and last_seen only ever moves later, so the row accumulates
// a true observation span across passes even though each pass sees a
// bounded window.
func (s *Store) upsertPattern(c PatternCandidate) error {
	repos, err := json.Marshal(nonNil(c.Repos))
	if err != nil {
		return err
	}
	sessions, err := json.Marshal(nonNil(c.Sessions))
	if err != nil {
		return err
	}
	excerpts, err := json.Marshal(nonNil(c.Excerpts))
	if err != nil {
		return err
	}
	_, err = s.writeDB.Exec(`
		INSERT INTO patterns (
			id, pattern_type, signature, occurrence_count, session_count,
			repos, sessions, first_seen, last_seen,
			representative_excerpts, computed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			occurrence_count = excluded.occurrence_count,
			session_count = excluded.session_count,
			repos = excluded.repos,
			sessions = excluded.sessions,
			first_seen = CASE
				WHEN patterns.first_seen = '' THEN excluded.first_seen
				WHEN excluded.first_seen = '' THEN patterns.first_seen
				WHEN excluded.first_seen < patterns.first_seen THEN excluded.first_seen
				ELSE patterns.first_seen
			END,
			last_seen = CASE
				WHEN excluded.last_seen > patterns.last_seen THEN excluded.last_seen
				ELSE patterns.last_seen
			END,
			representative_excerpts = excluded.representative_excerpts,
			computed_at = excluded.computed_at
	`,
		c.ID, c.PatternType, c.Signature, c.Occurrences, c.SessionCount,
		string(repos), string(sessions), c.FirstSeen, c.LastSeen,
		string(excerpts), c.ComputedAt,
	)
	return err
}

// nonNil returns an empty slice for nil so json.Marshal emits `[]`
// rather than `null` — the columns are declared NOT NULL DEFAULT '[]'
// and a `null` there would break every JSON reader downstream.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ListPatterns reads persisted patterns. This is the only read path;
// nothing re-mines on demand except the lazy first-run refresh in
// DiscoverPatterns.
func (s *Store) ListPatterns(q PatternQuery) ([]PatternCandidate, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}

	var where []string
	var args []any
	if strings.TrimSpace(q.Query) != "" {
		where = append(where, `p.rowid IN (SELECT rowid FROM patterns_fts WHERE patterns_fts MATCH ?)`)
		args = append(args, q.Query)
	}
	if q.Repo != "" {
		// repos is a JSON array; match a substring of any element
		// rather than of the serialised array, so a filter of "mnemo"
		// cannot be satisfied by the punctuation between two repos.
		where = append(where, `EXISTS (
			SELECT 1 FROM json_each(p.repos) je
			WHERE je.value LIKE ?
		)`)
		args = append(args, "%"+q.Repo+"%")
	}
	if q.Days > 0 {
		where = append(where, `(p.last_seen = '' OR p.last_seen >= datetime('now', ?))`)
		args = append(args, fmt.Sprintf("-%d days", q.Days))
	}
	if q.MinOccurrences > 0 {
		where = append(where, `p.occurrence_count >= ?`)
		args = append(args, q.MinOccurrences)
	}
	if q.MinSessions > 0 {
		where = append(where, `p.session_count >= ?`)
		args = append(args, q.MinSessions)
	}

	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)

	rows, err := s.readDB.Query(fmt.Sprintf(`
		SELECT p.id, p.pattern_type, p.signature, p.occurrence_count,
		       p.session_count, p.repos, p.sessions, p.first_seen,
		       p.last_seen, p.representative_excerpts, p.computed_at
		FROM patterns p
		%s
		ORDER BY p.occurrence_count DESC, p.last_seen DESC, p.id
		LIMIT ?
	`, clause), args...)
	if err != nil {
		return nil, fmt.Errorf("list patterns: %w", err)
	}
	defer rows.Close()

	var out []PatternCandidate
	for rows.Next() {
		var c PatternCandidate
		var repos, sessions, excerpts string
		if err := rows.Scan(
			&c.ID, &c.PatternType, &c.Signature, &c.Occurrences,
			&c.SessionCount, &repos, &sessions, &c.FirstSeen,
			&c.LastSeen, &excerpts, &c.ComputedAt,
		); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(repos), &c.Repos)
		json.Unmarshal([]byte(sessions), &c.Sessions)
		json.Unmarshal([]byte(excerpts), &c.Excerpts)
		c.Description = patternDescription(c.PatternType, c.Signature, c.SessionCount)
		c.Suggestion = patternSuggestion(c.PatternType)
		if len(c.Excerpts) > 0 {
			c.Evidence = c.Excerpts[0]
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DiscoverPatterns serves mnemo_discover_patterns from the persisted
// table (🎯T64.7). It used to mine on every call; now the hourly
// patterns reconciler owns mining and this is a read.
//
// The one exception is a table that has never been computed — a fresh
// install, or the first run after the upgrade that added it. Rather
// than answer "no patterns" (indistinguishable from "no patterns
// exist"), the first caller pays for one mining pass.
func (s *Store) DiscoverPatterns(days int, repoFilter string, minOccurrences int) ([]PatternCandidate, error) {
	if days <= 0 {
		days = patternsMineWindowDays
	}
	if minOccurrences <= 0 {
		minOccurrences = PatternEmitMinOccurrences
	}

	computed, err := s.patternsComputedAt()
	if err != nil {
		return nil, err
	}
	if computed.IsZero() {
		if _, err := s.RefreshPatterns(time.Now()); err != nil {
			return nil, err
		}
	}

	return s.ListPatterns(PatternQuery{
		Repo:           repoFilter,
		Days:           days,
		MinOccurrences: minOccurrences,
		MinSessions:    PatternEmitMinSessions,
	})
}

// patternsComputedAt returns the most recent successful mining stamp,
// or the zero time when the table has never been computed.
func (s *Store) patternsComputedAt() (time.Time, error) {
	var stamp sql.NullString
	err := s.readDB.QueryRow(`SELECT MAX(computed_at) FROM patterns`).Scan(&stamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("patterns computed_at: %w", err)
	}
	if !stamp.Valid || stamp.String == "" {
		return time.Time{}, nil
	}
	t, perr := time.Parse(time.RFC3339, stamp.String)
	if perr != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// PatternCorpusDocs returns the `pattern` stream of the clustering
// corpus (docs/design/vault-clustering.md § Inputs 3): patterns past
// the emission gate, each weighted PatternStreamWeight.
//
// A pattern spanning several repos yields one doc per repo, since the
// corpus row is repo-scoped and a cross-repo pattern is evidence in
// each. A pattern with no repo attribution yields a single doc with an
// empty repo rather than being dropped.
func (s *Store) PatternCorpusDocs() ([]ClusterCorpusDoc, error) {
	patterns, err := s.ListPatterns(PatternQuery{
		MinOccurrences: PatternEmitMinOccurrences,
		MinSessions:    PatternEmitMinSessions,
	})
	if err != nil {
		return nil, err
	}
	var out []ClusterCorpusDoc
	for _, p := range patterns {
		text := strings.TrimSpace(strings.Join(append(
			[]string{p.Description, p.Signature}, p.Excerpts...), "\n"))
		repos := p.Repos
		if len(repos) == 0 {
			repos = []string{""}
		}
		for _, repo := range repos {
			out = append(out, ClusterCorpusDoc{
				DocID:    "pattern:" + p.ID,
				Kind:     "pattern",
				EntityID: p.ID,
				Repo:     repo,
				Text:     text,
				TS:       p.LastSeen,
				Weight:   PatternStreamWeight,
			})
		}
	}
	return out, nil
}

// patternsReconcilerStream drives the hourly patterns refresh under the
// 🎯T68 convergence data plane. The registry worker ticks every minute,
// so the age check here — not Interval() — is what enforces the
// cadence.
type patternsReconcilerStream struct{ s *Store }

func (p patternsReconcilerStream) Name() string            { return "patterns" }
func (p patternsReconcilerStream) Interval() time.Duration { return patternsRefreshInterval }

func (p patternsReconcilerStream) Reconcile(ctx context.Context, now time.Time) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	computed, err := p.s.patternsComputedAt()
	if err != nil {
		return 0, err
	}
	if !computed.IsZero() && now.Sub(computed) < patternsRefreshInterval {
		return 0, nil
	}
	return p.s.RefreshPatterns(now)
}

// discoverNormalizeSQL strips string literals and numbers from a SQL query
// and collapses whitespace to produce a structural shape for grouping.
func discoverNormalizeSQL(q string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			if !inStr {
				inStr = true
			} else {
				inStr = false
				b.WriteString("?")
			}
			continue
		}
		if inStr {
			continue
		}
		b.WriteByte(c)
	}
	s := b.String()

	result := make([]byte, 0, len(s))
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			result = append(result, '?')
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
		} else {
			result = append(result, s[i])
			i++
		}
	}

	return strings.Join(strings.Fields(strings.ToLower(string(result))), " ")
}

// discoverNormalizeSearch lowercases and sorts words for canonical grouping.
func discoverNormalizeSearch(q string) string {
	words := strings.Fields(strings.ToLower(q))
	sort.Strings(words)
	return strings.Join(words, " ")
}
