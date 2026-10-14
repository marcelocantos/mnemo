// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestIdentifyCallerSources(t *testing.T) {
	lookup := func(id string) (string, error) {
		if id == "toolu_knownlookup" {
			return "sess-from-tool", nil
		}
		return "", nil
	}
	cases := []struct {
		name   string
		meta   map[string]any
		header http.Header
		want   string
		how    string
	}{
		{"meta", map[string]any{"claudecode/sessionId": "sess-from-meta"}, nil, "sess-from-meta", "meta"},
		{"nested", map[string]any{"client": map[string]any{"cursor/sessionId": "sess-nested"}}, nil, "sess-nested", "meta"},
		{"header", nil, http.Header{"X-Claude-Code-Session-Id": []string{"sess-header"}}, "sess-header", "header"},
		{"baggage", nil, http.Header{"Baggage": []string{"claude.session_id=sess-bag-1, other=no"}}, "sess-bag-1", "baggage"},
		{"tool", map[string]any{"claudecode/toolUseId": "toolu_knownlookup"}, nil, "sess-from-tool", "tool_use"},
		{"meta beats tool", map[string]any{"claudecode/sessionId": "sess-explicit", "claudecode/toolUseId": "toolu_knownlookup"}, nil, "sess-explicit", "meta"},
		{"short id rejected", map[string]any{"sessionId": "abc"}, nil, "", ""},
		{"unknown", map[string]any{"progressToken": "123"}, nil, "", ""},
		{"transport header ignored", nil, http.Header{"Mcp-Session-Id": []string{"mcp-session-id-value"}}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, how := IdentifyCaller(tc.meta, tc.header, lookup)
			if got != tc.want || how != tc.how {
				t.Fatalf("IdentifyCaller = %q %q, want %q %q", got, how, tc.want, tc.how)
			}
		})
	}
}

func TestRequestMetaPrefersCallParams(t *testing.T) {
	req := mcp.CallToolRequest{}
	req.Request.Params.Meta = &mcp.Meta{AdditionalFields: map[string]any{"sessionId": "from-request"}}
	req.Params.Meta = &mcp.Meta{AdditionalFields: map[string]any{"sessionId": "from-params"}}
	meta := requestMeta(req)
	if meta["sessionId"] != "from-params" {
		t.Fatalf("meta = %v, want the CallToolParams value", meta)
	}
}

func TestSearchExcludesCallerAndOptsIn(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	s, err := store.New(dbPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AwaitStartup()

	db, err := sql.Open("sqlite3_mnemo_rw", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const caller = "callerS1aaaaaaaa"
	const other = "otherS2bbbbbbbb"
	const token = "zqxsharedhit"
	pad := strings.Repeat("padding word ", 40)
	insert := func(session, text string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO messages (session_id, project, role, text, timestamp, type, is_noise, content_type)
			VALUES (?, 'workbench', 'user', ?, '2026-01-01T00:00:00Z', 'user', 0, 'text')`, session, text); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ {
		insert(caller, token)
	}
	insert(other, token+" "+pad)

	h := NewHandler(func(string) (store.Backend, error) { return s, nil })
	call := func(include bool) string {
		t.Helper()
		args := map[string]any{
			"query": token, "limit": float64(10), "kinds": "message",
			"context_before": float64(0), "context_after": float64(0),
			"session_type": "all",
		}
		if include {
			args["include_caller"] = true
		}
		text, isErr, err := h.Call(context.Background(), CallContext{
			Meta: map[string]any{"claudecode/sessionId": caller},
		}, "mnemo_search", args)
		if err != nil || isErr {
			t.Fatalf("search err=%v isErr=%v text=%s", err, isErr, text)
		}
		return text
	}
	excluded := call(false)
	if strings.Contains(excluded, caller[:8]) {
		t.Fatalf("caller session leaked into results:\n%s", excluded)
	}
	if !strings.Contains(excluded, other[:8]) {
		t.Fatalf("other session missing when the caller is excluded:\n%s", excluded)
	}
	if !strings.Contains(excluded, "excluded") {
		t.Fatalf("result did not say the caller was excluded:\n%s", excluded)
	}
	included := call(true)
	if !strings.Contains(included, caller[:8]) {
		t.Fatalf("include_caller did not bring the caller back:\n%s", included)
	}
}

func TestSearchToolDocumentsCallerExclusion(t *testing.T) {
	var desc string
	for _, tool := range Definitions() {
		if tool.Name == "mnemo_search" {
			desc = tool.Description
		}
	}
	for _, want := range []string{"include_caller", "CALLING SESSION", "Default kinds:", "before the per-corpus candidate limit"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q", want)
		}
	}
	found := false
	for _, tool := range Definitions() {
		if tool.Name == "mnemo_search" {
			if _, ok := tool.InputSchema.Properties["include_caller"]; ok {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("include_caller is not a parameter")
	}
}
