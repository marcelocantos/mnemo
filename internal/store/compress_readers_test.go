// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestCompressedColumnsAreReadThroughMnemoText is the ratchet for 🎯T151:
// a compressed row holds ” in messages.text / docs.content, so any SQL
// that reads the bare column silently sees empty text for new rows. The
// test walks every non-test Go file, pulls the string literals that name
// the messages or docs tables, and fails on a bare column reference.
// Writers (INSERT) and the FTS shadow columns are exempt.
func TestCompressedColumnsAreReadThroughMnemoText(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "bin" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests are scanned too: a test that reads a compressed column
		// from the base table passes only while nothing has compressed
		// that row yet, so it fails later and elsewhere (this rule was
		// added after TestEntriesTable started failing on Windows once
		// boot-time packing stopped racing it). compress_test.go is
		// exempt because asserting the storage shape is its job.
		if strings.HasSuffix(path, "_test.go") && filepath.Base(path) != "compress_test.go" {
			// fall through to the scan
			_ = path
		}
		if filepath.Base(path) == "compress_test.go" || filepath.Base(path) == "compress_readers_test.go" {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, col := range bareColumnReads(s) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, fmt.Sprintf("%s: bare %s in %s near %q",
					fset.Position(lit.Pos()), col, rel, bareSnippet(s, col)))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Error(o)
	}
	if len(offenders) > 0 {
		t.Errorf("%d SQL literal(s) read a compressed column directly; use mnemo_text(col, col_z) or the *_v view", len(offenders))
	}
}

var (
	// Reads only: writers go through textCodec.pack, and the compressing
	// GC's own UPDATE is exactly the statement that sets the sentinel.
	messagesTableRe = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+messages\b`)
	docsTableRe     = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+docs\b`)
	// 🎯T152: entries readers must use entries_v — the base table's
	// generated columns and raw are NULL once a row is compressed.
	entriesReadRe    = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+entries\b`)
	entriesDeleteRe  = regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+entries\b`)
	entriesHotColsRe = regexp.MustCompile(`(?i)\b(raw|uuid|model|stop_reason|input_tokens|output_tokens|cache_read_tokens|cache_creation_tokens|agent_id|version|slug|is_sidechain|data_type|data_command|data_hook_event|top_tool_use_id|parent_tool_use_id)\b`)
	bareTextRe       = regexp.MustCompile(`(?i)(^|[^\w.'])(\w+\.)?text\b`)
	bareContentRe    = regexp.MustCompile(`(?i)(^|[^\w.'])(\w+\.)?content\b`)
)

// bareColumnReads returns the compressed columns that literal s reads
// without going through mnemo_text. Only literals that actually query the
// base tables (not the _v views or _fts shadows) are considered.
func bareColumnReads(s string) []string {
	var out []string
	if messagesTableRe.MatchString(s) && !strings.Contains(s, "messages_v") {
		if bareTextRe.MatchString(stripSafeText(s)) {
			out = append(out, "messages.text")
		}
	}
	if docsTableRe.MatchString(s) && !strings.Contains(s, "docs_v") {
		if bareContentRe.MatchString(stripSafeContent(s)) {
			out = append(out, "docs.content")
		}
	}
	// A read of entries that touches raw or a generated column must go
	// through the view; id/session_id/type/timestamp-only reads (the GC
	// cursor, existence checks) are fine on the base table.
	// Token aggregates must read the base table's *_m columns so
	// idx_entries_*_m is usable — entries_v's COALESCE/mnemo_raw hides them.
	// A literal pinning an idx_entries_*_m index is a deliberate base-table
	// read of the materialised columns (a view cannot take INDEXED BY).
	//
	// The materialised-column test must be a WORD match. strings.Contains
	// on "_m" also matches the literal "session_meta", which most entries
	// queries join — that exempted nearly every query from this ratchet
	// and reopened the 🎯T152 hole it exists to hold shut (🎯T176).
	if entriesReadRe.MatchString(s) && entriesHotColsRe.MatchString(stripSafeEntries(s)) && !entriesDeleteRe.MatchString(s) &&
		!strings.Contains(s, "INDEXED BY idx_entries_") &&
		!(materialisedColRe.MatchString(s) && !entriesReadsRaw(s)) {
		out = append(out, "entries (use entries_v)")
	}
	return out
}

var safeTextRe = regexp.MustCompile(`(?i)mnemo_text\(\s*(\w+\.)?text\s*,\s*(\w+\.)?text_z\s*\)|\btext_z\b|\btext_\w+|\w+_text\b|'[^']*'|content_type\s*=\s*'text'|messages_fts\b|\btext\s*:|\bAS\s+text\b`)

// aliasedTextRe matches a qualified reference such as ex.text, which is a
// read of a subquery alias, not of the base column, when the literal
// itself defines the alias with "AS text".
var aliasedTextRe = regexp.MustCompile(`(?i)\b\w+\.text\b`)

var asTextRe = regexp.MustCompile(`(?i)\bAS\s+text\b`)

func stripSafeText(s string) string {
	aliased := asTextRe.MatchString(s)
	s = safeTextRe.ReplaceAllString(s, " ")
	if aliased {
		s = aliasedTextRe.ReplaceAllString(s, " ")
	}
	return s
}

