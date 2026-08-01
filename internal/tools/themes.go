// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marcelocantos/mnemo/internal/store"
)

// vaultRecluster implements mnemo_vault_recluster (🎯T64.8).
func (h *callHandler) vaultRecluster(args map[string]any, ctl ConfigController) (string, bool, error) {
	cfg := store.Config{}
	if ctl != nil {
		cfg = ctl.Get()
	}
	engine, _ := args["engine"].(string)
	force, _ := args["force_reembed"].(bool)
	ctx := h.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := h.mem.RunCluster(ctx, store.ClusterRunArgs{
		Config:         cfg.VaultClustering,
		Trigger:        "manual",
		EngineOverride: engine,
		ForceReembed:   force,
	})
	if err != nil {
		return fmt.Sprintf("recluster failed: %v", err), true, nil
	}
	buf, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(buf), false, nil
}

// vaultThemesInspect implements mnemo_vault_themes_inspect.
func (h *callHandler) vaultThemesInspect(args map[string]any) (string, bool, error) {
	ref, _ := args["theme"].(string)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "theme is required", true, nil
	}
	view, err := h.mem.InspectTheme(ref)
	if err != nil {
		return err.Error(), true, nil
	}
	buf, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return fmt.Sprintf("marshal failed: %v", err), true, nil
	}
	return string(buf), false, nil
}

// vaultThemesPin implements mnemo_vault_themes_pin.
func (h *callHandler) vaultThemesPin(args map[string]any) (string, bool, error) {
	ref, _ := args["theme"].(string)
	unpin, _ := args["unpin"].(bool)
	reason, _ := args["reason"].(string)
	if err := h.mem.PinTheme(ref, unpin, reason); err != nil {
		return err.Error(), true, nil
	}
	action := "pinned"
	if unpin {
		action = "unpinned"
	}
	return fmt.Sprintf(`{"theme":%q,"action":%q}`, ref, action), false, nil
}

// vaultThemesSplit is a config-rejecting stub that only records an override.
func (h *callHandler) vaultThemesSplit(args map[string]any) (string, bool, error) {
	ref, _ := args["theme"].(string)
	if strings.TrimSpace(ref) == "" {
		return "theme is required", true, nil
	}
	if err := h.mem.SetThemeOverride(ref, "split", `{}`); err != nil {
		return err.Error(), true, nil
	}
	return `{"status":"recorded","directive":"split","note":"split application ships in a follow-up; override stored for next pass"}`, false, nil
}

// vaultThemesMerge is a config-rejecting stub that only records an override.
func (h *callHandler) vaultThemesMerge(args map[string]any) (string, bool, error) {
	ref, _ := args["theme"].(string)
	with, _ := args["with"].(string)
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(with) == "" {
		return "theme and with are required", true, nil
	}
	payload, _ := json.Marshal(map[string]string{"with": with})
	if err := h.mem.SetThemeOverride(ref, "merge", string(payload)); err != nil {
		return err.Error(), true, nil
	}
	return `{"status":"recorded","directive":"merge","note":"merge application ships in a follow-up; override stored for next pass"}`, false, nil
}
