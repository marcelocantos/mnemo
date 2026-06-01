// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"testing"
	"time"

	"github.com/marcelocantos/mnemo/internal/store"
)

func TestVaultLayoutRecommendation(t *testing.T) {
	soak := 720 * time.Hour
	cases := []struct {
		name        string
		layout      string
		hoursInBoth time.Duration
		v1Leftover  bool
		want        string
	}{
		// 🎯T64.2 state-machine table from the design doc.
		{"v1 — no rec ever", store.VaultLayoutV1, 0, false, ""},
		{"v2 — no leftovers, migration done", store.VaultLayoutV2, 0, false, ""},
		{"v2 — leftovers present, gc owed", store.VaultLayoutV2, 0, true, "run gc_legacy"},
		{"both — no first_seen yet, transient", store.VaultLayoutBoth, 0, false, ""},
		{"both — within soak", store.VaultLayoutBoth, 100 * time.Hour, false, "still within soak"},
		{"both — just under soak boundary", store.VaultLayoutBoth, soak - time.Hour, false, "still within soak"},
		{"both — exactly at soak boundary", store.VaultLayoutBoth, soak, false, "opt into v2"},
		{"both — past soak", store.VaultLayoutBoth, soak + 24*time.Hour, false, "opt into v2"},
		{"unknown layout — empty", "garbage", 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := vaultLayoutRecommendation(c.layout, c.hoursInBoth, soak, c.v1Leftover)
			if got != c.want {
				t.Errorf("recommendation: got %q want %q", got, c.want)
			}
		})
	}
}
