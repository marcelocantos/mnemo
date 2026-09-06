// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package compact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ReplayEnv gates the live replay. The harness spawns the real claude
// binary once per span against the owner's account, so it runs only when
// asked for. It is the oracle for the bare-spawn change: a token profile
// nobody paid for proves nothing.
const ReplayEnv = "MNEMO_REPLAY_LIVE"

// replaySpanTimeout bounds one span. Production summarisation of a ~90KB
// transcript lands near 30s; three minutes is generous without letting a
// wedged spawn hold the suite.
const replaySpanTimeout = 3 * time.Minute

// minFileRecall is the fraction of the production run's cited files the
// bare-spawn run must also cite, averaged over the sample.
//
// It is not 1.0 because the two runs are separate stochastic samples of
// the same model on the same input: file lists differ at the margin
// between any two runs, including two runs of the unchanged code. The
// bar is set to catch a real regression — the summariser losing its grip
// on the transcript because context it needed was removed — not to pin
// run-to-run noise. The claim this test supports is "quality holds", so
// the number that matters is the aggregate, reported either way.
const minFileRecall = 0.5

// minParseRate is the fraction of spans that must yield a parseable
// payload.
//
// Not 1.0, because a summariser turn that returns something other than a
// payload is an expected outcome of the production path too, not a
// failure mode this change introduced: Compact treats ErrNoPayload as a
// soft result (🎯T77) and does not even quarantine it (🎯T163). Measured
// over this sample, 19/20 parsed on the bare arm and the one miss parsed
// cleanly on a re-run of the same span — stochastic, not structural. The
// floor catches a real break (a spawn that returns prose, an empty
// system prompt reinstating Claude Code's own persona) without failing
// the suite on the model's ordinary variance.
const minParseRate = 0.9

// ReplayArmEnv selects which spawn path the replay exercises: "bare"
// (default, claudecli) or "claudia" (the pre-change claudia.Task path).
// Both arms run the same frozen spans, so the before/after is a measured
// comparison on identical input rather than an extrapolation.
const ReplayArmEnv = "MNEMO_REPLAY_ARM"

// replayCase is one span of the frozen sample: the exact user prompt
// production sent, and the payload production got back.
type replayCase struct {
	name   string
	prompt string
	before Payload
}

func loadReplayCases(t *testing.T) []replayCase {
	t.Helper()
	prompts, err := filepath.Glob(filepath.Join("testdata", "replay", "*.prompt.txt"))
	if err != nil || len(prompts) == 0 {
		t.Fatalf("no replay sample under testdata/replay (glob err %v)", err)
	}
	sort.Strings(prompts)
	cases := make([]replayCase, 0, len(prompts))
	for _, p := range prompts {
		name := strings.TrimSuffix(filepath.Base(p), ".prompt.txt")
		prompt, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		beforeRaw, err := os.ReadFile(filepath.Join("testdata", "replay", name+".before.json"))
		if err != nil {
			t.Fatalf("read before payload for %s: %v", name, err)
		}
		var before Payload
		if err := json.Unmarshal(beforeRaw, &before); err != nil {
			t.Fatalf("parse before payload for %s: %v", name, err)
		}
		cases = append(cases, replayCase{name: name, prompt: string(prompt), before: before})
	}
	return cases
}

// recall is the fraction of want present in got, compared case- and
// path-insensitively on the base name: the two runs cite the same file
// with and without its directory often enough that comparing full strings
// measures formatting, not comprehension.
func recall(got, want []string) float64 {
	if len(want) == 0 {
		return 1
	}
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[strings.ToLower(filepath.Base(strings.TrimSpace(g)))] = true
	}
	var hits int
	for _, w := range want {
		if have[strings.ToLower(filepath.Base(strings.TrimSpace(w)))] {
			hits++
		}
	}
	return float64(hits) / float64(len(want))
}

