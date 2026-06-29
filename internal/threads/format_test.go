// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package threads

import (
	"testing"
	"time"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{0, "today"},
		{2 * time.Hour, "today"},
		{26 * time.Hour, "yesterday"},
		{3 * 24 * time.Hour, "3 days ago"},
		{10 * 24 * time.Hour, "1 week ago"},
		{20 * 24 * time.Hour, "2 weeks ago"},
		{60 * 24 * time.Hour, "2 months ago"},
	}
	for _, c := range cases {
		got := RelativeTime(now, now.Add(-c.ago))
		if got != c.want {
			t.Errorf("RelativeTime(-%v) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestCompactAge(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Second, "now"},
		{12 * time.Minute, "12m"},
		{3 * time.Hour, "3h"},
		{2 * 24 * time.Hour, "2d"},
		{3 * 7 * 24 * time.Hour, "3w"},
	}
	for _, c := range cases {
		got := CompactAge(now, now.Add(-c.ago))
		if got != c.want {
			t.Errorf("CompactAge(-%v) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestActivitySummary(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		th   Thread
		want string
	}{
		{Thread{}, "empty"},
		{Thread{FileCount: 3, HasActivity: true, Activity: now}, "3 files, today"},
		{Thread{FileCount: 1, HasActivity: true, Activity: now.Add(-26 * time.Hour)}, "1 file, yesterday"},
		{Thread{FileCount: 2}, "2 files"},
		{Thread{HasActivity: true, Activity: now}, "today"},
	}
	for _, c := range cases {
		if got := c.th.ActivitySummary(now); got != c.want {
			t.Errorf("ActivitySummary(%+v) = %q, want %q", c.th, got, c.want)
		}
	}
}
