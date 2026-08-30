// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// TestConfigDocsCoverEveryKey is the anti-drift ratchet for 🎯T156.
//
// Configuration is file-only, so `mnemo --help-config` is the whole
// interface. The key list is reflected from Config, but the prose is
// written by hand, and hand-written prose beside a struct is exactly
// what drifted before: the tool this replaced validated patches against
// a hand-maintained key list, and menu_bar_app and threads_root became
// unpatchable when they were added to the struct but not the list.
func TestConfigDocsCoverEveryKey(t *testing.T) {
	keys := ConfigKeys()
	if len(keys) == 0 {
		t.Fatal("no config keys reflected from Config — the json tags moved")
	}
	declared := map[string]bool{}
	for _, k := range keys {
		declared[k] = true
		d, ok := configDocs[k]
		if !ok {
			t.Errorf("config key %q has no entry in configDocs; document it (and say whether it hot-reloads)", k)
			continue
		}
		if strings.TrimSpace(d.summary) == "" {
			t.Errorf("config key %q has an empty summary", k)
		}
	}
	for k := range configDocs {
		if !declared[k] {
			t.Errorf("configDocs documents %q, which Config no longer declares — stale entry", k)
		}
	}
}

// TestConfigSchemaDocRenders: every key appears in the rendered help,
// under a heading that matches its declared reload behaviour.
func TestConfigSchemaDocRenders(t *testing.T) {
	doc := ConfigSchemaDoc("/tmp/config.json")
	for _, k := range ConfigKeys() {
		if !strings.Contains(doc, k) {
			t.Errorf("rendered help omits %q", k)
		}
	}
	live := strings.Index(doc, "Adopted live")
	restart := strings.Index(doc, "Require a daemon restart")
	if live < 0 || restart < 0 || live > restart {
		t.Fatalf("both sections must render, live first: live=%d restart=%d", live, restart)
	}
	// linked_instances is the canonical restart-only key.
	if idx := strings.Index(doc, "linked_instances"); idx < restart {
		t.Error("linked_instances must be documented under the restart section")
	}
}

// TestDefaultRetentionIsOne guards the retention default (🎯T158). At
// the previous default of 7 the backup directory held ~81 GB against an
// 18.9 GB database — 4.3x the data it protects, and scaling with it. A
// silent revert would multiply steady-state disk by seven with nothing
// failing, which is why the number is pinned rather than merely
// documented.
func TestDefaultRetentionIsOne(t *testing.T) {
	if got := (BackupConfig{}).EffectiveKeepDailies(); got != 1 {
		t.Errorf("default keep_dailies = %d, want 1", got)
	}
	// An explicit value still wins, so more history stays one edit away.
	if got := (BackupConfig{KeepDailies: 5}).EffectiveKeepDailies(); got != 5 {
		t.Errorf("explicit keep_dailies = %d, want 5", got)
	}
}
