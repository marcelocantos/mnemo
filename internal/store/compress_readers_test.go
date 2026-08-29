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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
	messagesTableRe = regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE|DELETE\s+FROM)\s+messages\b`)
	docsTableRe     = regexp.MustCompile(`(?i)\b(FROM|JOIN|UPDATE|DELETE\s+FROM)\s+docs\b`)
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
	// A literal pinning an idx_entries_*_m index is a deliberate base-table
	// read of the materialised columns (a view cannot take INDEXED BY).
	if entriesReadRe.MatchString(s) && entriesHotColsRe.MatchString(s) && !entriesDeleteRe.MatchString(s) &&
		!strings.Contains(s, "INDEXED BY idx_entries_") {
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
