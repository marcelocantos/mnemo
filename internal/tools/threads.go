// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/marcelocantos/mnemo/internal/iterm"
	"github.com/marcelocantos/mnemo/internal/store"
	"github.com/marcelocantos/mnemo/internal/threads"
)

// threadManager builds a threads.Manager from the live config, so a
// threads_root change via mnemo_config takes effect on the next call with no
// daemon restart (🎯T85.1, Integration §0.5). The model is a filesystem
// projection; no store access is needed.
func threadManager() (*threads.Manager, error) {
	home, err := store.EffectiveHome()
	if err != nil {
		return nil, err
	}
	cfg, err := store.LoadConfig()
	if err != nil {
		return nil, err
	}
	return &threads.Manager{Root: cfg.ResolvedThreadsRoot(), Home: home}, nil
}

func jsonResult(v any) (string, bool, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("encode result: %v", err), true, nil
	}
	return string(data), false, nil
}

func (h *callHandler) threadList(args map[string]any) (string, bool, error) {
	m, err := threadManager()
	if err != nil {
		return fmt.Sprintf("threads: %v", err), true, nil
	}
	ts, err := m.ListSorted()
	if err != nil {
		return fmt.Sprintf("list threads: %v", err), true, nil
	}
	views := threads.Views(ts, time.Now())
	return jsonResult(map[string]any{
		"root":    m.Root,
		"count":   len(views),
		"threads": views,
	})
}

func (h *callHandler) threadShow(args map[string]any) (string, bool, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true, nil
	}
	m, err := threadManager()
	if err != nil {
		return fmt.Sprintf("threads: %v", err), true, nil
	}
	if _, err := m.Get(name); err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}
	body, err := m.ReadContext(name)
	if err != nil {
		body = "" // a thread may legitimately lack a CLAUDE.md
	}
	files, err := m.Files(name)
	if err != nil {
		return fmt.Sprintf("list files: %v", err), true, nil
	}
	type fileView struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		IsDir   bool   `json:"is_dir"`
		ModTime string `json:"mod_time"`
	}
	fvs := make([]fileView, 0, len(files))
	for _, f := range files {
		fvs = append(fvs, fileView{
			Name:    f.Name,
			Size:    f.Size,
			IsDir:   f.IsDir,
			ModTime: f.ModTime.Format(time.RFC3339),
		})
	}
	return jsonResult(map[string]any{
		"name":             name,
		"path":             m.Root + "/" + name,
		"context_markdown": body,
		"files":            fvs,
	})
}

func (h *callHandler) threadNew(args map[string]any) (string, bool, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true, nil
	}
	m, err := threadManager()
	if err != nil {
		return fmt.Sprintf("threads: %v", err), true, nil
	}
	t, err := m.New(threads.NewArgs{Name: name})
	if err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}
	return jsonResult(t.View(time.Now()))
}

func (h *callHandler) threadArchive(args map[string]any) (string, bool, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true, nil
	}
	m, err := threadManager()
	if err != nil {
		return fmt.Sprintf("threads: %v", err), true, nil
	}
	if err := m.Archive(name); err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}
	return jsonResult(map[string]any{"archived": name})
}

func (h *callHandler) threadGo(args map[string]any) (string, bool, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true, nil
	}
	noResume, _ := args["no_resume"].(bool)
	m, err := threadManager()
	if err != nil {
		return fmt.Sprintf("threads: %v", err), true, nil
	}
	path, err := m.Resolve(name)
	if err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}
	res, err := iterm.Go(h.ctx, iterm.GoArgs{
		Path:     path,
		Name:     filepath.Base(path),
		NoResume: noResume,
	})
	if err != nil {
		return fmt.Sprintf("%v", err), true, nil
	}
	return jsonResult(res)
}
