// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestQueryTimeoutCancelsSlowRead drives Store.Query with a deliberately
// expensive recursive CTE under a short budget (🎯T74).
func TestQueryTimeoutCancelsSlowRead(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	s.SetQueryTimeoutForTest(200 * time.Millisecond)

	// Recursive CTE that would run for a long time without interrupt.
	q := `
WITH RECURSIVE r(i) AS (
  SELECT 1
  UNION ALL
  SELECT i+1 FROM r WHERE i < 100000000
)
SELECT count(*) AS n FROM r
`
	start := time.Now()
	_, err := s.Query(q)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !IsQueryTimeout(err) && !errors.Is(err, ErrQueryTimeout) {
		// Accept either typed wrap or message containing budget guidance.
		if !strings.Contains(err.Error(), "budget") && !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("error not timeout-shaped: %v", err)
		}
	}
	if !strings.Contains(err.Error(), "narrow the query") {
		t.Errorf("timeout error should guide agent to refine: %v", err)
	}
	// Must return well under a hang; allow generous margin for CI load.
	if elapsed > 3*time.Second {
		t.Fatalf("query took %v, want cancel near 200ms budget", elapsed)
	}
	// Connection returned cleanly: a subsequent simple query must work.
	rows, err := s.Query(`SELECT 1 AS ok`)
	if err != nil {
		t.Fatalf("follow-up query after timeout: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("follow-up rows=%d", len(rows))
	}
}

func TestQueryFastPathUnaffected(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	rows, err := s.Query(`SELECT 42 AS n`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
}
