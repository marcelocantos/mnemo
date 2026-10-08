// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestExcerptCentresOnTheMatch: a tool_output body is shown as a bounded
// window around the first query term, not as its head and not whole
// (🎯T192).
func TestExcerptCentresOnTheMatch(t *testing.T) {
	body := strings.Repeat("filler words here\n", 300) + "the needle zqxneedle appears late\n" + strings.Repeat("more filler\n", 300)
	got := excerpt(body, `"needle zqxneedle"`, 200)
	if !strings.Contains(got, "zqxneedle") {
		t.Fatalf("excerpt lost the match: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 200+2 {
		t.Errorf("excerpt is %d runes, want at most 202", n)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("a window cut from the middle should be marked on both ends: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("excerpt should be flattened to one line: %q", got)
	}

	// No term present: the head, bounded.
	head := excerpt(body, "absent", 50)
	if !strings.HasPrefix(head, "filler words") || !strings.HasSuffix(head, "…") || utf8.RuneCountInString(head) > 51 {
		t.Errorf("fallback should be the bounded head: %q", head)
	}
	// Short bodies pass through whole.
	if got := excerpt("short body", "body", 400); got != "short body" {
		t.Errorf("short body changed: %q", got)
	}
	// Operators are not terms.
	if got := excerpt(body, "AND OR NOT", 50); !strings.HasPrefix(got, "filler") {
		t.Errorf("operators were treated as terms: %q", got)
	}
}
