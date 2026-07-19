// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/marcelocantos/mnemo/internal/store"
)

// inprocessHost serves an embedded JS (goja) plugin as a local HTTP
// server on 127.0.0.1 (🎯T102.6). The public /plugins/<name>/* proxy
// and facets treat it identically to connect/launch once BaseURL is set.
//
// Script contract (goja):
//
//	function handle(req) {
//	  // req: { method, path, query, headers, body }
//	  return { status: 200, headers: {...}, body: "..." };
//	}
//
// The script must implement at least /ready and /manifest so
// AttachConnect succeeds. No mnemo rebuild is required to add a script.
type inprocessHost struct {
	name   string
	ln     net.Listener
	srv    *http.Server
	base   string
	mu     sync.Mutex
	closed bool
}

// startInProcess loads scriptPath, starts a local listener, and returns
// an AttachResult after ready+manifest validation.
func startInProcess(ctx context.Context, m *Manager, inst *Instance) (*AttachResult, *inprocessHost, error) {
	scriptPath := store.ExpandPluginPath(inst.Entry.Script, m.home, inst.Home)
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read script %s: %w", scriptPath, err)
	}
	if !strings.HasSuffix(strings.ToLower(scriptPath), ".js") &&
		!strings.HasSuffix(strings.ToLower(scriptPath), ".mjs") {
		// Lua is the same host shape; only goja is wired today.
		return nil, nil, fmt.Errorf("in-process plugins must be JavaScript (.js); got %s", filepath.Ext(scriptPath))
	}

	vm := goja.New()
	// Provide a minimal console.log for scripts.
	console := vm.NewObject()
	_ = console.Set("log", func(call goja.FunctionCall) goja.Value {
		parts := make([]any, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.Export()
		}
		m.log.Info("plugin script", append([]any{"name", inst.Name}, parts...)...)
		return goja.Undefined()
	})
	_ = vm.Set("console", console)
	// Params as a plain object for the script.
	_ = vm.Set("params", inst.Entry.Params)
	_ = vm.Set("pluginName", inst.Name)
	_ = vm.Set("pluginHome", inst.Home)

	if _, err := vm.RunString(string(src)); err != nil {
		return nil, nil, fmt.Errorf("script eval: %w", err)
	}
	handleVal := vm.Get("handle")
	if handleVal == nil || goja.IsUndefined(handleVal) || goja.IsNull(handleVal) {
		return nil, nil, fmt.Errorf("script %s must define function handle(req)", scriptPath)
	}
	handleFn, ok := goja.AssertFunction(handleVal)
	if !ok {
		return nil, nil, fmt.Errorf("script handle is not a function")
	}

	var vmMu sync.Mutex // goja is not concurrent-safe
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		headers := map[string]string{}
		for k := range r.Header {
			headers[k] = r.Header.Get(k)
		}
		reqObj := map[string]any{
			"method":  r.Method,
			"path":    r.URL.Path,
			"query":   r.URL.RawQuery,
			"headers": headers,
			"body":    string(body),
		}
		vmMu.Lock()
		defer vmMu.Unlock()
		// Fresh object each call.
		arg := vm.ToValue(reqObj)
		res, err := handleFn(goja.Undefined(), arg)
		if err != nil {
			http.Error(w, "plugin script error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		exported := res.Export()
		status, hdrs, respBody := parseJSResponse(exported)
		for k, v := range hdrs {
			w.Header().Set(k, v)
		}
		if w.Header().Get("Content-Type") == "" && json.Valid([]byte(respBody)) {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	host := &inprocessHost{
		name: inst.Name,
		ln:   ln,
		srv:  srv,
		base: "http://" + ln.Addr().String(),
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			m.log.Warn("in-process plugin server stopped", "name", inst.Name, "err", err)
		}
	}()

	att, err := AttachConnect(ctx, m.client, host.base, inst.Name)
	if err != nil {
		host.Stop()
		return nil, nil, err
	}
	return att, host, nil
}

func parseJSResponse(v any) (status int, headers map[string]string, body string) {
	status = 200
	headers = map[string]string{}
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["status"].(int64); ok {
			status = int(s)
		} else if s, ok := x["status"].(int); ok {
			status = s
		} else if s, ok := x["status"].(float64); ok {
			status = int(s)
		}
		if h, ok := x["headers"].(map[string]any); ok {
			for k, vv := range h {
				headers[k] = fmt.Sprint(vv)
			}
		}
		if b, ok := x["body"].(string); ok {
			body = b
		} else if b, ok := x["body"]; ok && b != nil {
			raw, _ := json.Marshal(b)
			body = string(raw)
		}
	case string:
		body = x
	default:
		if v != nil {
			raw, _ := json.Marshal(v)
			body = string(raw)
		}
	}
	return status, headers, body
}

// Stop shuts down the local listener.
func (h *inprocessHost) Stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	_ = h.ln.Close()
}
