// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const (
	// anthropicAdminAPIKeyEnv is the environment variable holding the
	// Anthropic Admin API key used for cost reconciliation. The admin
	// key is distinct from the regular ANTHROPIC_API_KEY — it requires
	// elevated permissions granted via the Anthropic console.
	anthropicAdminAPIKeyEnv = "ANTHROPIC_ADMIN_API_KEY"

	// anthropicCostReportURL is the Anthropic Admin API endpoint for
	// per-day cost figures. Requires an admin API key.
	// Response shape (best-effort, verify against live key):
	//   { "data": [{ "date": "YYYY-MM-DD", "cost_usd": 1.23 }, ...] }
	anthropicCostReportURL = "https://api.anthropic.com/v1/organizations/cost_report"

	// reconcilerInterval is the minimum interval between cost-report polls.
	// Anthropic's documented sustained polling rate is once per minute.
	reconcilerInterval = time.Minute
)

// costReportResponse models the Anthropic Admin API cost_report response.
// This is a best-effort model based on the documented endpoint shape.
// Fields are preserved for forward-compatibility.
type costReportResponse struct {
	Data []costReportEntry `json:"data"`
}

type costReportEntry struct {
	// Date is the UTC calendar date in YYYY-MM-DD format.
	Date string `json:"date"`
	// CostUSD is the authoritative daily cost in US dollars.
	CostUSD float64 `json:"cost_usd"`
	// Some API responses may use "total_cost" instead; keep both.
	TotalCost float64 `json:"total_cost"`
}

// StartReconciler launches a background goroutine that polls the Anthropic
// Admin API for authoritative daily costs and stores them via
// UpsertReconciledCost. It stops when ctx is cancelled.
//
// If ANTHROPIC_ADMIN_API_KEY is not set, a one-time warning is logged and
// the reconciler exits immediately without affecting normal operation.
func (s *Store) StartReconciler(ctx context.Context) {
	apiKey := os.Getenv(anthropicAdminAPIKeyEnv)
	if apiKey == "" {
		slog.Warn("cost reconciliation disabled: ANTHROPIC_ADMIN_API_KEY not set; " +
			"usage rows will report source=estimated")
		return
	}

	go func() {
		// Poll once immediately on startup, then on the interval ticker.
		if err := s.reconcileOnce(apiKey); err != nil {
			slog.Warn("cost reconciliation failed", "err", err)
		}

		ticker := time.NewTicker(reconcilerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.reconcileOnce(apiKey); err != nil {
					slog.Warn("cost reconciliation failed", "err", err)
				}
			}
		}
	}()
}

// reconcileOnce fetches the cost report for the last 30 days and upserts
// each day's authoritative cost into the reconciled_costs table.
func (s *Store) reconcileOnce(apiKey string) error {
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -30)

	url := fmt.Sprintf("%s?start_date=%s&end_date=%s",
		anthropicCostReportURL,
		start.Format("2006-01-02"),
		end.Format("2006-01-02"),
	)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Truncate body to avoid log spam on auth errors.
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return fmt.Errorf("cost report API returned %d: %s", resp.StatusCode, preview)
	}

	var report costReportResponse
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	var upserted int
	for _, entry := range report.Data {
		if entry.Date == "" {
			continue
		}
		// Prefer CostUSD; fall back to TotalCost if the API uses that field name.
		cost := entry.CostUSD
		if cost == 0 && entry.TotalCost != 0 {
			cost = entry.TotalCost
		}
		if err := s.UpsertReconciledCost(entry.Date, cost); err != nil {
			slog.Warn("failed to upsert reconciled cost", "date", entry.Date, "err", err)
			continue
		}
		upserted++
	}

	slog.Debug("cost reconciliation complete", "entries", upserted)
	return nil
}