var safeContentRe = regexp.MustCompile(`(?i)mnemo_text\(\s*(\w+\.)?content\s*,\s*(\w+\.)?content_z\s*\)|\bcontent_z\b|\bcontent_\w+|\w+_content\b|'[^']*'|docs_fts\b`)

func stripSafeContent(s string) string { return safeContentRe.ReplaceAllString(s, " ") }

// mnemo_raw(raw, raw_z) is the documented decoder; *_m columns are the
// hot-path source of truth (token aggregates must not go through the view).
// materialisedColRe matches a materialised twin as a WORD — foo_m, not
// any string containing "_m" such as session_meta (🎯T176).
var materialisedColRe = regexp.MustCompile(`\b\w+_m\b`)

var safeEntriesRe = regexp.MustCompile(`(?i)mnemo_raw\(\s*(\w+\.)?raw\s*,\s*(\w+\.)?raw_z\s*\)|\braw_z\b|\b\w+_m\b`)

var entriesAliasRe = regexp.MustCompile(`(?i)\bAS\s+(raw|uuid|model|stop_reason|input_tokens|output_tokens|cache_read_tokens|cache_creation_tokens|agent_id|version|slug|is_sidechain|data_type|data_command|data_hook_event|top_tool_use_id|parent_tool_use_id)\b`)

// entriesReadsRaw reports a raw-column read that is not mnemo_raw/raw_z.
func entriesReadsRaw(s string) bool {
	s = regexp.MustCompile(`(?i)mnemo_raw\(\s*(\w+\.)?raw\s*,\s*(\w+\.)?raw_z\s*\)|\braw_z\b`).ReplaceAllString(s, " ")
	return regexp.MustCompile(`(?i)\braw\b`).MatchString(s)
}

func stripSafeEntries(s string) string {
	// Record aliases before stripping: `model_m AS model` plus an outer
	// `e.model` in the same literal is the CTE name, not the generated
	// column. A single ReplaceAll cannot see `AS model` after `model_m`
	// has already been removed.
	aliased := map[string]bool{}
	for _, m := range entriesAliasRe.FindAllStringSubmatch(s, -1) {
		if len(m) > 1 {
			aliased[strings.ToLower(m[1])] = true
		}
	}
	s = safeEntriesRe.ReplaceAllString(s, " ")
	s = entriesAliasRe.ReplaceAllString(s, " ")
	for col := range aliased {
		s = regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(col)+`\b`).ReplaceAllString(s, " ")
	}
	return s
}

// bareSnippet returns the text around the first bare reference, for the
// failure message.
func bareSnippet(s, col string) string {
	var loc []int
	if col == "messages.text" {
		loc = bareTextRe.FindStringIndex(stripSafeText(s))
	} else if strings.HasPrefix(col, "entries") {
		loc = entriesReadRe.FindStringIndex(s)
	} else {
		loc = bareContentRe.FindStringIndex(stripSafeContent(s))
	}
	if loc == nil {
		return ""
	}
	a, b := loc[0]-30, loc[1]+30
	if a < 0 {
		a = 0
	}
	if b > len(s) {
		b = len(s)
	}
	return s[a:b]
}

// TestRatchetIsNotDisarmedBySessionMetaJoin is the regression test for
// 🎯T176.
//
// The exemption for deliberate base-table reads of the materialised
// columns was `strings.Contains(s, "_m")`. "session_meta" contains "_m",
// and most entries queries join it — so the substring test exempted
// nearly every query in the tree and the ratchet silently stopped
// guarding the 🎯T152 hole it exists for.
func TestRatchetIsNotDisarmedBySessionMetaJoin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sql     string
		flagged bool
		why     string
	}{
		{
			name: "generated column, joined to session_meta",
			sql: `SELECT e.model, e.input_tokens FROM entries e
			      LEFT JOIN session_meta sm ON sm.session_id = e.session_id`,
			flagged: true,
			why:     "model/input_tokens are NULL on packed rows; the session_meta join must not exempt this",
		},
		{
			name: "materialised twins, joined to session_meta",
			sql: `SELECT e.model_m, e.input_tokens_m FROM entries e
			      LEFT JOIN session_meta sm ON sm.session_id = e.session_id`,
			flagged: false,
			why:     "reading the twins on the base table is the intended token-aggregate form",
		},
		{
			name:    "generated column, no join at all",
			sql:     `SELECT e.input_tokens FROM entries e WHERE e.session_id = ?`,
			flagged: true,
			why:     "the plain case the ratchet has always caught",
		},
		{
			name:    "session_meta alone, no entries columns",
			sql:     `SELECT sm.repo FROM session_meta sm`,
			flagged: false,
			why:     "not an entries read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := bareColumnReads(tc.sql)
			var flagged bool
			for _, g := range got {
				if strings.HasPrefix(g, "entries") {
					flagged = true
				}
			}
			if flagged != tc.flagged {
				t.Errorf("flagged = %v, want %v — %s", flagged, tc.flagged, tc.why)
			}
		})
	}
}
