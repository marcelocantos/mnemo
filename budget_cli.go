// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/marcelocantos/mnemo/internal/api"
)

// cmdBudget implements `mnemo budget` — read spend + throttle without MCP
// (🎯T140). Delegates to the daemon like `mnemo resume` so a single budget
// payload serves CLI, dashboard, and menubar.
func cmdBudget(args []string) {
	fs := flag.NewFlagSet("budget", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print raw JSON from GET /api/budget")
	trees := fs.Bool("trees", true, "include agent trees in the request (default true)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: mnemo budget [--json] [--trees=false]

Print monthly budget, projection, throttle state, and top agent trees
from the running mnemo daemon (GET /api/budget). No MCP client required.

  --json         emit the API JSON payload
  --trees=false  omit agent_trees (lighter)

MCP home (🎯T140 decision): spend/throttle for agents is also available as
mnemo_ops op=budget (and op=agent_trees). CLI + dashboard remain the
primary human surfaces.
`)
	}
	_ = fs.Parse(args)

	url := daemonBaseURL() + "/api/budget"
	if !*trees {
		url += "?trees=0"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "budget: cannot reach mnemo daemon at %s: %v\n", daemonBaseURL(), err)
		fmt.Fprintln(os.Stderr, "is it running? start it with `brew services start mnemo`.")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "budget: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	if *jsonOut {
		os.Stdout.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			fmt.Println()
		}
		return
	}

	var snap api.BudgetSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		fmt.Fprintf(os.Stderr, "budget: decode: %v\n", err)
		os.Exit(1)
	}
	printBudgetHuman(snap)
}

func printBudgetHuman(snap api.BudgetSnapshot) {
	b := snap.Budget
	if b == nil {
		fmt.Println("budget: no budget data")
	} else {
		fmt.Printf("Budget period %s (%s → %s)\n",
			b.Period.Label,
			b.Period.Start.Format("2006-01-02"),
			b.Period.End.Format("2006-01-02"))
		fmt.Printf("  cap        $%.2f\n", b.CapUSD)
		fmt.Printf("  spent      $%.2f (%.0f%%)  elapsed %.0f%% of period\n",
			b.SpentUSD, b.SpentPct, b.ElapsedPct)
		fmt.Printf("  projected  $%.2f (%.0f%%)  burn $%.2f/day over %dd\n",
			b.ProjectedUSD, b.ProjectedPct, b.BurnUSDPerDay, b.BurnWindowDays)
		if b.ExhaustionDate != "" {
			fmt.Printf("  exhaustion %s\n", b.ExhaustionDate)
		}
		fmt.Printf("  severity   %s\n", b.Severity)
		fmt.Printf("  governed   $%.2f (%.0f%% of spent — mnemo's own agents)\n",
			b.GovernedUSD, b.GovernedPct)
		fmt.Printf("  priced     %v\n", b.Priced)
		if b.Headline != "" {
			fmt.Printf("  %s\n", b.Headline)
		}
	}
	th := snap.Throttle
	fmt.Printf("Throttle: %s", th.Level)
	if th.Throttling {
		fmt.Print(" (ACTIVE)")
	}
	fmt.Println()
	if th.Detail != "" {
		fmt.Printf("  %s\n", th.Detail)
	}
	if th.Remediation != "" {
		fmt.Printf("  lifts: %s\n", th.Remediation)
	}
	if len(snap.Trees) > 0 {
		fmt.Println("Top agent trees:")
		for i, tr := range snap.Trees {
			if i >= 5 {
				break
			}
			fmt.Printf("  $%.2f  agents=%d  %s  %s\n",
				tr.TreeCostUSD, tr.Agents, tr.Skill, tr.Repo)
		}
	}
}
