// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package upgrade

import "testing"

func TestIsNewer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"0.61.0", "v0.62.0", true},
		{"v0.62.0", "0.61.0", false},
		{"0.61.0", "0.61.0", false},
		{"v0.61.0", "v0.61.1", true},
		{"1.0.0", "0.99.0", false},
	}
	for _, tc := range cases {
		got, err := IsNewer(tc.cur, tc.lat)
		if err != nil {
			t.Fatalf("IsNewer(%q,%q): %v", tc.cur, tc.lat, err)
		}
		if got != tc.want {
			t.Errorf("IsNewer(%q,%q)=%v want %v", tc.cur, tc.lat, got, tc.want)
		}
	}
}