// TestReplayBareSpawnHoldsQuality runs the frozen 20-span sample through
// the bare-spawn caller and reports, per span, what it cost and how much
// of the production run's content it reproduced.
//
// This is the acceptance oracle for replacing claudia.Task with claudecli
// in the summariser. The cost half of the claim is measured here on real
// spans rather than extrapolated from a probe, and the quality half is
// what stops the cost half from being won by summarising badly.
func TestReplayBareSpawnHoldsQuality(t *testing.T) {
	if os.Getenv(ReplayEnv) == "" {
		t.Skipf("live replay: set %s=1 (spawns claude once per span, spends real money)", ReplayEnv)
	}
	cases := loadReplayCases(t)
	workDir := t.TempDir() // no CLAUDE.md ancestors: see TestBareSpawnWorkDirMustBeNeutral
	opts := ClaudiaCallerOpts{WorkDir: workDir, Provider: "claude", Model: DefaultClaudeModel}

	arm := os.Getenv(ReplayArmEnv)
	var caller LLMCaller
	switch arm {
	case "", "bare":
		arm = "bare"
		caller = NewSummariserCaller(opts)
	case "claudia":
		caller = NewClaudiaCaller(opts)
	default:
		t.Fatalf("%s=%q: want bare or claudia", ReplayArmEnv, arm)
	}
	t.Logf("arm=%s spans=%d", arm, len(cases))

	var (
		totalPrompt, totalOutput int
		totalCost, totalRecall   float64
		parsed                   int
	)
	for _, tc := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), replaySpanTimeout)
		res, err := caller.Call(ctx, SystemPrompt, tc.prompt)
		cancel()
		if err != nil {
			t.Errorf("%s: call: %v", tc.name, err)
			continue
		}
		totalPrompt += res.PromptTokens
		totalOutput += res.OutputTokens
		totalCost += res.CostUSD

		payload, _, err := parsePayload(res.Text)
		if err != nil {
			t.Errorf("%s: no payload from %d prompt tokens: %v", tc.name, res.PromptTokens, err)
			continue
		}
		parsed++
		if strings.TrimSpace(payload.Summary) == "" {
			t.Errorf("%s: empty summary", tc.name)
		}
		r := recall(payload.Files, tc.before.Files)
		totalRecall += r
		t.Logf("%s: prompt=%d output=%d cost=$%.4f file_recall=%.2f threads=%d/%d decisions=%d/%d",
			tc.name, res.PromptTokens, res.OutputTokens, res.CostUSD, r,
			len(payload.OpenThreads), len(tc.before.OpenThreads),
			len(payload.Decisions), len(tc.before.Decisions))
	}

	if parsed == 0 {
		t.Fatal("no span produced a payload")
	}
	avgRecall := totalRecall / float64(parsed)
	parseRate := float64(parsed) / float64(len(cases))
	t.Logf("SUMMARY arm=%s spans=%d parsed=%d prompt_tokens=%d output_tokens=%d cost=$%.4f avg_prompt=%d avg_file_recall=%.2f",
		arm, len(cases), parsed, totalPrompt, totalOutput, totalCost, totalPrompt/parsed, avgRecall)

	if parseRate < minParseRate {
		t.Errorf("payload parsed for %d/%d spans (%.2f < %.2f)", parsed, len(cases), parseRate, minParseRate)
	}
	if avgRecall < minFileRecall {
		t.Errorf("avg file recall %.2f < %.2f: the bare spawn is losing content the full spawn kept", avgRecall, minFileRecall)
	}
}

// TestBareSpawnWorkDirMustBeNeutral pins the one thing --setting-sources
// "" does not cover.
//
// Measured while building this change: the same bare-spawn argv billed
// 559 prompt tokens from /tmp/mnemo-summariser and 3,319 from a directory
// with a CLAUDE.md above it. Project instructions are discovered from the
// cwd, not from the settings sources, so the saving depends on the
// summariser's working directory staying free of them — a property of
// main.summariserWorkDir that nothing else asserts.
func TestBareSpawnWorkDirMustBeNeutral(t *testing.T) {
	dir := t.TempDir()
	for d := dir; ; d = filepath.Dir(d) {
		for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				t.Fatalf("%s found at %s: a bare spawn from here would re-inject project instructions", name, d)
			}
		}
		if d == filepath.Dir(d) {
			break
		}
	}
}
