// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package hours

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func at(min float64) time.Time { return t0.Add(time.Duration(min * float64(time.Minute))) }

func minuteOf(min int64) int64 { return t0.Unix()/60 + min }

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.011 {
		t.Errorf("%s = %.3f h, want %.3f h", what, got, want)
	}
}

func day() (time.Time, time.Time) { return t0.Add(-9 * time.Hour), t0.Add(15 * time.Hour) }

// Five threads prompted in the same stretch are one stretch of the
// owner's attention, not five.
func TestParallelThreadsCountOnce(t *testing.T) {
	var in []Input
	repos := []string{"a", "b", "c", "d", "e"}
	for i := range 60 { // a prompt a minute for an hour, round-robin
		in = append(in, Input{At: at(float64(i)), Session: repos[i%5], Repo: repos[i%5]})
	}
	lo, hi := day()
	res := Compute(DefaultParams(), lo, hi, time.UTC, in, nil)
	// 59 minutes of prompts, plus 2 lead and 5 tail.
	near(t, "attended", res.AttendedHours, 66.0/60)
	if res.Blocks != 1 || res.Inputs != 60 {
		t.Errorf("blocks=%d inputs=%d, want 1 and 60", res.Blocks, res.Inputs)
	}
	var sum float64
	for _, r := range res.ByRepo {
		sum += r.AttendedHours
	}
	near(t, "sum of projects", sum, res.AttendedHours)
}

// A block's attribution sums to the block, and each stretch goes to the
// prompt it led up to.
func TestBlockAttributionSumsToBlock(t *testing.T) {
	in := []Input{
		{At: at(0), Session: "s1", Repo: "alpha"},
		{At: at(10), Session: "s2", Repo: "beta"}, // the 10 minutes before go to beta
		{At: at(12), Session: "s1", Repo: "alpha"},
	}
	lo, hi := day()
	res := Compute(DefaultParams(), lo, hi, time.UTC, in, nil)
	near(t, "attended", res.AttendedHours, 19.0/60) // 2 lead + 12 + 5 tail
	got := map[string]float64{}
	for _, r := range res.ByRepo {
		got[r.Repo] = r.AttendedHours
	}
	near(t, "alpha", got["alpha"], 9.0/60) // lead 2 + 2 before the third prompt + tail 5
	near(t, "beta", got["beta"], 10.0/60)
	near(t, "alpha+beta", got["alpha"]+got["beta"], res.AttendedHours)
}

// Inputs further apart than Gap are separate blocks, and the silence
// between them is not attended.
func TestGapSplitsBlocks(t *testing.T) {
	in := []Input{{At: at(0), Repo: "a"}, {At: at(16), Repo: "a"}}
	lo, hi := day()
	res := Compute(DefaultParams(), lo, hi, time.UTC, in, nil)
	if res.Blocks != 2 {
		t.Fatalf("blocks = %d, want 2", res.Blocks)
	}
	near(t, "attended", res.AttendedHours, 14.0/60) // two blocks of 2 + 5
}

// An agent that finished and then sat waiting for the owner was not
// working: bridging stops at a human prompt, and the agent's time
// resumes from the prompt.
func TestAgentBridgingStopsAtHumanPrompt(t *testing.T) {
	mins := []AgentMinute{
		{Session: "s", Repo: "a", Minute: minuteOf(0)},
		{Session: "s", Repo: "a", Minute: minuteOf(1)},
		// quiet 2..5, prompt at 4.5, output again at 5
		{Session: "s", Repo: "a", Minute: minuteOf(5)},
	}
	lo, hi := day()
	without := Compute(DefaultParams(), lo, hi, time.UTC, nil, mins)
	near(t, "bridged agent time", without.AgentHours, 6.0/60)

	prompt := []Input{{At: at(4.5), Session: "s", Repo: "a"}}
	with := Compute(DefaultParams(), lo, hi, time.UTC, prompt, mins)
	near(t, "agent time with a prompt in the gap", with.AgentHours, 2.0/60+1.5/60)
}

// A prompt in another session does not stop this session's bridging.
func TestOtherSessionsPromptDoesNotStopBridging(t *testing.T) {
	mins := []AgentMinute{
		{Session: "s", Repo: "a", Minute: minuteOf(0)},
		{Session: "s", Repo: "a", Minute: minuteOf(5)},
	}
	prompt := []Input{{At: at(3), Session: "other", Repo: "b"}}
	lo, hi := day()
	res := Compute(DefaultParams(), lo, hi, time.UTC, prompt, mins)
	near(t, "agent", res.AgentHours, 6.0/60)
}

// Parallel agents on one project count once for that project, and
// agent time while the owner was away is reported apart.
func TestAgentUnionAndUnattended(t *testing.T) {
	var mins []AgentMinute
	for m := range int64(60) {
		mins = append(mins, AgentMinute{Session: "s1", Repo: "a", Minute: minuteOf(m)})
		mins = append(mins, AgentMinute{Session: "s2", Repo: "a", Minute: minuteOf(m)})
	}
	// The owner attends 0..15 (prompts at 2 and 10, lead 2, tail 5).
	in := []Input{{At: at(2), Session: "s1", Repo: "a"}, {At: at(10), Session: "s1", Repo: "a"}}
	lo, hi := day()
	res := Compute(DefaultParams(), lo, hi, time.UTC, in, mins)
	near(t, "agent", res.AgentHours, 1)
	near(t, "attended", res.AttendedHours, 15.0/60)
	near(t, "agent unattended", res.AgentUnattendedHours, 1-15.0/60)
	if len(res.ByRepo) != 1 {
		t.Fatalf("rows = %+v", res.ByRepo)
	}
	near(t, "repo agent", res.ByRepo[0].AgentHours, 1)
}

// Only the part of a block inside the range counts, and the day split
// follows the requested zone.
func TestRangeClipAndDaySplit(t *testing.T) {
	syd, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("no zoneinfo")
	}
	midnight := time.Date(2026, 11, 11, 0, 0, 0, 0, syd)
	in := []Input{
		{At: midnight.Add(-10 * time.Minute), Repo: "a"},
		{At: midnight.Add(5 * time.Minute), Repo: "a"},
	}
	res := Compute(DefaultParams(), midnight.Add(-24*time.Hour), midnight.Add(24*time.Hour), syd, in, nil)
	if len(res.ByDay) != 2 {
		t.Fatalf("days = %+v", res.ByDay)
	}
	near(t, "first day", res.ByDay[0].AttendedHours, 12.0/60)
	near(t, "second day", res.ByDay[1].AttendedHours, 10.0/60)

	clipped := Compute(DefaultParams(), midnight, midnight.Add(24*time.Hour), syd, in, nil)
	near(t, "clipped", clipped.AttendedHours, 10.0/60)
	if clipped.Inputs != 1 {
		t.Errorf("inputs in range = %d, want 1", clipped.Inputs)
	}
}

func TestSubtract(t *testing.T) {
	a := []interval{{0, 10}, {20, 30}}
	b := []interval{{2, 4}, {8, 22}, {25, 26}}
	got := subtract(a, b)
	want := []interval{{0, 2}, {4, 8}, {22, 25}, {26, 30}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
