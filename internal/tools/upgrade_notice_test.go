// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/upgrade"
)

func TestLocalHandlerInjectsUpgradeNoticeOnce(t *testing.T) {
	t.Parallel()
	tr := upgrade.NewNoticeTracker()
	tr.MarkSession("sess-1", "0.61.0", "0.62.0")

	// Resolve fails → Call returns a tool-level error string; notice
	// must still be prepended exactly once (🎯T97.6).
	h := NewHandler(func(username string) (store.Backend, error) {
		return nil, context.Canceled
	})
	h.SetUpgradeNotices(tr)

	handler := h.LocalHandler("mnemo_stats")
	req := mcp.CallToolRequest{}
	req.Header = http.Header{}
	req.Header.Set("Mcp-Session-Id", "sess-1")

	res, err := handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty result")
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	if !strings.HasPrefix(tc.Text, "mnemo upgraded v0.61.0 -> v0.62.0\n\n") {
		t.Fatalf("missing notice prefix: %q", tc.Text)
	}

	// Second call: notice consumed.
	res2, err := handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	tc2 := res2.Content[0].(mcp.TextContent)
	if strings.Contains(tc2.Text, "mnemo upgraded") {
		t.Fatalf("notice must be once-only: %q", tc2.Text)
	}
}
